// Package taskmgr coordinates task execution, pending prompts, attachments, and ordered runtime history import.
//
// It sits between the HTTP adapter (internal/server) and task execution
// (internal/task/taskrun). Manager owns the registry of running tasks and
// repository checkouts; Lifecycle performs operations for one task.
//
// Two contexts coexist here. Methods accept a request-scoped ctx that is
// honored for synchronous work (state checks, logging). Background goroutines
// that must outlive any single request use serverCtx instead — it tracks the
// lifetime of the Manager itself.
//
// A task incarnation publishes one terminal result through Entry.Finish, and a
// purge is final for that incarnation: the runtime instance is gone, so a stop
// that publishes afterwards cannot replace the purged snapshot. Lifecycle
// operations that publish terminal results, meaning Stop, Revive, and the purge
// completion, hold operationMu so the result order follows the state order.
package taskmgr

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/taskslog/data/inputqueue"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/maruel/gomode/mcp"
)

type relayReader interface {
	Status(ctx context.Context, target runtime.ConnectionTarget) (bool, string, error)
	ReadTail(ctx context.Context, target runtime.ConnectionTarget, maxBytes int64) (agent.RelaySnapshot, int64, error)
	ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string
}

const maxDelegatedTasksPerParent = 10

type agentRelayReader struct{}

func (agentRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	if target.SSHHost == "" {
		return false, "", errors.New("agent connection target missing SSH host")
	}
	return agent.RelayStatus(ctx, target.SSHHost)
}

func (agentRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, maxBytes int64) (timeline agent.RelaySnapshot, size int64, err error) {
	if target.SSHHost == "" {
		return agent.RelaySnapshot{}, 0, errors.New("agent connection target missing SSH host")
	}
	return agent.ReadRelayTail(ctx, target.SSHHost, maxBytes)
}

func (agentRelayReader) ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string {
	if target.SSHHost == "" {
		return ""
	}
	return agent.ReadRelayLog(ctx, target.SSHHost, maxBytes)
}

// Config holds the dependencies Manager needs at construction.
type Config struct {
	// ServerCtx tracks the Manager's own lifetime. Used as the parent for
	// background goroutines that must survive individual requests.
	ServerCtx context.Context
	Log       *slog.Logger
	// LogStore is the task-log directory the Manager appends task logs to. It
	// must be non-nil.
	LogStore *taskhistory.Store
	// Runtimes validates runtime selection and dispatches task runtime operations.
	Runtimes             *runtime.Router
	Backends             agent.Backends
	HarnessEnv           map[string][]string
	RuntimeMetadata      runtime.Metadata
	RuntimeStartTimeout  time.Duration
	EnsureSessionTimeout time.Duration
	Provider             genai.Provider // nil-safe
	// Pricer resolves per-model token pricing for cost reporting. When nil,
	// a default quota-provider pricer is used.
	Pricer quotausage.ModelPricer
	// Rollup consumes task messages for the cross-task usage rollup. Required.
	Rollup    task.RollupSink
	Checkouts *repo.Registry
}

// TaskMCPScoper derives a server-authorized MCP registry for a task.
//
// The task manager supplies only its trusted task ID. The server owns the
// authorization policy that turns that ID into an MCP capability.
type TaskMCPScoper interface {
	ForTask(id ksid.ID) mcp.Registry
}

const maxConcurrentTaskImports = 2

type instanceImportJob struct {
	checkout         *repo.Checkout
	instance         *runtime.Instance
	branch           string
	taskID           string
	metadataResolved bool
}

// PendingInput is an immutable pending prompt and its delivery state.
//
// Callers must not modify its fields or Prompt.Images. State changes publish
// a new value so a retained pointer remains safe to read.
type PendingInput struct {
	ID         ksid.ID
	Prompt     agent.Prompt
	Delivering bool
	Error      string
	Delivered  bool
}

// Manager owns task lifecycle state, durable prompt queues, runtime import, session watching, and
// stats streaming.
type Manager struct {
	// Immutable.
	agent.Backends

	Runtimes     *runtime.Router
	QuotaTracker *quotausage.Tracker
	Checkouts    *repo.Registry

	log                  *slog.Logger
	serverCtx            context.Context // lifetime of the Manager; for goroutines that outlive requests
	cancelServerCtx      context.CancelFunc
	logStore             *taskhistory.Store
	harnessEnv           map[string][]string
	runtimeMetadata      runtime.Metadata
	runtimeStartTimeout  time.Duration
	ensureSessionTimeout time.Duration
	provider             genai.Provider
	pricer               quotausage.ModelPricer
	rollup               task.RollupSink
	taskMCPScoper        TaskMCPScoper
	mcpExecutor          *agent.MCPExecutor
	relay                relayReader

	// Guarded by eventMu.
	eventMu              sync.Mutex
	importing            bool
	importDone           chan struct{} // creation waits for startup branch ownership
	pendingRuntimeEvents []runtime.Event

	// Guarded by quotaWatchMu.
	quotaWatchMu     sync.Mutex
	quotaWatchers    sync.WaitGroup
	quotaWatchClosed bool

	// Serializes pending-input persistence and registration.
	inputMu          sync.Mutex
	unrestoredInputs map[ksid.ID]queuedPrompt

	background sync.WaitGroup

	// Preparation locks are bounded by registered repository names. A creator
	// holds its sorted repository set through publication; disjoint sets never
	// wait for each other's Git fetches. These locks also protect adoption and
	// shared sequence-number allocation across the participating checkouts.
	preparationMu    sync.Mutex
	preparationLocks map[string]*sync.Mutex

	// Guarded by mu.
	mu            sync.Mutex
	tasks         map[ksid.ID]*Entry
	changed       chan struct{} // closed on mutation, replaced under mu
	changeVersion uint64        // incremented with changed under mu

	// Guarded by settledMu. Tracks background runtime/history restoration so the
	// task-list stream can report it (loading -> completed | failed). The zero
	// value means no pass has completed, so a fresh Manager reports loading
	// until CompleteRestoration runs.
	settledMu        sync.Mutex
	settledCompleted bool
	settledError     string
}

// New creates a Manager. Register each repo checkout in Checkouts, then Start.
func New(cfg Config) (*Manager, error) { //nolint:gocritic // Config is a value bag passed once at construction
	if cfg.ServerCtx == nil {
		return nil, errors.New("task manager server context is required")
	}
	if cfg.LogStore == nil {
		return nil, errors.New("task manager task log store is required")
	}
	if cfg.Runtimes == nil {
		return nil, errors.New("task manager runtime router is required")
	}
	if cfg.Checkouts == nil {
		return nil, errors.New("task manager checkout registry is required")
	}
	if cfg.Rollup == nil {
		return nil, errors.New("task manager usage rollup sink is required")
	}
	if cfg.RuntimeStartTimeout <= 0 {
		return nil, errors.New("task manager runtime start timeout is required")
	}
	if cfg.Log == nil {
		return nil, errors.New("task manager logger is required")
	}
	serverCtx, cancelServerCtx := context.WithCancel(cfg.ServerCtx)
	// Without an injected pricer, fall back to static quota-provider pricing.
	pricer := cfg.Pricer
	if pricer == nil {
		pricer = quotausage.NewPricer(nil)
	}
	m := &Manager{
		Runtimes:             cfg.Runtimes,
		QuotaTracker:         quotausage.NewTracker(),
		log:                  cfg.Log.With("cmp", "taskmgr"),
		serverCtx:            serverCtx,
		mcpExecutor:          agent.NewMCPExecutor(serverCtx),
		cancelServerCtx:      cancelServerCtx,
		Backends:             maps.Clone(cfg.Backends),
		logStore:             cfg.LogStore,
		harnessEnv:           cfg.HarnessEnv,
		runtimeMetadata:      maps.Clone(cfg.RuntimeMetadata),
		runtimeStartTimeout:  cfg.RuntimeStartTimeout,
		ensureSessionTimeout: cfg.EnsureSessionTimeout,
		provider:             cfg.Provider,
		pricer:               pricer,
		rollup:               cfg.Rollup,
		Checkouts:            cfg.Checkouts,
		relay:                agentRelayReader{},
		tasks:                make(map[ksid.ID]*Entry),
		preparationLocks:     make(map[string]*sync.Mutex),
		changed:              make(chan struct{}),
	}
	inputs, err := m.logStore.Journal.LoadInputQueue()
	if err != nil {
		cancelServerCtx()
		return nil, err
	}
	m.unrestoredInputs = make(map[ksid.ID]queuedPrompt, len(inputs))
	for _, entry := range inputs {
		id, err := ksid.Parse(entry.ID)
		if err != nil {
			cancelServerCtx()
			return nil, fmt.Errorf("queued prompt id: %w", err)
		}
		taskID, err := ksid.Parse(entry.TaskID)
		if err != nil {
			cancelServerCtx()
			return nil, fmt.Errorf("queued task id: %w", err)
		}
		q := queuedPrompt{ID: id, TaskID: taskID, Prompt: agent.Prompt{Text: entry.Text}}
		for _, img := range entry.Images {
			q.Prompt.Images = append(q.Prompt.Images, v3.ImageData{MediaType: img.MediaType, Data: img.Data})
		}
		if _, exists := m.unrestoredInputs[q.TaskID]; exists {
			cancelServerCtx()
			return nil, fmt.Errorf("task %s has multiple pending prompts", q.TaskID)
		}
		m.unrestoredInputs[q.TaskID] = q
	}
	return m, nil
}

// Cancel stops admission and signals local producers without closing their journals.
// App calls Cancel before joining automation; Close then joins all producers and
// seals the accepted task tails. Repeated cancellation is safe.
func (m *Manager) Cancel() {
	m.quotaWatchMu.Lock()
	m.quotaWatchClosed = true
	m.cancelServerCtx()
	m.quotaWatchMu.Unlock()
	m.Range(func(_ ksid.ID, e *Entry) bool {
		if e.Lifecycle != nil {
			e.Lifecycle.Cancel()
		}
		return true
	})
}

// Close stops background work and waits for task lifecycles and all Manager watchers to finish.
func (m *Manager) Close() error {
	m.Cancel()
	// Stop shared producers before sealing per-task journals. Accepted MCP
	// work and queued deliveries may still submit records while draining.
	m.background.Wait()
	m.quotaWatchers.Wait()
	m.mcpExecutor.Close()
	var closeErr error
	m.Range(func(_ ksid.ID, e *Entry) bool {
		if e.Lifecycle != nil {
			closeErr = errors.Join(closeErr, e.Lifecycle.Close())
		}
		return true
	})
	// Flush usage only after every task owner has applied its accepted tail.
	return errors.Join(closeErr, m.rollup.Close())
}

// Start activates m with its task-scoped MCP authority and starts its runtime
// event and stats watchers. It subscribes to events before startup inventory is
// listed, and ImportInstances applies any buffered events after registration.
func (m *Manager) Start(scoper TaskMCPScoper) error {
	if scoper == nil {
		return errors.New("task MCP scoper is required")
	}
	m.taskMCPScoper = scoper
	m.eventMu.Lock()
	m.importing = true
	m.importDone = make(chan struct{})
	m.eventMu.Unlock()
	events, err := m.Runtimes.WatchEvents(m.serverCtx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
	m.background.Go(func() { m.watchRuntimeEvents(m.serverCtx, events) })
	m.background.Go(func() { m.watchStats(m.serverCtx) })
	m.background.Go(func() { m.watchInputQueue(m.serverCtx) })
	m.background.Go(func() { m.watchDiskUsage(m.serverCtx) })
	if err != nil {
		return fmt.Errorf("watch runtime events: %w", err)
	}
	return nil
}

// NewEntry creates an unregistered entry with its immutable lifecycle.
func (m *Manager) NewEntry(t *taskrun.Task, lt *taskhistory.LoadedTask) *Entry {
	var taskMCP mcp.Registry
	if t.CaicMCP {
		taskMCP = m.mcpExecutor.Wrap(m.taskMCPScoper.ForTask(t.ID))
	}
	e := &Entry{
		task:            t,
		loadedTask:      lt,
		historyInMemory: lt == nil,
		historyGate:     make(chan struct{}, 1),
	}
	e.term.Store(&entryTerminal{done: make(chan struct{})})
	if lt != nil {
		e.LogPath.Set(lt.LogPath())
	}
	e.Lifecycle = &Lifecycle{
		manager: m,
		entry:   e,
		ctx:     context.WithoutCancel(m.serverCtx),
		agentRuntime: taskrun.AgentRuntime{
			Backends:             m.Backends,
			LogStore:             m.logStore,
			LogPath:              &e.LogPath,
			Runtimes:             m.Runtimes,
			Log:                  m.log.With("task", t.ID),
			NotifyTaskChange:     m.NotifyTaskChange,
			Checkout:             m.resolveCheckout(t),
			RuntimeMetadata:      m.runtimeMetadata,
			RuntimeStartTimeout:  m.runtimeStartTimeout,
			EnsureSessionTimeout: m.ensureSessionTimeout,
			MCPRegistry:          taskMCP,
		},
	}
	return e
}

// Insert registers a pre-built entry. Production task creation goes through
// Create/Fork; Insert is retained for tests (in internal/tasks and
// internal/server) that seed the registry without a real checkout.
func (m *Manager) Insert(id ksid.ID, entry *Entry) {
	m.insertEntry(id, entry)
}

// Range iterates over every registered entry. It snapshots the registry under
// m.mu and invokes fn unlocked, so fn may safely call back into the Manager
// (e.g. Checkout). The entry set is a point-in-time snapshot; Entry pointers are
// stable and carry their own locking. Stops iteration if fn returns false.
func (m *Manager) Range(fn func(id ksid.ID, e *Entry) bool) {
	m.mu.Lock()
	type kv struct {
		id ksid.ID
		e  *Entry
	}
	snap := make([]kv, 0, len(m.tasks))
	for id, e := range m.tasks {
		snap = append(snap, kv{id, e})
	}
	m.mu.Unlock()
	for _, it := range snap {
		if !fn(it.id, it.e) {
			return
		}
	}
}

// Len returns the number of registered entries.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tasks)
}

// TaskMCPAvailable reports whether new tasks can receive the bounded MCP capability.
func (m *Manager) TaskMCPAvailable() bool {
	return m.taskMCPScoper != nil
}

// NotifyTaskChange signals that task data may have changed.
func (m *Manager) NotifyTaskChange() {
	m.mu.Lock()
	m.taskChangedLocked()
	m.mu.Unlock()
}

// Changed returns a channel that is closed on task mutation.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// ChangeSnapshot returns the current task-change version and its notification
// channel. Callers that re-arm a change subscription must retain the version:
// it lets them install the replacement channel before handling the update that
// woke them, preventing another mutation in that handling window from being
// missed.
func (m *Manager) ChangeSnapshot() (version uint64, changed <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changeVersion, m.changed
}

// CompleteRestoration records the outcome after runtime and history restoration.
// A non-nil error leaves the registered partial subset usable, but prevents a
// task-list snapshot from certifying absence. Callers include errors from each
// restoration stage; a successful subsequent pass clears the previous error.
func (m *Manager) CompleteRestoration(err error) {
	m.settledMu.Lock()
	m.settledCompleted = true
	if err != nil {
		m.settledError = err.Error()
	} else {
		m.settledError = ""
	}
	m.settledMu.Unlock()
	m.NotifyTaskChange()
}

// RestorationStatus reports the background task-restoration state atomically:
// loading is true until CompleteRestoration runs, and error is non-empty only
// after a failed pass. Both fields are read under one lock so a concurrent
// CompleteRestoration cannot interleave between two separate reads.
func (m *Manager) RestorationStatus() (loading bool, err string) {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	return !m.settledCompleted, m.settledError
}

// TaskListComplete reports whether task-list absence is authoritative. Runtime
// import must have released its startup fence and all restoration stages must
// have completed successfully. Read before assembling the task-list snapshot.
func (m *Manager) TaskListComplete() bool {
	m.eventMu.Lock()
	importing := m.importing
	m.eventMu.Unlock()
	loading, err := m.RestorationStatus()
	return !importing && !loading && err == ""
}

// RegisteredLogPaths returns the set of cleaned log paths currently owned by
// registered entries. Maintenance passes use it to exclude logs owned by
// entries so they yield to the per-task terminal compression path.
func (m *Manager) RegisteredLogPaths() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := make(map[string]struct{}, len(m.tasks))
	for _, e := range m.tasks {
		if p := e.LogPath.Get(); p != "" {
			paths[filepath.Clean(p)] = struct{}{}
		}
	}
	return paths
}

// Create initializes branch identity and history before publishing a task,
// then starts its runtime asynchronously. Preparation failures return without
// registering a task.
func (m *Manager) Create(ctx context.Context, p CreateParams) (ksid.ID, error) { //nolint:gocritic // CreateParams is a request-shaped value bag
	// Runtime restoration publishes entries only after their histories are
	// trusted. Wait before allocating branches to avoid adopting one still
	// owned by an importing task. Reads remain available during restoration.
	if err := m.WaitForRuntimeImport(ctx); err != nil {
		return 0, err
	}
	// Resolve primary checkout.
	if len(p.Repos) > 0 {
		_, ok := m.Checkouts.Checkout(p.Repos[0].Name)
		if !ok {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownRepository, Msg: "unknown repo: " + p.Repos[0].Name}
		}
	}

	// Validate that every extra repo has a registered checkout (branches are
	// allocated later, in one pass, by publishEntry).
	for _, rs := range p.Repos[min(1, len(p.Repos)):] {
		if _, ok := m.Checkouts.Checkout(rs.Name); !ok {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownRepository, Msg: "unknown extra repo: " + rs.Name}
		}
	}

	runtimeName, err := resolveRuntimeName(m.Runtimes, p.RuntimeName)
	if err != nil {
		return 0, err
	}

	backend, ok := m.Backends[p.Harness]
	if !ok {
		return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownHarness, Msg: "unknown harness: " + string(p.Harness)}
	}
	if p.CaicMCP && !m.TaskMCPAvailable() {
		return 0, badRequestf("task-scoped MCP is unavailable")
	}

	if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
		return 0, &Error{Kind: KindBadRequest, Code: CodeUnsupportedModel, Msg: "unsupported model for " + string(p.Harness) + ": " + p.Model}
	}

	if len(p.Prompt.Images) > 0 && !backend.SupportsImages() {
		return 0, badRequestf("%s does not support images", string(p.Harness))
	}

	// Build repository mounts. ContainerPath follows the fixed "~/src/<name>"
	// convention used by the instance provisioner. Use the basename unless
	// another registered repo shares it.
	mounts := make([]repo.Mount, len(p.Repos))
	for i, rs := range p.Repos {
		r, _ := m.Checkouts.Checkout(rs.Name)
		mounts[i] = repo.Mount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir, ContainerPath: m.containerPathForRepo(rs.Name)}
	}

	t, err := m.newTask(p.Prompt, p.Harness, p.Model, p.Effort, p.BaseImage, p.ContainerPlatform, "")
	if err != nil {
		return 0, badRequestf("%v", err)
	}
	t.SetSessionMetadata(agent.SessionMetadata{SessionID: p.ResumeSessionID})
	t.Repos = mounts
	t.RuntimeName = runtimeName
	t.MaxCPUs = p.MaxCPUs
	t.CacheMounts = slices.Clone(p.CacheMounts)
	t.Mounts = slices.Clone(p.Mounts)
	t.GitHubToken = p.GitHubToken
	t.Tailscale = p.Tailscale
	t.USB = p.USB
	t.Display = p.Display
	t.Sudo = p.Sudo
	t.OwnerID = p.OwnerID
	t.Provider = m.provider
	t.CaicMCP = p.CaicMCP
	t.ForgeIssue = p.ForgeIssue
	if p.ForgeOwner != "" {
		// Set forge owner/repo so ListPendingBotTasks can resolve the commenter.
		t.SetPR(p.ForgeOwner, p.ForgeRepo, 0)
	}
	entry := m.NewEntry(t, nil)
	if err := m.publishEntry(ctx, entry, 1, true); err != nil {
		return 0, err
	}
	entry.Lifecycle.generateTitle()

	// Run setup under the lifecycle context.
	entry.Lifecycle.wg.Go(func() {
		ghToken := p.ResolvedGitHubToken

		if err := entry.Lifecycle.Start(entry.Lifecycle.ctx, ghToken); err != nil {
			return
		}
	})
	return t.ID, nil
}

// WaitForRuntimeImport waits until startup restores live task ownership.
// It returns immediately before Start or after import completion. Callers
// remain cancellation-aware while creation or an unresolved lookup is fenced.
func (m *Manager) WaitForRuntimeImport(ctx context.Context) error {
	m.eventMu.Lock()
	ready := m.importDone
	m.eventMu.Unlock()
	if ready != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.serverCtx.Done():
			return m.serverCtx.Err()
		case <-ready:
		}
	}
	return nil
}

// GetEntry returns the entry for taskID.
func (m *Manager) GetEntry(taskID ksid.ID) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.tasks[taskID]
	return e, ok
}

// Entries returns a point-in-time sequence of registered task entries.
//
// The iteration order is unspecified. The sequence owns a shallow snapshot,
// so callers do not hold the manager lock while yielding entries.
func (m *Manager) Entries() iter.Seq2[ksid.ID, *Entry] {
	m.mu.Lock()
	tasks := maps.Clone(m.tasks)
	m.mu.Unlock()
	return maps.All(tasks)
}

// EffectiveBaseBranch returns the branch the task was forked from.
func (m *Manager) EffectiveBaseBranch(t *taskrun.Task) string {
	p := t.Primary()
	if p == nil {
		return ""
	}
	if p.BaseBranch != "" {
		return p.BaseBranch
	}
	if checkout, ok := m.Checkouts.Checkout(p.Name); ok {
		return checkout.BaseBranch
	}
	return ""
}

// SudoPassword returns the cached sudo password for a task, fetching it from
// the instance on first access.
//
// Returns "" when the task does not use sudo or has no instance yet. The
// fetched password is cached on the task so subsequent calls avoid the SSH
// round-trip. A lookup failure is logged and returns "".
func (m *Manager) SudoPassword(ctx context.Context, t *taskrun.Task) string {
	enabled, instanceID, cached := t.SudoLookupState()
	if !enabled || instanceID == "" {
		return ""
	}
	if cached != "" {
		return cached
	}
	pw, err := m.Runtimes.SudoPassword(ctx, instanceID)
	if err != nil {
		m.log.WarnContext(ctx, "sudo password lookup failed", "instance", instanceID, "err", err)
		return ""
	}
	t.SetSudoPassword(pw)
	return pw
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
// Equivalent to entry.SetMonitorBranch; kept for backend.Backend interface symmetry.
func (m *Manager) SetTaskMonitorBranch(entry *Entry, branch string) {
	entry.SetMonitorBranch(branch)
}

// ImportInstances registers preexisting runtime instances as tasks. Failures
// return an ImportError counting affected instances alongside successful entries.
func (m *Manager) ImportInstances(ctx context.Context, instances []runtime.Instance, allLogs []*taskhistory.LoadedTask) ([]*Entry, error) {
	defer m.completeRuntimeImport(ctx)
	if instances == nil {
		return nil, nil
	}
	resolvedTaskIDs, rejected, validationErr := m.resolveImportTaskIDs(ctx, instances)

	// Map repo+branch loaded from purged task logs to their ID.
	branchIDs := make(map[string][]ksid.ID)
	m.Range(func(id ksid.ID, e *Entry) bool {
		if p := e.Task().Primary(); p != nil && p.Branch != "" {
			key := p.Name + "\x00" + p.Branch
			branchIDs[key] = append(branchIDs[key], id)
		}
		return true
	})

	var errs []error
	if validationErr != nil {
		errs = append(errs, validationErr)
	}
	claimed := make(map[runtime.ID]bool, len(instances))
	for id := range rejected {
		claimed[id] = true
	}
	jobs := make([]instanceImportJob, 0, len(instances)-len(claimed))

	for checkout := range m.Checkouts.Checkouts() {
		for i := range instances {
			c := &instances[i]
			if claimed[c.ID] {
				continue
			}
			branch, matched := primaryBranchForImport(checkout, c)
			if !matched {
				continue
			}
			claimed[c.ID] = true
			taskIDVal, metadataResolved := resolvedTaskIDs[c.ID]
			jobs = append(jobs, instanceImportJob{checkout: checkout, instance: c, branch: branch, taskID: taskIDVal, metadataResolved: metadataResolved})
		}
	}

	// Import no-repo runtime instances.
	for i := range instances {
		c := &instances[i]
		if claimed[c.ID] || !strings.HasPrefix(string(c.ID.InstanceID()), "md-agent-") {
			continue
		}
		taskIDVal, metadataResolved := resolvedTaskIDs[c.ID]
		jobs = append(jobs, instanceImportJob{instance: c, taskID: taskIDVal, metadataResolved: metadataResolved})
	}

	jobsCh := make(chan instanceImportJob, len(jobs))
	for _, job := range jobs {
		jobsCh <- job
	}
	close(jobsCh)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var entries []*Entry
	failed := len(rejected)
	for range min(maxConcurrentTaskImports, len(jobs)) {
		wg.Go(func() {
			for job := range jobsCh {
				entry, err := m.importInstance(ctx, job.checkout, job.instance, job.branch, job.taskID, job.metadataResolved, branchIDs, allLogs)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
					failed++
				}
				if entry != nil {
					entries = append(entries, entry)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return entries, &ImportError{Failed: failed, Err: err}
	}
	return entries, nil
}

func primaryBranchForImport(checkout *repo.Checkout, c *runtime.Instance) (string, bool) {
	if len(c.Repos) == 0 {
		return "", false
	}
	r := c.Repos[0]
	if !containerPathMatchesRepo(r.ContainerPath, checkout) {
		return "", false
	}
	return r.Branch, true
}

func containerPathMatchesRepo(containerPath string, checkout *repo.Checkout) bool {
	relPath := filepath.ToSlash(checkout.RelPath)
	baseName := filepath.Base(checkout.Dir)
	return slices.Contains([]string{
		"/home/user/src/" + relPath,
		"~/src/" + relPath,
		relPath,
		"/home/user/src/" + baseName,
		"~/src/" + baseName,
		baseName,
	}, containerPath)
}

// FindTasksMonitoringBranch returns all entries that match the given forge owner/repo
// and have a monitor branch set.
func (m *Manager) FindTasksMonitoringBranch(owner, repoName string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		if e.MonitorBranch() == "" {
			continue
		}
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repoName {
			if p := e.task.Primary(); p != nil && p.Branch == e.MonitorBranch() {
				out = append(out, e)
			}
		}
	}
	return out
}

// FindTasksByPR returns all entries matching the given forge owner/repo and PR number.
func (m *Manager) FindTasksByPR(owner, repoName string, prNumber int) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repoName && snap.ForgePR == prNumber {
			out = append(out, e)
		}
	}
	return out
}

// FindTasksMatchingBranch returns all entries for a forge owner/repo where the
// primary repo branch matches the given branch.
func (m *Manager) FindTasksMatchingBranch(owner, repoName, branch string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repoName {
			if p := e.task.Primary(); p != nil && p.Branch == branch {
				out = append(out, e)
			}
		}
	}
	return out
}

// ListPendingBotTasks returns non-terminal tasks that have a ForgeIssue set.
func (m *Manager) ListPendingBotTasks() []BotPendingTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []BotPendingTask
	for id, entry := range m.tasks {
		snap := entry.task.Snapshot()
		if snap.ForgeIssue <= 0 {
			continue
		}
		st := snap.State
		if st == task.StateWaiting || st == task.StateStopped || st == task.StateCrashed || st == task.StateFailed || st == task.StatePurged {
			continue
		}
		out = append(out, BotPendingTask{
			TaskID:      id,
			ForgeOwner:  snap.ForgeOwner,
			ForgeRepo:   snap.ForgeRepo,
			IssueNumber: snap.ForgeIssue,
		})
	}
	return out
}

// WatchTaskCompletion blocks until the task reaches a terminal state.
func (m *Manager) WatchTaskCompletion(ctx context.Context, taskID ksid.ID) (state, result string, err error) {
	entry, ok := m.GetEntry(taskID)
	if !ok {
		return "", "", notFoundf("task %s not found", taskID)
	}
	for {
		st := entry.task.GetState()
		switch st {
		case task.StateWaiting, task.StateStopped, task.StateCrashed, task.StateFailed, task.StatePurged:
			return st.String(), lastResultText(entry.task), nil
		case task.StatePending, task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateAsking, task.StateHasPlan, task.StatePulling, task.StatePushing, task.StateStopping, task.StatePurging:
			// Not yet done; keep waiting below.
		}
		m.mu.Lock()
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
}

// lastResultText returns the Result field of the most recent ResultMessage.
func lastResultText(t *taskrun.Task) string {
	for msg := range t.BackwardMessages() {
		if rm, ok := msg.(*agent.ResultMessage); ok {
			return rm.Result
		}
	}
	return ""
}

// LoadUnsettledTasks loads plain (uncompressed) task logs into the registry
// without any per-repo cap. Plain logs are the working set: live tasks, terminal
// logs not yet compressed, and abandoned logs. The caller scans with the
// retention cutoff, so only recent logs arrive here.
func (m *Manager) LoadUnsettledTasks(plain []*taskhistory.LoadedTask) error {
	loaded, err := m.insertLoadedTasks(plain)
	if err != nil {
		return err
	}
	if loaded > 0 {
		m.NotifyTaskChange()
	}
	m.log.Info("loaded unsettled tasks from logs", "n", loaded, "candidates", len(plain))
	return nil
}

// LoadPurgedTasks registers settled task logs selected by the taskslog settled
// scan, which already caps entries per repo before a full decode.
func (m *Manager) LoadPurgedTasks(purged []*taskhistory.LoadedTask) error {
	if len(purged) == 0 {
		m.log.Info("no purged tasks to load")
		return nil
	}
	// Do not scan full logs here. Some compressed histories are multi-GB, and
	// historical session metadata must not block the task list becoming usable.
	loaded, err := m.insertLoadedTasks(purged)
	if err != nil {
		return err
	}
	if loaded > 0 {
		m.NotifyTaskChange()
	}
	m.log.Info("loaded purged tasks from logs", "n", loaded, "candidates", len(purged))
	return nil
}

// HistorySource opens a header-only raw-log reader for stopped or terminal
// history. It is the cold-history path: live readers should use the Task
// timeline rather than repeatedly reparsing its growing log.
func (m *Manager) HistorySource(entry *Entry) (*taskhistory.LoadedTask, error) {
	if entry == nil {
		return nil, errors.New("task history entry is nil")
	}
	path := entry.LogPath.Get()
	if path == "" {
		return nil, taskslog.ErrNoLog
	}
	loaded, err := m.logStore.LoadSource(path)
	if err != nil {
		return nil, fmt.Errorf("load task history source: %w", err)
	}
	entry.SetLoadedTask(loaded)
	return loaded, nil
}

// LastAgentMessage returns the newest text, question, or result message the
// task's agent produced outside a child source, or nil when it produced none.
//
// Live and adopted tasks read their in-memory timeline. Restored cold tasks
// decode their log once and retain the answer until the log changes, because a
// whole-log decode costs seconds on a large task.
func (m *Manager) LastAgentMessage(ctx context.Context, entry *Entry) (agent.Message, error) {
	if entry == nil {
		return nil, errors.New("task history entry is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.historyInMemory {
		return foldLastAgentMessage(ctx, inMemoryMessages(ctx, entry.Task()))
	}
	identity, err := historyIdentityOf(entry)
	if err != nil {
		return nil, err
	}
	if err := entry.acquireHistory(ctx); err != nil {
		return nil, err
	}
	defer entry.releaseHistory()
	memo := entry.coldHistoryLocked(identity)
	if memo.haveLast {
		return memo.last, nil
	}
	last, err := foldLastAgentMessage(ctx, m.coldMessages(ctx, entry))
	if err != nil {
		return nil, err
	}
	memo.last, memo.haveLast = last, true
	return last, nil
}

// ToolInput resolves the recorded input of one tool invocation. A zero query
// source accepts any owner but reports ErrToolUseAmbiguous instead of guessing,
// and a cold task retains a bounded number of answers so revisiting a tool does
// not decode its whole log again.
func (m *Manager) ToolInput(ctx context.Context, entry *Entry, query *ToolInputQuery) (*agent.ToolUseMessage, error) {
	if query == nil || query.ToolUseID == "" {
		return nil, errors.New("tool use ID is required")
	}
	if entry == nil {
		return nil, errors.New("task history entry is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lookup := agent.ToolKeyForSource(&query.Source, query.ToolUseID)
	if entry.historyInMemory {
		return foldToolInput(ctx, inMemoryMessages(ctx, entry.Task()), query)
	}
	identity, err := historyIdentityOf(entry)
	if err != nil {
		return nil, err
	}
	if err := entry.acquireHistory(ctx); err != nil {
		return nil, err
	}
	defer entry.releaseHistory()
	memo := entry.coldHistoryLocked(identity)
	if retained, ok := memo.outcome(&lookup); ok {
		return retained.input, retained.err
	}
	input, err := foldToolInput(ctx, m.coldMessages(ctx, entry), query)
	if errors.Is(err, ErrToolUseAmbiguous) || errors.Is(err, ErrToolUseNotFound) {
		// The verdict of a complete scan cannot change while the log does not.
		memo.retainOutcome(&lookup, nil, err)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	memo.retainOutcome(&lookup, input, nil)
	return input, nil
}

// BranchAssociated reports whether a nonterminal task has claimed branch for repoName.
func (m *Manager) BranchAssociated(repoName, branch string) bool {
	for _, entry := range m.Entries() {
		candidate := entry.Task()
		if candidate.GetState().IsTerminal() {
			continue
		}
		for _, mounted := range candidate.ReposSnapshot() {
			if mounted.Name == repoName && mounted.Branch == branch {
				return true
			}
		}
	}
	return false
}

// coldMessages decodes the task's physical log in recorded order. The consumer
// must range to EOF so that interpretation failures reach it.
func (m *Manager) coldMessages(ctx context.Context, entry *Entry) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		source, err := m.HistorySource(entry)
		if err != nil {
			yield(nil, err)
			return
		}
		for parsed, streamErr := range source.StreamMessages(ctx) {
			if !yield(parsed.Message, streamErr) {
				return
			}
			if streamErr != nil {
				return
			}
		}
	}
}

// updatePendingInputsLocked persists changes before publishing them on entries.
// The caller holds inputMu; disk I/O never holds an Entry's mu.
func (m *Manager) updatePendingInputsLocked(updates map[*Entry]*PendingInput) error {
	entries := make([]queuedPrompt, 0, len(m.unrestoredInputs))
	for _, q := range m.unrestoredInputs {
		entries = append(entries, q)
	}
	for id, e := range m.Entries() {
		q, changed := updates[e]
		if !changed {
			q = e.PendingInput()
		}
		if q != nil {
			entries = append(entries, queuedPrompt{ID: q.ID, TaskID: id, Prompt: q.Prompt})
		}
	}
	encoded := make([]inputqueue.Entry, 0, len(entries))
	for _, q := range entries {
		entry := inputqueue.Entry{ID: q.ID.String(), TaskID: q.TaskID.String(), Text: q.Prompt.Text}
		for _, img := range q.Prompt.Images {
			entry.Images = append(entry.Images, inputqueue.Image{MediaType: img.MediaType, Data: img.Data})
		}
		encoded = append(encoded, entry)
	}
	if err := m.logStore.Journal.SaveInputQueue(encoded); err != nil {
		return err
	}
	for e, q := range updates {
		e.setPendingInput(q)
	}
	return nil
}

// newTask creates a task and attaches the manager's model pricer.
func (m *Manager) newTask(prompt agent.Prompt, h harness.Name, model, effort, baseImage, containerPlatform, title string) (*taskrun.Task, error) {
	t, err := taskrun.NewTask(ksid.NewID(), prompt, h, model, effort, baseImage, containerPlatform, title)
	if err != nil {
		return nil, err
	}
	t.Pricer = m.pricer
	t.Rollup = m.rollup
	return t, nil
}

// insertLoadedTasks inserts loaded task logs into the registry, skipping tasks
// whose id or repo/branch already exists. It is shared by LoadUnsettledTasks
// (plain logs, uncapped) and LoadPurgedTasks (settled logs, pre-capped by the
// scan).
func (m *Manager) insertLoadedTasks(lts []*taskhistory.LoadedTask) (int, error) {
	m.inputMu.Lock()
	defer m.inputMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	existingBranches := make(map[string]struct{})
	for _, e := range m.tasks {
		if p := e.task.Primary(); p != nil && p.Branch != "" {
			existingBranches[p.Name+"\x00"+p.Branch] = struct{}{}
		}
	}
	loaded := 0
	for _, lt := range lts {
		if lt.LastTrailer == nil {
			lt.LastTrailer = &task.Result{State: task.StateFailed}
		}
		taskID := ksid.NewID()
		parsedID := false
		if len(lt.TaskID) >= 9 {
			if parsed, parseErr := ksid.Parse(lt.TaskID); parseErr == nil && parsed != 0 {
				taskID = parsed
				parsedID = true
			}
		}
		if parsedID {
			if _, exists := m.tasks[taskID]; exists {
				continue
			}
		}
		if p := lt.Primary(); p != nil && p.Branch != "" {
			if _, exists := existingBranches[p.Name+"\x00"+p.Branch]; exists {
				continue
			}
		}

		rt := m.Runtimes.Runtimes[0].Name()
		if lt.RuntimeName != "" {
			rt = lt.RuntimeName
		} else {
			lt.RuntimeName = rt
		}
		var forkedFromTaskID ksid.ID
		if lt.ForkedFromTaskID != "" {
			parsed, err := ksid.Parse(lt.ForkedFromTaskID)
			if err != nil {
				m.log.Warn("skipping task with invalid forkedFromTaskID", "task", lt.TaskID, "forkedFromTaskID", lt.ForkedFromTaskID, "err", err)
				continue
			}
			forkedFromTaskID = parsed
		}
		var parentTaskID ksid.ID
		if lt.ParentTaskID != "" {
			parsed, err := ksid.Parse(lt.ParentTaskID)
			if err != nil {
				m.log.Warn("skipping task with invalid parentTaskID", "task", lt.TaskID, "parentTaskID", lt.ParentTaskID, "err", err)
				continue
			}
			parentTaskID = parsed
		}
		t, err := taskrun.NewTask(taskID, agent.Prompt{Text: lt.Prompt}, lt.Harness, lt.RequestedModel, lt.RequestedEffort, lt.BaseImage, lt.ContainerPlatform, lt.Title)
		if err != nil {
			m.log.Warn("skipping purged task with invalid metadata", "task", lt.TaskID, "err", err)
			continue
		}
		t.Pricer = m.pricer
		t.Rollup = m.rollup
		t.RuntimeName = rt
		t.Repos = lt.Repos
		t.MaxCPUs = lt.MaxCPUs
		t.CacheMounts = slices.Clone(lt.CacheMounts)
		t.Mounts = slices.Clone(lt.Mounts)
		t.StartedAt = lt.StartedAt
		t.OwnerID = lt.OwnerID
		t.ForkedFromTaskID = forkedFromTaskID
		t.ParentTaskID = parentTaskID
		t.CaicMCP = lt.CaicMCP
		t.Tailscale = lt.Tailscale
		t.USB = lt.USB
		t.Display = lt.Display
		t.Sudo = lt.Sudo
		t.GitHubToken = lt.GitHubToken
		t.SetStateAt(lt.State, lt.LastStateUpdateAt)
		t.SetSessionMetadata(lt.SessionMetadata)
		if lt.State == task.StateRunning {
			// Nothing adopted the instance that was running this log, so the task can
			// never settle. Name the downgrade: a bare failure with no error gives the
			// task list no way to explain what happened.
			m.log.Warn("loaded running task log without an adopted instance; marking failed",
				"task", lt.TaskID, "harness", lt.Harness, "log", lt.LogPath(), "state", lt.State)
			t.SetState(task.StateFailed)
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
		entry := m.NewEntry(t, lt)
		entry.Finish(lt.LastTrailer)
		m.replaceEntryLocked(t.ID, entry)
		loaded++
	}
	return loaded, nil
}

// resolveImportTaskIDs selects instances for configured checkouts, reads
// their task IDs, and rejects candidates with unavailable or duplicate IDs.
func (m *Manager) resolveImportTaskIDs(ctx context.Context, instances []runtime.Instance) (resolved map[runtime.ID]string, rejected map[runtime.ID]bool, retErr error) {
	candidates := make(map[runtime.ID]bool, len(instances))
	for checkout := range m.Checkouts.Checkouts() {
		for j := range instances {
			if _, matched := primaryBranchForImport(checkout, &instances[j]); matched {
				candidates[instances[j].ID] = true
			}
		}
	}
	for i := range instances {
		if strings.HasPrefix(string(instances[i].ID.InstanceID()), "md-agent-") {
			candidates[instances[i].ID] = true
		}
	}

	resolved = make(map[runtime.ID]string, len(candidates))
	rejected = make(map[runtime.ID]bool)
	seen := make(map[string]runtime.ID, len(candidates))
	var errs []error
	for i := range instances {
		instance := &instances[i]
		if !candidates[instance.ID] {
			continue
		}
		taskID, err := m.runtimeTaskID(ctx, instance.ID)
		if err != nil {
			rejected[instance.ID] = true
			errs = append(errs, fmt.Errorf("metadata check for %s: %w", instance.ID, err))
			continue
		}
		resolved[instance.ID] = taskID
		if taskID == "" {
			continue
		}
		if previous, ok := seen[taskID]; ok {
			rejected[previous] = true
			rejected[instance.ID] = true
			errs = append(errs, fmt.Errorf("duplicate runtime task ID %q on instances %s and %s", taskID, previous, instance.ID))
			continue
		}
		seen[taskID] = instance.ID
	}
	return resolved, rejected, errors.Join(errs...)
}

func (m *Manager) containerPathForRepo(relPath string) string {
	base := filepath.Base(relPath)
	if !m.repoBasenameCollides(relPath) {
		return "~/src/" + base
	}
	return "~/src/" + relPath
}

func (m *Manager) repoBasenameCollides(relPath string) bool {
	base := filepath.Base(relPath)
	collides := false
	for checkout := range m.Checkouts.Checkouts() {
		if checkout.RelPath != "" && checkout.RelPath != relPath && filepath.Base(checkout.RelPath) == base {
			collides = true
			break
		}
	}
	return collides
}

// lockPreparation locks a sorted, deduplicated repository set. The small map
// mutex is released before acquiring any preparation lock or running Git.
func (m *Manager) lockPreparation(mounts []repo.Mount) (func(), error) {
	names := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if _, ok := m.Checkouts.Checkout(mount.Name); !ok {
			return nil, fmt.Errorf("repo %q is not registered", mount.Name)
		}
		names = append(names, mount.Name)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	locks := make([]*sync.Mutex, 0, len(names))
	m.preparationMu.Lock()
	for _, name := range names {
		lock := m.preparationLocks[name]
		if lock == nil {
			lock = &sync.Mutex{}
			m.preparationLocks[name] = lock
		}
		locks = append(locks, lock)
	}
	m.preparationMu.Unlock()
	for _, lock := range locks {
		lock.Lock()
	}
	return func() {
		for _, lock := range slices.Backward(locks) {
			lock.Unlock()
		}
	}, nil
}

// publishEntry prepares final branch identities and a closed readable history
// before registration. Preparation owns only branches it actually creates and
// its exclusive new log; any failure rolls those resources back. Reserved or
// adopted branches are never deleted. Repository locks span publication so a
// branch cannot be adopted twice, while the delegated cap is checked atomically
// with insertion across disjoint repository sets.
func (m *Manager) publishEntry(ctx context.Context, entry *Entry, reserveOnly int, adoptLocal bool) (err error) {
	t := entry.Task()
	mounts := t.ReposSnapshot()
	unlock, err := m.lockPreparation(mounts)
	if err != nil {
		return internalErr(err, "lock task repositories")
	}
	defer unlock()
	branches, allocationErr := m.allocateBranchesLocked(ctx, t, mounts, reserveOnly, adoptLocal)
	var log *taskslog.PreparedLog
	defer func() {
		if err == nil {
			return
		}
		var cleanupErr error
		if log != nil {
			cleanupErr = log.Rollback()
		}
		for _, branch := range slices.Backward(branches) {
			cleanupErr = errors.Join(cleanupErr, branch.Rollback(ctx, m.log))
		}
		if cleanupErr != nil {
			err = errors.Join(err, internalErr(cleanupErr, "rollback task preparation"))
		}
	}()
	if allocationErr != nil {
		return internalErr(allocationErr, "allocate branch")
	}
	log, err = m.logStore.Initialize(t.LogFilename(), t.LogHeader())
	if err != nil {
		return internalErr(err, "initialize task log")
	}
	entry.LogPath.Set(log.Path)
	m.mu.Lock()
	if err := m.serverCtx.Err(); err != nil {
		m.mu.Unlock()
		return err
	}
	if t.ParentTaskID != 0 {
		// TODO: Replace this temporary cap with configurable, time-based delegation quotas.
		children := 0
		for _, candidate := range m.tasks {
			child := candidate.Task()
			if child.ParentTaskID == t.ParentTaskID && child.GetState() != task.StatePurged {
				children++
			}
		}
		if children >= maxDelegatedTasksPerParent {
			m.mu.Unlock()
			return conflict(fmt.Sprintf("delegating task already has %d non-purged child tasks", maxDelegatedTasksPerParent))
		}
	}
	m.tasks[t.ID] = entry
	m.taskChangedLocked()
	m.mu.Unlock()
	m.watchRateLimitEvents(t)
	return nil
}

// allocateBranchesLocked assigns every repo its final branch. Fresh tasks
// adopt an explicitly selected local branch when available; forks always get
// new branches. mounts[:reserveOnly] need only a reservation: runtime setup
// creates those branches. The remaining branches are created in their checkout.
// Fresh branches share the highest next sequence number across the checkouts,
// falling back to a new number when the shared name already exists in Git.
//
// Requires all participating repository preparation locks through publication:
// the immutable log header and filename embed these branch names, and a newly
// adopted local branch must become associated before another creator checks it.
func (m *Manager) allocateBranchesLocked(ctx context.Context, t *taskrun.Task, mounts []repo.Mount, reserveOnly int, adoptLocal bool) ([]*repo.AllocatedBranch, error) {
	var created []*repo.AllocatedBranch
	// Resolve adoption first; adopted repos keep their own branch name.
	var shared []int
	var checkouts []*repo.Checkout
	for i := range mounts {
		ws, ok := m.Checkouts.Checkout(mounts[i].Name)
		if !ok {
			return created, fmt.Errorf("repo %q is not registered", mounts[i].Name)
		}
		if adoptLocal && mounts[i].BaseBranch != "" && !m.BranchAssociated(mounts[i].Name, mounts[i].BaseBranch) {
			branches, err := ws.AdoptableBranches(ctx, m.log)
			if err != nil {
				return created, fmt.Errorf("inspect branch for %s: %w", mounts[i].Name, err)
			}
			if slices.Contains(branches, mounts[i].BaseBranch) {
				t.SetRepoBranch(i, mounts[i].BaseBranch)
				continue
			}
		}
		shared = append(shared, i)
		checkouts = append(checkouts, ws)
	}
	if len(shared) == 0 {
		return created, nil
	}

	// One branch name for every repo that receives a fresh caic-N branch: the
	// highest next sequence number across their checkouts, so it is free in
	// each. The repository locks make the peek-and-reserve below atomic with
	// respect to other tasks' allocations.
	n := -1
	for _, ws := range checkouts {
		if next := ws.NextBranchSeq(); next > n {
			n = next
		}
	}
	preferred := fmt.Sprintf("caic-%d", n)
	for k, i := range shared {
		if i >= reserveOnly {
			continue // AllocateBranch advances the counter itself.
		}
		checkouts[k].ReserveBranchNumber(n)
	}
	for k, i := range shared {
		if i < reserveOnly {
			t.SetRepoBranch(i, preferred)
			continue
		}
		branch, err := checkouts[k].AllocateBranch(ctx, m.log, mounts[i].BaseBranch, preferred)
		if err != nil {
			return created, fmt.Errorf("allocate branch for %s: %w", mounts[i].Name, err)
		}
		created = append(created, branch)
		t.SetRepoBranch(i, branch.Name)
	}
	return created, nil
}

// watchStats streams runtime resource stats for the current active task set.
func (m *Manager) watchStats(ctx context.Context) {
	const retryDelay = 5 * time.Second
	for {
		ids, changed := m.activeStatsIDs()
		if len(ids) == 0 {
			if !waitStatsChange(ctx, changed) {
				return
			}
			continue
		}

		streamCtx, cancel := context.WithCancel(ctx)
		var changedWatch sync.WaitGroup
		changedWatch.Go(func() {
			// Task data changes frequently without changing the sampled containers.
			// Preserve the stream's CPU measurement interval across those updates.
			ch := changed
			for {
				select {
				case <-ch:
					current, next := m.activeStatsIDs()
					if !slices.Equal(ids, current) {
						cancel()
						return
					}
					ch = next
				case <-streamCtx.Done():
					return
				}
			}
		})

		stats, err := m.Runtimes.WatchStats(streamCtx, ids)
		if err != nil {
			cancel()
			changedWatch.Wait()
			m.log.WarnContext(ctx, "stats stream failed", "err", err)
			if !m.waitStatsRetry(ctx, ids, retryDelay) {
				return
			}
			continue
		}
		for sample, err := range stats {
			if err != nil {
				if streamCtx.Err() == nil {
					m.log.WarnContext(ctx, "stats stream failed", "err", err)
				}
				break
			}
			m.pushStatsSample(&sample)
		}
		// Capture interruption before cleanup cancels the context. Otherwise an
		// unexpected stream end would look interrupted and skip the retry delay.
		interrupted := streamCtx.Err() != nil
		cancel()
		changedWatch.Wait()
		if ctx.Err() != nil {
			return
		}
		if !interrupted && !m.waitStatsRetry(ctx, ids, retryDelay) {
			return
		}
	}
}

func (m *Manager) activeStatsIDs() (ids []runtime.ID, changed <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids = make([]runtime.ID, 0, len(m.tasks))
	for _, e := range m.tasks {
		name := e.task.RuntimeInstanceID()
		if name == "" || !statsStateActive(e.task.GetState()) {
			continue
		}
		ids = append(ids, name)
	}
	slices.Sort(ids)
	return ids, m.changed
}

const diskUsageInterval = 5 * time.Second

// watchDiskUsage periodically samples all active writable layers in one batch.
// A final sample is taken after an instance stops, then the timer remains
// disabled until another task becomes active.
func (m *Manager) watchDiskUsage(ctx context.Context) {
	var tracked []runtime.ID
	var timer *time.Timer
	var tick <-chan time.Time
	for {
		active, changed := m.activeDiskUsageIDs()
		if !slices.Equal(active, tracked) {
			m.updateDiskUsage(ctx, unionRuntimeIDs(active, tracked))
			tracked = slices.Clone(active)
			if timer == nil && len(active) > 0 {
				timer = time.NewTimer(diskUsageInterval)
				tick = timer.C
			} else if timer != nil {
				timer.Stop()
				if len(active) > 0 {
					timer.Reset(diskUsageInterval)
					tick = timer.C
				} else {
					timer = nil
					tick = nil
				}
			}
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-changed:
		case <-tick:
			m.updateDiskUsage(ctx, active)
			timer.Reset(diskUsageInterval)
		}
	}
}

func (m *Manager) activeDiskUsageIDs() (ids []runtime.ID, changed <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids = make([]runtime.ID, 0, len(m.tasks))
	for _, e := range m.tasks {
		id := e.task.RuntimeInstanceID()
		if id == "" || !diskUsageStateActive(e.task.GetState()) {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, m.changed
}

func (m *Manager) updateDiskUsage(ctx context.Context, ids []runtime.ID) {
	if len(ids) == 0 {
		return
	}
	usage, err := m.Runtimes.DiskUsage(ctx, ids)
	if err != nil {
		m.log.WarnContext(ctx, "disk usage poll failed", "err", err)
		return
	}
	m.mu.Lock()
	tasks := make(map[runtime.ID]*taskrun.Task, len(usage))
	for _, e := range m.tasks {
		if _, ok := usage[e.task.RuntimeInstanceID()]; ok {
			tasks[e.task.RuntimeInstanceID()] = e.task
		}
	}
	m.mu.Unlock()
	for id, size := range usage {
		if target := tasks[id]; target != nil {
			target.UpdateDiskUsage(size)
		}
	}
}

func unionRuntimeIDs(a, b []runtime.ID) []runtime.ID {
	ids := make([]runtime.ID, 0, len(a)+len(b))
	for _, candidates := range [][]runtime.ID{a, b} {
		for _, id := range candidates {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

func resolveRuntimeName(router *runtime.Router, id runtime.Name) (runtime.Name, error) {
	if id == "" {
		return router.Runtimes[0].Name(), nil
	}
	if _, ok := router.ByName[id]; !ok {
		return "", &Error{Kind: KindBadRequest, Code: CodeUnknownRuntime, Msg: "unknown runtime: " + string(id)}
	}
	return id, nil
}

func waitStatsChange(ctx context.Context, changed <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return true
	}
}

// waitStatsRetry backs off after a failed or unexpectedly ended stats stream,
// avoiding a tight runtime-command restart loop and repeated failure logs.
// Container-set changes bypass the delay so sampling follows the new set;
// ordinary task updates preserve the delay. It returns false on shutdown.
func (m *Manager) waitStatsRetry(ctx context.Context, ids []runtime.ID, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		current, changed := m.activeStatsIDs()
		if !slices.Equal(ids, current) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-timer.C:
			return true
		}
	}
}

func (m *Manager) pushStatsSample(sample *runtime.StatsSample) {
	m.mu.Lock()
	var target *taskrun.Task
	for _, e := range m.tasks {
		if e.task.RuntimeInstanceID() != sample.InstanceID {
			continue
		}
		target = e.task
		break
	}
	m.mu.Unlock()
	if target == nil || !statsStateActive(target.GetState()) {
		return
	}
	s := sample.Stats
	s.Ts = time.Now()
	target.PushStats(&s)
}

func statsStateActive(st task.State) bool {
	switch st {
	case task.StatePurged, task.StateFailed, task.StateCrashed, task.StateStopped, task.StateStopping, task.StatePurging:
		return false
	default:
		return true
	}
}

func diskUsageStateActive(st task.State) bool {
	switch st {
	case task.StatePurged, task.StateFailed, task.StateCrashed, task.StateStopped, task.StatePurging:
		return false
	default:
		return true
	}
}

// watchRuntimeEvents reconnects the runtime event stream after interruption.
func (m *Manager) watchRuntimeEvents(ctx context.Context, events iter.Seq2[runtime.Event, error]) {
	for {
		if events == nil {
			var err error
			events, err = m.Runtimes.WatchEvents(ctx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				m.log.WarnContext(ctx, "runtime events failed, retrying in 5s", "err", err)
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
		}
		for event, err := range events {
			if err != nil {
				if ctx.Err() == nil {
					m.log.WarnContext(ctx, "runtime events stream failed", "err", err)
				}
				break
			}
			if ctx.Err() != nil {
				return
			}
			m.handleRuntimeEvent(ctx, event)
		}
		if ctx.Err() != nil {
			return
		}
		m.log.WarnContext(ctx, "runtime events stream ended, reconnecting in 5s")
		events = nil
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) completeRuntimeImport(ctx context.Context) {
	for {
		m.eventMu.Lock()
		if !m.importing {
			m.eventMu.Unlock()
			return
		}
		events := m.pendingRuntimeEvents
		m.pendingRuntimeEvents = nil
		if len(events) == 0 {
			m.importing = false
			close(m.importDone)
			m.eventMu.Unlock()
			return
		}
		m.eventMu.Unlock()
		for _, event := range events {
			m.applyRuntimeEvent(ctx, event)
		}
	}
}

func (m *Manager) handleRuntimeEvent(ctx context.Context, event runtime.Event) {
	if event.InstanceID == "" || event.Kind == "" {
		m.log.WarnContext(ctx, "ignoring malformed runtime event", "instance", event.InstanceID, "kind", event.Kind)
		return
	}
	m.eventMu.Lock()
	if m.importing {
		m.pendingRuntimeEvents = append(m.pendingRuntimeEvents, event)
		m.eventMu.Unlock()
		return
	}
	m.eventMu.Unlock()
	m.applyRuntimeEvent(ctx, event)
}

func (m *Manager) applyRuntimeEvent(ctx context.Context, event runtime.Event) {
	switch event.Kind {
	case runtime.EventDie:
		m.handleRuntimeInstanceExit(ctx, event.InstanceID)
	case runtime.EventDestroy:
		m.handleRuntimeDestroy(ctx, event.InstanceID)
	case runtime.EventOOM:
		m.handleRuntimeOOM(ctx, event.InstanceID)
	case runtime.EventRestart, runtime.EventStart:
		m.handleRuntimeStart(event.InstanceID) //nolint:contextcheck // reconnection must outlive event handling.
	default:
		m.log.WarnContext(ctx, "ignoring unknown runtime event", "instance", event.InstanceID, "kind", event.Kind)
	}
}

func (m *Manager) entryForRuntime(instanceID runtime.ID) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.tasks {
		if e.task.RuntimeInstanceID() != instanceID {
			continue
		}
		return e
	}
	return nil
}

// handleRuntimeInstanceExit archives a stopped runtime instance.
func (m *Manager) handleRuntimeInstanceExit(ctx context.Context, instanceID runtime.ID) {
	found := m.entryForRuntime(instanceID)
	if found == nil {
		return
	}
	// Lifecycle operations own their self-inflicted stop events. A resumed
	// session must not be detached by the previous incarnation's death.
	if !found.Lifecycle.operationMu.TryLock() {
		return
	}
	defer found.Lifecycle.operationMu.Unlock()
	// Events can arrive after a subsequent revival. Only current runtime
	// observation can distinguish that stale event from a new death.
	info, err := m.Runtimes.Inspect(ctx, instanceID)
	if err != nil {
		m.log.WarnContext(ctx, "confirm runtime exit", "instance", instanceID, "err", err)
		return
	}
	if info.State != "exited" {
		return
	}
	t := found.task
	// Atomically archive as stopped unless the task is already terminal or in
	// cleanup. StateStopping and StatePurging are both self-inflicted: Stop and
	// Purge stop/remove the runtime instance, which emits the "die" event we are
	// handling here. Acting on them would flap the task to StateStopped
	// mid-cleanup and race the cleanup goroutine. Guarding and transitioning in
	// one lock also prevents clobbering a state a concurrent Stop/Purge sets
	// between our check and our write.
	prevState, changed := t.SetStateUnless(task.StateStopped,
		task.StatePurged, task.StateCrashed, task.StateFailed, task.StateStopped,
		task.StateStopping, task.StatePurging, task.StateProvisioning, task.StateStarting)
	if !changed {
		return
	}
	deathBranch := ""
	if p := t.Primary(); p != nil {
		deathBranch = p.Branch
	}
	m.log.InfoContext(ctx, "instance died, archiving as stopped", "instance", instanceID, "task", t.ID, "br", deathBranch, "prev_state", prevState)
	t.DetachSession()
	m.NotifyTaskChange()
}

func (m *Manager) handleRuntimeDestroy(ctx context.Context, instanceID runtime.ID) {
	entry := m.entryForRuntime(instanceID)
	if entry == nil {
		return
	}
	t := entry.Task()
	if _, changed := t.SetStateUnless(task.StateFailed, task.StatePurged, task.StatePurging, task.StateFailed, task.StateStopping); !changed {
		return
	}
	err := errors.New("runtime instance was destroyed")
	t.DetachSession()
	entry.Finish(&task.Result{State: task.StateFailed, Err: err})
	m.log.WarnContext(ctx, "runtime instance destroyed", "instance", instanceID, "task", t.ID)
	m.NotifyTaskChange()
}

func (m *Manager) handleRuntimeOOM(ctx context.Context, instanceID runtime.ID) {
	entry := m.entryForRuntime(instanceID)
	if entry == nil {
		return
	}
	t := entry.Task()
	if !t.MarkSessionCrashed() {
		return
	}
	m.log.WarnContext(ctx, "runtime instance ran out of memory", "instance", instanceID, "task", t.ID)
	m.NotifyTaskChange()
}

func (m *Manager) handleRuntimeStart(instanceID runtime.ID) {
	entry := m.entryForRuntime(instanceID)
	if entry == nil {
		return
	}
	t := entry.Task()
	if _, changed := t.SetStateIfAny(task.StateWaiting, task.StateStopped); !changed {
		return
	}
	entry.Lifecycle.reconnectImportedSession()
	m.NotifyTaskChange()
}

// insertEntry adds an entry under m.mu and signals the change.
func (m *Manager) insertEntry(id ksid.ID, entry *Entry) {
	m.inputMu.Lock()
	m.mu.Lock()
	m.replaceEntryLocked(id, entry)
	m.taskChangedLocked()
	m.mu.Unlock()
	m.inputMu.Unlock()
	m.watchRateLimitEvents(entry.Task())
}

// replaceEntryLocked preserves task-owned input during startup adoption.
// The caller holds both inputMu and mu.
func (m *Manager) replaceEntryLocked(id ksid.ID, entry *Entry) {
	// Import may finish after Cancel snapshots registered entries. Fence its
	// execution admission before publication, even with a retained lifecycle context.
	if m.serverCtx.Err() != nil {
		entry.Lifecycle.Cancel()
	}
	pending := entry.PendingInput()
	if previous := m.tasks[id]; previous != nil && previous != entry {
		pending = previous.PendingInput()
		previous.setPendingInput(nil)
	}
	// Claim staged prompts before registration can certify task-list membership.
	// Pruning then cannot mistake a newly registered task's input for missing.
	if q, ok := m.unrestoredInputs[id]; ok {
		pending = &PendingInput{ID: q.ID, Prompt: q.Prompt}
		delete(m.unrestoredInputs, id)
	}
	entry.setPendingInput(pending)
	m.tasks[id] = entry
}

// watchRateLimitEvents forwards task quota changes to the shared change
// channel, letting task and usage SSE subscribers refresh immediately.
func (m *Manager) watchRateLimitEvents(t *taskrun.Task) {
	m.quotaWatchMu.Lock()
	defer m.quotaWatchMu.Unlock()
	if m.quotaWatchClosed {
		return
	}
	history, live, _ := t.SubscribeRateLimits(m.serverCtx)
	// Subscribe before processing history so events arriving during setup are
	// buffered on live. Apply each event directly: the task snapshot retains
	// only its latest rate limit, while the tracker needs every quota window.
	historyChanged := false
	for _, rateLimit := range history {
		if m.recordRateLimitMessage(rateLimit) {
			historyChanged = true
		}
	}
	if historyChanged {
		m.NotifyTaskChange()
	}
	m.quotaWatchers.Go(func() {
		for rateLimit := range live {
			m.recordRateLimitMessage(rateLimit)
			m.NotifyTaskChange()
		}
	})
}

func (m *Manager) recordRateLimitMessage(rateLimit *agent.RateLimitMessage) bool {
	// A status-only event must not replace the provider's real value with 0%; a
	// rejected status already implies a full window.
	if rateLimit.Utilization < 0 && rateLimit.Status != agent.RateLimitStatusRejected {
		return false
	}
	return m.QuotaTracker.Apply(&quotausage.TaskQuotaUpdate{
		Provider:      rateLimit.QuotaProvider,
		ProviderLabel: rateLimit.QuotaLabel,
		Window:        rateLimit.QuotaWindow,
		Status:        rateLimit.Status,
		Utilization:   rateLimit.Utilization,
		ResetsAt:      rateLimit.ResetsAt,
		ObservedAt:    time.Now().UTC(),
	})
}

// taskChangedLocked closes the current changed channel and replaces it.
// Must be called while holding m.mu.
func (m *Manager) taskChangedLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
	m.changeVersion++
}

// resolveCheckout returns the checkout for a task's primary repo, if any.
func (m *Manager) resolveCheckout(t *taskrun.Task) *repo.Checkout {
	if p := t.Primary(); p != nil {
		r, _ := m.Checkouts.Checkout(p.Name)
		return r
	}
	return nil
}

func applyLoadedSessionMetadata(t *taskrun.Task, lt *taskhistory.LoadedTask) {
	if lt == nil {
		return
	}
	m := lt.SessionMetadata
	if t.GetSessionID() != "" {
		m.SessionID = ""
	}
	t.SetSessionMetadata(m)
}

// importInstance investigates a single runtime instance and registers it as a task.
func (m *Manager) importInstance(ctx context.Context, checkout *repo.Checkout, c *runtime.Instance, branch, taskIDVal string, metadataResolved bool, branchIDs map[string][]ksid.ID, allLogs []*taskhistory.LoadedTask) (*Entry, error) {
	ctx, importTask := trace.NewTask(ctx, "import-instance")
	defer importTask.End()
	relPath := ""
	if checkout != nil {
		relPath = checkout.RelPath
	}
	trace.Logf(ctx, "instance", "%s repo=%s branch=%s", c.ID, relPath, branch)

	// Only import runtime instances that caic started. MetadataTaskID is set at
	// creation and is the authoritative proof of ownership.
	if !metadataResolved {
		var err error
		taskIDVal, err = m.runtimeTaskID(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("metadata check for %s: %w", c.ID, err)
		}
	}
	if taskIDVal == "" {
		m.log.InfoContext(ctx, "instance", "msg", "skipping non-caic", "repo", relPath, "instance", c.ID, "br", branch)
		return nil, nil //nolint:nilnil // non-caic runtime instances are intentionally skipped
	}
	taskID, err := ksid.Parse(taskIDVal)
	if err != nil {
		return nil, fmt.Errorf("parse caic metadata %q on %s: %w", taskIDVal, c.ID, err)
	}
	if taskID == 0 {
		return nil, fmt.Errorf("caic metadata %q on %s parsed to a zero task ID", taskIDVal, c.ID)
	}

	isExited := c.State == "exited"
	if isExited {
		m.log.InfoContext(ctx, "instance", "msg", "importing exited instance as stopped", "instance", c.ID, "br", branch)
	}

	// Find the log file for this task.
	var matchingLogs []*taskhistory.LoadedTask
	for _, log := range allLogs {
		if log != nil && log.TaskID == taskID.String() {
			matchingLogs = append(matchingLogs, log)
		}
	}
	switch len(matchingLogs) {
	case 0:
		return nil, fmt.Errorf("task log %s not found", taskID)
	case 1:
	default:
		return nil, fmt.Errorf("multiple task logs found for task %s", taskID)
	}
	lt := matchingLogs[0]
	importLogPath := lt.LogPath()
	lp := lt.Primary()
	if branch == "" && relPath == "" {
		if lp != nil {
			return nil, fmt.Errorf("task log %s has repo %q but runtime has no repo", taskID, lp.Name)
		}
	} else if lp == nil || lp.Name != relPath || lp.Branch != branch {
		return nil, fmt.Errorf("task log %s does not match runtime repo %q branch %q", taskID, relPath, branch)
	}

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Read harness from runtime metadata only as corroboration. The persisted
	// log header is the authority for any task with a local log.
	harnessLabel, err := m.Runtimes.Metadata(ctx, c.ID, runtime.MetadataHarness)
	if err != nil {
		return nil, fmt.Errorf("read harness metadata for %s: %w", c.ID, err)
	}
	if harnessLabel == "" {
		harnessLabel, err = m.Runtimes.Metadata(ctx, c.ID, runtime.MetadataLegacyHarness)
		if err != nil {
			return nil, fmt.Errorf("read legacy harness metadata for %s: %w", c.ID, err)
		}
	}
	runtimeHarness := harness.Name(harnessLabel)
	if lt.Harness == "" {
		return nil, fmt.Errorf("task log %s has no harness authority", lt.LogPath())
	}
	if runtimeHarness != "" && runtimeHarness != lt.Harness {
		return nil, fmt.Errorf("runtime harness %q does not match task log harness %q", runtimeHarness, lt.Harness)
	}
	backend, ok := m.Backends[lt.Harness]
	if !ok || backend == nil {
		return nil, fmt.Errorf("unknown harness %q for imported task %s", lt.Harness, taskID)
	}

	// Persisted user shutdown intent does not need a readable relay snapshot.
	// Skip probes so unavailable output cannot prevent completing that intent.
	shutdownIntent := lt.State == task.StateStopping || lt.State == task.StatePurging || lt.State == task.StateStopped || lt.State == task.StatePurged
	// Check relay liveness.
	var relayAlive bool
	relayConfirmedDead := isExited
	var relayTimeline agent.RelaySnapshot
	var relaySize int64
	var relayDiag string
	var relaySnapshotRead bool
	relayTarget := c.AgentTarget
	if !isExited && !shutdownIntent {
		var relayErr error
		relayAlive, relayDiag, relayErr = m.relay.Status(ctx, relayTarget)
		relayConfirmedDead = relayErr == nil && !relayAlive
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "check failed during import", "repo", relPath, "br", branch, "instance", c.ID, "err", relayErr, "diag", relayDiag)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
		relayTimeline, relaySize, relayErr = m.relay.ReadTail(readCtx, relayTarget, 10<<20) // 10 MiB tail
		readCancel()
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "read output failed", "repo", relPath, "br", branch, "instance", c.ID, "err", relayErr)
			if !relayConfirmedDead {
				// A live attachment must start at a verified snapshot boundary.
				// Offset zero would replay records already in trusted history.
				return nil, fmt.Errorf("read relay snapshot for %s: %w", c.ID, relayErr)
			}
		} else {
			relaySnapshotRead = true
		}
	}

	if lt.Prompt != "" {
		prompt = lt.Prompt
		startedAt = lt.StartedAt
		stateUpdatedAt = lt.LastStateUpdateAt
	}
	if stateUpdatedAt.IsZero() {
		stateUpdatedAt = time.Now().UTC()
	}
	var mounts []repo.Mount
	if relPath != "" {
		primaryBaseBranch := ""
		if lp != nil {
			primaryBaseBranch = lp.BaseBranch
		}
		// Derive ContainerPath from the runtime instance repo metadata.
		var containerPath string
		if len(c.Repos) > 0 && c.Repos[0].Branch == branch {
			containerPath = c.Repos[0].ContainerPath
		}
		if containerPath == "" {
			containerPath = m.containerPathForRepo(relPath)
		}
		mounts = []repo.Mount{{Name: relPath, BaseBranch: primaryBaseBranch, GitRoot: checkout.Dir, Branch: branch, ContainerPath: containerPath}}
		// Build lookup of instance repos by branch for ContainerPath.
		runtimeRepoByBranch := make(map[string]string, len(c.Repos))
		for _, cr := range c.Repos {
			runtimeRepoByBranch[cr.Branch] = cr.ContainerPath
		}
		for _, lm := range lt.Repos[1:] {
			gitRoot := ""
			if er, ok := m.Checkouts.Checkout(lm.Name); ok {
				gitRoot = er.Dir
			}
			containerPath := runtimeRepoByBranch[lm.Branch]
			if containerPath == "" {
				containerPath = m.containerPathForRepo(lm.Name)
			}
			mounts = append(mounts, repo.Mount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, ContainerPath: containerPath})
		}
	}

	forgeIssue := lt.ForgeIssue
	rt := m.Runtimes.Runtimes[0].Name()
	var forkedFromTaskID, parentTaskID ksid.ID
	if lt.RuntimeName != "" {
		rt = lt.RuntimeName
	} else {
		lt.RuntimeName = rt
	}
	if lt.ForkedFromTaskID != "" {
		forkedFromTaskID, err = ksid.Parse(lt.ForkedFromTaskID)
		if err != nil {
			return nil, fmt.Errorf("import task %q: invalid forkedFromTaskID %q: %w", taskID.String(), lt.ForkedFromTaskID, err)
		}
	}
	if lt.ParentTaskID != "" {
		parentTaskID, err = ksid.Parse(lt.ParentTaskID)
		if err != nil {
			return nil, fmt.Errorf("import task %q: invalid parentTaskID %q: %w", taskID.String(), lt.ParentTaskID, err)
		}
	}
	if c.ID.RuntimeName() != "" {
		rt = c.ID.RuntimeName()
	}

	t, err := taskrun.NewTask(taskID, agent.Prompt{Text: prompt}, lt.Harness, lt.RequestedModel, lt.RequestedEffort, lt.BaseImage, lt.ContainerPlatform, lt.Title)
	if err != nil {
		return nil, fmt.Errorf("import task %q: %w", taskID.String(), err)
	}
	t.Pricer = m.pricer
	t.Rollup = m.rollup
	t.Repos = mounts
	t.RuntimeName = rt
	t.MaxCPUs = lt.MaxCPUs
	t.CacheMounts = lt.CacheMounts
	t.Mounts = lt.Mounts
	t.StartedAt = startedAt
	t.OwnerID = lt.OwnerID
	t.ForkedFromTaskID = forkedFromTaskID
	t.ParentTaskID = parentTaskID
	t.CaicMCP = lt.CaicMCP
	t.Tailscale = c.Tailscale
	t.TailscaleFQDN = c.TailscaleFQDN
	t.USB = c.USB
	t.Display = c.Display
	t.Sudo = c.Sudo
	t.VNCPort = c.VNCPort
	t.Provider = m.provider
	t.ForgeIssue = forgeIssue
	t.SetRuntimeConnectionInfo(c.ID, c.AgentTarget, c.TailscaleFQDN, "", c.VNCPort)
	// Restore GitHub token flag from log trailer (primary) or runtime metadata (fallback).
	gtLabel, _ := m.Runtimes.Metadata(ctx, c.ID, runtime.MetadataGitHubToken)
	if lt.GitHubToken || gtLabel == "true" {
		t.SetGitHubTokenEnabled(true)
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	if c.Sudo {
		if pw, err := m.Runtimes.SudoPassword(ctx, c.ID); err == nil {
			t.SetSudoPassword(pw)
		}
	}
	switch {
	case lt.ForgePR > 0:
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
	case forgeIssue > 0 && checkout != nil && checkout.Repository != nil:
		t.SetPR(checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, 0)
	}

	// Reconcile physical history before restoring Task. Relay-only projections
	// lack locally recorded inputs and provider state from before the bounded
	// snapshot. The final ordered log is the semantic authority, including for
	// responses received while caic was offline. Scan only physical boundaries
	// for reconciliation, then decode the final history once in provider order.
	var encoded []byte
	if len(relayTimeline.RelayRecords) > 0 {
		records, generation, err := m.logStore.LoadRelayBoundaries(importLogPath)
		if err != nil {
			return nil, fmt.Errorf("load imported relay boundaries %s: %w", taskID, err)
		}
		encoded, err = relaySnapshotAppend(records, generation, relayTimeline)
		if err != nil {
			return nil, fmt.Errorf("reconcile imported relay snapshot %s: %w", taskID, err)
		}
	}
	if len(encoded) > 0 {
		log, path, err := m.logStore.Reopen(filepath.Base(lt.LogPath()), t.LogHeader())
		if err != nil {
			return nil, fmt.Errorf("reopen imported task log %s: %w", taskID, err)
		}
		importLogPath = path
		if err := errors.Join(log.AppendRecord(agent.PhysicalRecord{Encoded: encoded}), log.Close()); err != nil {
			return nil, fmt.Errorf("persist imported relay snapshot %s: %w", taskID, err)
		}
		lt, err = m.logStore.LoadSource(path)
		if err != nil {
			return nil, fmt.Errorf("open reconciled task history %s: %w", taskID, err)
		}
	}
	if err := lt.LoadMessages(); err != nil {
		return nil, fmt.Errorf("load reconciled task history %s: %w", taskID, err)
	}
	t.SeedTimelineParts(lt.Timeline, nil)
	m.log.DebugContext(ctx, "relay", "msg", "restored ordered history", "repo", relPath, "br", branch, "instance", c.ID, "alive", relayAlive, "msgs", len(lt.Timeline))
	if relaySnapshotRead {
		t.SetRelayOffset(relaySize)
	}
	var importFailure error
	if lt.LogPath() != "" && lt.State == task.StateRunning && t.InitialPrompt.Text != "" && !t.HasAcceptedInputEvidence() && t.LastExitError() == "" {
		importFailure = taskrun.ErrInitialPromptNotAccepted
		t.SetStateAt(task.StateFailed, stateUpdatedAt)
		m.log.ErrorContext(ctx, "imported task startup incomplete", "task", t.ID, "instance", c.ID, "err", importFailure)
		if !isExited {
			if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // import must outlive request
				m.log.ErrorContext(ctx, "stop incomplete imported task failed", "task", t.ID, "instance", c.ID, "err", err)
			}
		}
	}
	// The durable summary only retains a sticky diff-created signal. Restore
	// the authoritative full branch diff and compact per-repo states before
	// publishing the adopted task, including exited and mid-turn instances that
	// do not run the post-reconnect refresh below. Without this restore its card
	// stays blank until its next mutating tool call.
	if checkout != nil {
		snapshot, err := checkout.DiffStatAndRepoStates(ctx, m.log, m.Runtimes, t.GitTarget())
		if snapshot.Read.NewerThan(repo.GitRead{}) {
			t.RestoreRepositorySummary(&snapshot)
		}
		if err != nil {
			m.log.WarnContext(ctx, "adopt", "msg", "restore diff stat failed", "task", t.ID, "err", err)
		}
	}
	applyLoadedSessionMetadata(t, lt)
	// Restore the persisted diff signal. SeedTimelineParts recomputes diffCreated
	// from replayed history, but that replay is skipped when neither the relay
	// tail nor the log messages load; the summary flag (scanned from the log)
	// carries it through regardless. Sticky: only ever set, never cleared.
	if lt.DiffCreated {
		t.MarkDiffCreated()
	}
	t.SetStateAt(t.GetState(), stateUpdatedAt)
	if importFailure == nil && lt.State == task.StateFailed {
		t.SetStateAt(lt.State, stateUpdatedAt)
	}
	if importFailure == nil && lt.State == task.StateCrashed && t.LastExitError() != "" {
		t.SetStateAt(lt.State, stateUpdatedAt)
	}

	// The validated full parse above may have recovered PR metadata.
	if t.GetPR() == 0 {
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
	}

	if !isExited {
		t.SetTurnStartedAt(time.Now().UTC())
	}

	if isExited {
		if importFailure == nil && t.GetState() != task.StateCrashed {
			if t.LastExitError() != "" {
				t.MarkSessionCrashed()
			} else {
				t.SetState(task.StateStopped)
			}
		}
	} else if relayConfirmedDead {
		relayLog := m.relay.ReadLog(ctx, relayTarget, 4096)
		if relayLog != "" {
			m.log.WarnContext(ctx, "relay", "msg", "log from dead relay", "instance", c.ID, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		trace.Logf(ctx, "import", "%s: relay-dead", c.ID)
		if t.LastExitError() != "" {
			t.MarkSessionCrashed()
			if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // import must outlive request
				m.log.ErrorContext(ctx, "stop failed after imported relay crash", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
			}
		} else if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			m.log.WarnContext(ctx, "relay", "msg", "dead, marking waiting",
				"repo", relPath, "br", branch, "instance", c.ID,
				"sess", t.GetSessionID(), "msgs", t.MessageCount())
		}
	}

	if shutdownIntent {
		t.SetState(task.StateStopped)
		if !isExited {
			if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // completing persisted user intent uses the Manager lifetime
				return nil, fmt.Errorf("complete persisted stop intent: %w", err)
			}
		}
	}
	recoverImported := importFailure == nil && relayConfirmedDead && canRecoverImportedTask(t, lt)
	if recoverImported {
		t.SetState(task.StateProvisioning)
	}
	recoveryState := t.GetState()
	entry := m.NewEntry(t, lt)
	entry.LogPath.Set(importLogPath)
	// Import reconstructs the full timeline because the task may resume live;
	// subsequent lookups must use that authoritative in-memory fold.
	entry.historyInMemory = true
	if t.GetState() == task.StateCrashed || t.GetState() == task.StateFailed {
		resultErr := importFailure
		if resultErr == nil {
			resultErr = errors.New("agent session failed")
		}
		if t.GetState() == task.StateCrashed {
			resultErr = errors.New("agent session crashed")
		}
		if exitErr := t.LastExitError(); exitErr != "" {
			resultErr = errors.New(exitErr)
		}
		costUSD, numTurns, duration, usage, _ := t.LiveStats()
		result := &task.Result{
			State:       t.GetState(),
			DiffStat:    t.LiveDiffStat(),
			CostUSD:     costUSD,
			Duration:    duration,
			NumTurns:    numTurns,
			Usage:       usage,
			AgentResult: t.LastAgentResult(),
			Err:         resultErr,
		}
		entry.Finish(result)
		if !recoverImported {
			if err := entry.Lifecycle.agentRuntime.CommitSettledResult(ctx, t, result); err != nil {
				m.log.WarnContext(ctx, "write imported result trailer failed", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
			}
		}
	}

	// Register the entry, replacing stale log entries for the same branch.
	m.inputMu.Lock()
	m.mu.Lock()
	if relPath != "" || branch != "" {
		for _, oldID := range branchIDs[relPath+"\x00"+branch] {
			if oldID != t.ID {
				delete(m.tasks, oldID)
			}
		}
	}
	m.replaceEntryLocked(t.ID, entry)
	m.taskChangedLocked()
	m.mu.Unlock()
	m.inputMu.Unlock()
	m.watchRateLimitEvents(t)

	m.log.InfoContext(ctx, "instance", "msg", "imported",
		"repo", relPath, "instance", c.ID, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Preserve saved titles on restart, including when new turns were restored.
	if lt.Title == "" {
		entry.Lifecycle.generateTitle()
	}

	// Recovery runs once during startup, only for a proven dead relay and
	// trusted resumable history. Old failed revives lack a typed reason;
	// recognize their persisted runtime-revive error without retrying other
	// startup failures or deliberate stops.
	if recoverImported {
		entry.Lifecycle.scheduleImportedRecovery(recoveryState) //nolint:contextcheck // recovery uses the Manager lifetime
		return entry, nil
	}

	// Auto-reconnect immediately so imported live tasks can accept input as
	// soon as startup returns. The attach must not race the first user reply.
	if importFailure == nil && t.GetState() != task.StateStopped && relayAlive {
		entry.Lifecycle.reconnectImportedSession() //nolint:contextcheck // imported watcher uses the Manager lifetime.
	} else if relayConfirmedDead && t.GetState() != task.StateStopped && t.GetState() != task.StateCrashed && t.GetState() != task.StateFailed {
		m.log.ErrorContext(ctx, "relay dead, stopping instance",
			"repo", relPath, "br", branch, "instance", c.ID,
			"state", t.GetState())
		t.SetState(task.StateStopping)
		if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // import must outlive request
			m.log.ErrorContext(ctx, "stop failed", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
		}
		t.SetState(task.StateStopped)
	}

	return entry, nil
}

// canRecoverImportedTask requires trusted resumable history and excludes
// intentional stop/purge records and incomplete startup failures.
func canRecoverImportedTask(t *taskrun.Task, lt *taskhistory.LoadedTask) bool {
	if t.GetSessionID() == "" || !t.HasAcceptedInputEvidence() {
		return false
	}
	switch t.Harness {
	case harness.Claude, harness.Codex, harness.OpenCode, harness.Pi:
	default:
		return false
	}
	switch lt.State {
	case task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateStarting, task.StateProvisioning, task.StateCrashed:
		return lt.LastTrailer == nil || lt.LastTrailer.StartupFailure == nil
	case task.StateFailed:
		return lt.LastTrailer != nil && lt.LastTrailer.Err != nil && strings.HasPrefix(lt.LastTrailer.Err.Error(), "revive instance:")
	default:
		return false
	}
}

func (m *Manager) runtimeTaskID(ctx context.Context, id runtime.ID) (string, error) {
	value, err := m.Runtimes.Metadata(ctx, id, runtime.MetadataTaskID)
	if err != nil {
		return "", err
	}
	if value != "" {
		return value, nil
	}
	return m.Runtimes.Metadata(ctx, id, runtime.MetadataLegacyTaskID)
}

func (m *Manager) watchInputQueue(ctx context.Context) {
	if err := m.WaitForRuntimeImport(ctx); err != nil {
		return
	}
	for {
		changed := m.Changed()
		m.drainInputQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// drainInputQueue reserves at most one prompt per task and starts independent
// deliveries. A stalled transport must not block another task or queue editing.
// Shutdown cancels transport writes before joining these delivery workers.
func (m *Manager) drainInputQueue(ctx context.Context) {
	m.inputMu.Lock()
	defer m.inputMu.Unlock()
	m.pruneInputQueueLocked(ctx)
	started := false
	for _, e := range m.Entries() {
		q := e.PendingInput()
		if ctx.Err() != nil {
			break
		}
		if q == nil || q.Delivering || q.Error != "" || e.Task().GetState() != task.StateWaiting {
			continue
		}
		next := *q
		next.Delivering = true
		e.setPendingInput(&next)
		id, p := q.ID, q.Prompt
		m.background.Go(func() { m.deliverQueuedInput(ctx, e, id, p) })
		started = true
	}
	if started {
		m.NotifyTaskChange()
	}
}

// pruneInputQueueLocked removes registered terminal tasks and, only after full
// restoration certifies absence, missing tasks. Stopped/crashed tasks may resume.
// In-flight entries stay reserved until their worker finishes.
func (m *Manager) pruneInputQueueLocked(ctx context.Context) {
	removed := make(map[*Entry]*PendingInput)
	for _, e := range m.Entries() {
		if q := e.PendingInput(); q != nil && !q.Delivering && e.Task().GetState().IsTerminal() {
			removed[e] = nil
		}
	}
	var missing map[ksid.ID]queuedPrompt
	if m.TaskListComplete() {
		missing = m.unrestoredInputs
		m.unrestoredInputs = make(map[ksid.ID]queuedPrompt)
	}
	if len(removed) == 0 && len(missing) == 0 {
		return
	}
	if err := m.updatePendingInputsLocked(removed); err != nil {
		maps.Copy(m.unrestoredInputs, missing)
		m.log.ErrorContext(ctx, "discard obsolete pending input failed", "err", err)
		return
	}
	m.NotifyTaskChange()
}

func (m *Manager) deliverQueuedInput(ctx context.Context, e *Entry, id ksid.ID, p agent.Prompt) {
	e.Lifecycle.inputMu.Lock()
	waiting := e.Task().GetState() == task.StateWaiting
	var err error
	if waiting {
		err = e.Lifecycle.sendInputLocked(ctx, p)
	}
	e.Lifecycle.inputMu.Unlock()
	m.inputMu.Lock()
	defer m.inputMu.Unlock()
	// Cancellation rejects in-flight entries, so the reserved prompt still exists.
	q := *e.PendingInput()
	q.Delivering = false
	sent := waiting && err == nil
	if sent {
		err = m.updatePendingInputsLocked(map[*Entry]*PendingInput{e: nil})
		if err != nil {
			err = fmt.Errorf("prompt sent but pending input removal failed; cancel to prevent replay: %w", err)
		}
	}
	// Acceptance followed by a crash before saved removal can replay a prompt:
	// the relay transport provides at-least-once delivery.
	if err != nil && ctx.Err() == nil {
		q.Error, q.Delivered = err.Error(), sent
		m.log.WarnContext(ctx, "pending input failed", "task", e.Task().ID, "input", id, "err", err)
	}
	if !sent || err != nil {
		e.setPendingInput(&q)
	}
	m.NotifyTaskChange()
}

// relaySnapshotAppend returns only physically verified, unmatched relay bytes.
// Provider projections never participate in overlap matching: a bounded relay
// snapshot does not share the ordered local wire state. The caller appends these
// bytes once, then derives Task from the complete persisted history.
func relaySnapshotAppend(records []agent.RelayRecordBoundary, generation string, snapshot agent.RelaySnapshot) ([]byte, error) {
	if len(snapshot.RelayRecords) == 0 {
		return nil, nil
	}
	first := snapshot.RelayRecords[0]
	if len(records) == 0 {
		if first.RelayEnd != int64(first.ByteEnd) {
			return nil, errors.New("marked empty relay generation has a truncated snapshot")
		}
		return snapshot.Encoded, nil
	}
	if first.Generation != "" && first.Generation != generation {
		if first.RelayEnd == int64(first.ByteEnd) {
			return snapshot.Encoded, nil
		}
		if generation != "" {
			return nil, errors.New("new relay generation has a truncated snapshot")
		}
	}
	end, err := relayPositionOverlap(records, snapshot.RelayRecords)
	if err != nil {
		return nil, err
	}
	if end == 0 && records[len(records)-1].Generation != "" {
		return nil, errors.New("marked relay generation has no physical position overlap")
	}
	if end == 0 {
		end, err = relayFingerprintOverlap(records, snapshot.RelayRecords)
		if err != nil {
			return nil, err
		}
	}
	if end == 0 {
		return nil, errors.New("relay snapshot has no physical overlap with durable task log")
	}
	byteEnd := snapshot.RelayRecords[end-1].ByteEnd
	if byteEnd < 0 || byteEnd > len(snapshot.Encoded) {
		return nil, errors.New("relay overlap byte boundary is outside the snapshot")
	}
	return snapshot.Encoded[byteEnd:], nil
}

// relayPositionOverlap matches generation-local offsets independently of
// stateful semantic message counts. The durable endpoint can occur anywhere
// within the bounded snapshot; all shared positions must agree.
func relayPositionOverlap(records, snapshot []agent.RelayRecordBoundary) (int, error) {
	generation := records[len(records)-1].Generation
	if generation == "" {
		return 0, nil
	}
	logEnd := records[len(records)-1].RelayEnd
	matchEnd, matches := 0, 0
	for relayEnd := range snapshot {
		candidate := snapshot[relayEnd]
		if candidate.Generation != generation || candidate.RelayEnd != logEnd {
			continue
		}
		overlap := min(len(records), relayEnd+1)
		logStart := len(records) - overlap
		relayStart := relayEnd + 1 - overlap
		matched := true
		for i := range overlap {
			if records[logStart+i].Generation != generation || snapshot[relayStart+i].Generation != generation ||
				records[logStart+i].RelayEnd != snapshot[relayStart+i].RelayEnd {
				matched = false
				break
			}
		}
		if matched {
			matchEnd = relayEnd + 1
			matches++
		}
	}
	if matches > 1 {
		return 0, errors.New("marked relay generation has ambiguous physical position overlap")
	}
	return matchEnd, nil
}

// relayFingerprintOverlap recognizes unmarked log history by exact physical
// records. A unique endpoint needs two adjacent records, unless the match
// reaches the beginning of either source. This also tolerates a discontinuity
// earlier than that endpoint without trusting an ambiguous repeated record.
func relayFingerprintOverlap(records, snapshot []agent.RelayRecordBoundary) (int, error) {
	for _, record := range records {
		if record.Fingerprint == ([32]byte{}) {
			return 0, nil
		}
	}
	for _, record := range snapshot {
		if record.Fingerprint == ([32]byte{}) {
			return 0, nil
		}
	}
	logEnd := records[len(records)-1].Fingerprint
	matchEnd, matches := 0, 0
	for relayEnd := range snapshot {
		if snapshot[relayEnd].Fingerprint != logEnd {
			continue
		}
		overlap := 0
		for overlap < len(records) && overlap <= relayEnd &&
			records[len(records)-1-overlap].Fingerprint == snapshot[relayEnd-overlap].Fingerprint {
			overlap++
		}
		if overlap < 2 && overlap != len(records) && overlap != relayEnd+1 {
			continue
		}
		matchEnd = relayEnd + 1
		matches++
	}
	if matches == 0 {
		return 0, errors.New("unmarked relay history has no exact physical overlap")
	}
	if matches > 1 {
		return 0, errors.New("unmarked relay history has ambiguous repeated physical overlap")
	}
	return matchEnd, nil
}

// queuedPrompt is a validated coordinator-owned pending prompt.
type queuedPrompt struct {
	ID     ksid.ID
	TaskID ksid.ID
	Prompt agent.Prompt
}
