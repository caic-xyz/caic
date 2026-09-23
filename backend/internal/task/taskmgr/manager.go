// Package taskmgr owns the task registry: task state, creation, lifecycle, and runtime-instance import.
//
// It sits between the HTTP adapter (internal/server) and the domain layer
// (internal/task): task.Task / repo.Checkout are domain types, Manager owns
// their registry state, and Lifecycle performs operations for one task.
//
// Two contexts coexist here. Methods accept a request-scoped ctx that is
// honored for synchronous work (state checks, logging). Background goroutines
// that must outlive any single request use serverCtx instead — it tracks the
// lifetime of the Manager itself.
package taskmgr

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
)

type relayReader interface {
	Status(ctx context.Context, target runtime.ConnectionTarget) (bool, string, error)
	ReadTail(ctx context.Context, target runtime.ConnectionTarget, parser *agent.LogRecordParser, maxBytes int64) (agent.ParsedTimeline, int64, error)
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

func (agentRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parser *agent.LogRecordParser, maxBytes int64) (timeline agent.ParsedTimeline, size int64, err error) {
	if target.SSHHost == "" {
		return agent.ParsedTimeline{}, 0, errors.New("agent connection target missing SSH host")
	}
	return agent.ReadRelayTail(ctx, target.SSHHost, parser, maxBytes)
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
	LogStore *taskslog.Store
	// Runtimes validates runtime selection and dispatches task runtime operations.
	Runtimes            *runtime.Router
	Backends            agent.Backends
	HarnessEnv          map[string][]string
	RuntimeMetadata     runtime.Metadata
	RuntimeStartTimeout time.Duration
	Provider            genai.Provider // nil-safe
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

// Manager owns task lifecycle state, runtime import, session watching, and
// stats streaming.
type Manager struct {
	// Immutable.
	agent.Backends

	Runtimes     *runtime.Router
	QuotaTracker *quotausage.Tracker
	Checkouts    *repo.Registry

	log                 *slog.Logger
	serverCtx           context.Context // lifetime of the Manager; for goroutines that outlive requests
	cancelServerCtx     context.CancelFunc
	logStore            *taskslog.Store
	harnessEnv          map[string][]string
	runtimeMetadata     runtime.Metadata
	runtimeStartTimeout time.Duration
	provider            genai.Provider
	pricer              quotausage.ModelPricer
	rollup              task.RollupSink
	taskMCPScoper       TaskMCPScoper
	relay               relayReader

	// Guarded by eventMu.
	eventMu              sync.Mutex
	importing            bool
	pendingRuntimeEvents []runtime.Event

	// Guarded by quotaWatchMu.
	quotaWatchMu     sync.Mutex
	quotaWatchers    sync.WaitGroup
	quotaWatchClosed bool

	background sync.WaitGroup

	branchAllocationMu sync.Mutex // Serializes branch adoption checks and reservations across task startups.

	// Guarded by mu.
	mu            sync.Mutex
	tasks         map[string]*Entry
	changed       chan struct{} // closed on mutation, replaced under mu
	changeVersion uint64        // incremented with changed under mu

	// Guarded by settledMu. Tracks the background settled-history pass so the
	// task-list stream can report it (loading -> completed | failed). The zero
	// value means no pass has completed, so a fresh Manager reports loading
	// until CompleteSettledLoad runs.
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
		Runtimes:            cfg.Runtimes,
		QuotaTracker:        quotausage.NewTracker(),
		log:                 cfg.Log.With("cmp", "taskmgr"),
		serverCtx:           serverCtx,
		cancelServerCtx:     cancelServerCtx,
		Backends:            maps.Clone(cfg.Backends),
		logStore:            cfg.LogStore,
		harnessEnv:          cfg.HarnessEnv,
		runtimeMetadata:     maps.Clone(cfg.RuntimeMetadata),
		runtimeStartTimeout: cfg.RuntimeStartTimeout,
		provider:            cfg.Provider,
		pricer:              pricer,
		rollup:              cfg.Rollup,
		Checkouts:           cfg.Checkouts,
		relay:               agentRelayReader{},
		tasks:               make(map[string]*Entry),
		changed:             make(chan struct{}),
	}
	return m, nil
}

// Close stops background work and waits for task lifecycles and all Manager watchers to finish.
func (m *Manager) Close() error {
	m.quotaWatchMu.Lock()
	m.quotaWatchClosed = true
	m.cancelServerCtx()
	m.quotaWatchMu.Unlock()
	m.Range(func(_ string, e *Entry) bool {
		if e.Lifecycle != nil {
			_ = e.Lifecycle.Close()
		}
		return true
	})
	m.quotaWatchers.Wait()
	m.background.Wait()
	// After all task activity has settled, flush the usage rollup's pending
	// deltas so a graceful shutdown never loses them.
	return m.rollup.Close()
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
	m.eventMu.Unlock()
	events, err := m.Runtimes.WatchEvents(m.serverCtx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
	if err != nil {
		m.eventMu.Lock()
		m.importing = false
		m.eventMu.Unlock()
	}
	m.background.Go(func() { m.watchRuntimeEvents(m.serverCtx, events) })
	m.background.Go(func() { m.watchStats(m.serverCtx) })
	m.background.Go(func() { m.watchDiskUsage(m.serverCtx) })
	if err != nil {
		return fmt.Errorf("watch runtime events: %w", err)
	}
	return nil
}

// NewEntry creates an unregistered entry with its immutable lifecycle.
func (m *Manager) NewEntry(t *task.Task, lt *taskslog.LoadedTask) *Entry {
	var taskMCP mcp.Registry
	if t.CaicMCP {
		taskMCP = m.taskMCPScoper.ForTask(t.ID)
	}
	e := &Entry{
		task:            t,
		loadedTask:      lt,
		historyInMemory: lt == nil,
	}
	e.term.Store(&entryTerminal{done: make(chan struct{})})
	if lt != nil {
		e.LogPath.Set(lt.LogPath())
	}
	e.Lifecycle = &Lifecycle{
		manager: m,
		entry:   e,
		ctx:     context.WithoutCancel(m.serverCtx),
		agentRuntime: task.AgentRuntime{
			Backends:            m.Backends,
			LogStore:            m.logStore,
			LogPath:             &e.LogPath,
			Runtimes:            m.Runtimes,
			Log:                 m.log.With("task", t.ID),
			NotifyTaskChange:    m.NotifyTaskChange,
			Checkout:            m.resolveCheckout(t),
			RuntimeMetadata:     m.runtimeMetadata,
			RuntimeStartTimeout: m.runtimeStartTimeout,
			MCPRegistry:         taskMCP,
		},
	}
	return e
}

// Insert registers a pre-built entry. Production task creation goes through
// Create/Fork; Insert is retained for tests (in internal/tasks and
// internal/server) that seed the registry without a real checkout.
func (m *Manager) Insert(id string, entry *Entry) {
	m.insertEntry(id, entry)
}

// Range iterates over every registered entry. It snapshots the registry under
// m.mu and invokes fn unlocked, so fn may safely call back into the Manager
// (e.g. Checkout). The entry set is a point-in-time snapshot; Entry pointers are
// stable and carry their own locking. Stops iteration if fn returns false.
func (m *Manager) Range(fn func(id string, e *Entry) bool) {
	m.mu.Lock()
	type kv struct {
		id string
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

// CompleteSettledLoad records the outcome of the settled-history pass and
// notifies task-list watchers so they can emit the status transition. err is
// nil on a clean pass; a non-nil err keeps the valid partial subset already
// registered and surfaces as the pass error. A clean completion clears any
// previously recorded error so the state always reflects the pass that just
// ran.
func (m *Manager) CompleteSettledLoad(err error) {
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

// SettledStatus reports the background task-history load pass state atomically:
// loading is true until CompleteSettledLoad runs, and error is non-empty only
// after a failed pass. Both fields are read under one lock so a concurrent
// CompleteSettledLoad cannot interleave between two separate reads.
func (m *Manager) SettledStatus() (loading bool, err string) {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	return !m.settledCompleted, m.settledError
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

// Create handles the HTTP task creation path.
func (m *Manager) Create(ctx context.Context, p CreateParams) (string, error) { //nolint:gocritic // CreateParams is a request-shaped value bag
	// Resolve primary checkout.
	if len(p.Repos) > 0 {
		_, ok := m.Checkouts.Checkout(p.Repos[0].Name)
		if !ok {
			return "", &Error{Kind: KindBadRequest, Code: CodeUnknownRepository, Msg: "unknown repo: " + p.Repos[0].Name}
		}
	}

	// Validate that every extra repo has a registered checkout (branches are
	// allocated later, in one pass, by allocateBranches).
	for _, rs := range p.Repos[min(1, len(p.Repos)):] {
		if _, ok := m.Checkouts.Checkout(rs.Name); !ok {
			return "", &Error{Kind: KindBadRequest, Code: CodeUnknownRepository, Msg: "unknown extra repo: " + rs.Name}
		}
	}

	runtimeName, err := resolveRuntimeName(m.Runtimes, p.RuntimeName)
	if err != nil {
		return "", err
	}

	backend, ok := m.Backends[p.Harness]
	if !ok {
		return "", &Error{Kind: KindBadRequest, Code: CodeUnknownHarness, Msg: "unknown harness: " + string(p.Harness)}
	}
	if p.CaicMCP && !m.TaskMCPAvailable() {
		return "", badRequestf("task-scoped MCP is unavailable")
	}

	if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
		return "", &Error{Kind: KindBadRequest, Code: CodeUnsupportedModel, Msg: "unsupported model for " + string(p.Harness) + ": " + p.Model}
	}

	if len(p.Prompt.Images) > 0 && !backend.SupportsImages() {
		return "", badRequestf("%s does not support images", string(p.Harness))
	}

	// Build RepoMount slice. ContainerPath follows the fixed "~/src/<name>"
	// convention used by the instance provisioner. Use the basename unless
	// another registered repo shares it.
	mounts := make([]taskslog.RepoMount, len(p.Repos))
	for i, rs := range p.Repos {
		r, _ := m.Checkouts.Checkout(rs.Name)
		mounts[i] = taskslog.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir, ContainerPath: m.containerPathForRepo(rs.Name)}
	}

	t, err := m.newTask(p.Prompt, p.Harness, p.Model, p.Effort, p.BaseImage, p.ContainerPlatform, "")
	if err != nil {
		return "", badRequestf("%v", err)
	}
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

	m.insertEntry(t.ID.String(), entry)
	entry.Lifecycle.generateTitle()

	// Run setup under the lifecycle context.
	entry.Lifecycle.wg.Go(func() {
		// The primary's branch is created by the agent runtime before instance launch,
		// so it only needs a name reserved; extras are created here.
		if err := m.allocateBranches(entry.Lifecycle.ctx, t, mounts, 1, true); err != nil {
			entry.Finish(&taskslog.Result{State: taskslog.StateFailed, Err: internalErr(err, "allocate branch")})
			m.NotifyTaskChange()
			return
		}

		ghToken := p.ResolvedGitHubToken

		if err := entry.Lifecycle.Start(entry.Lifecycle.ctx, ghToken); err != nil {
			return
		}
	})
	return t.ID.String(), nil
}

// GetEntry returns the entry for taskID.
func (m *Manager) GetEntry(taskID string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.tasks[taskID]
	return e, ok
}

// Entries returns a point-in-time sequence of registered task entries.
//
// The iteration order is unspecified. The sequence owns a shallow snapshot,
// so callers do not hold the manager lock while yielding entries.
func (m *Manager) Entries() iter.Seq2[string, *Entry] {
	m.mu.Lock()
	tasks := maps.Clone(m.tasks)
	m.mu.Unlock()
	return maps.All(tasks)
}

// EffectiveBaseBranch returns the branch the task was forked from.
func (m *Manager) EffectiveBaseBranch(t *task.Task) string {
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
func (m *Manager) SudoPassword(ctx context.Context, t *task.Task) string {
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

// ImportInstances registers preexisting runtime instances as tasks.
func (m *Manager) ImportInstances(ctx context.Context, instances []runtime.Instance, allLogs []*taskslog.LoadedTask) ([]*Entry, error) {
	defer m.completeRuntimeImport(ctx)
	if instances == nil {
		return nil, nil
	}
	resolvedTaskIDs, rejected, validationErr := m.resolveImportTaskIDs(ctx, instances)

	// Map repo+branch loaded from purged task logs to their ID.
	branchIDs := make(map[string][]string)
	m.Range(func(id string, e *Entry) bool {
		if p := e.Task().Primary(); p != nil && p.Branch != "" {
			key := p.Name + "\x00" + p.Branch
			branchIDs[key] = append(branchIDs[key], id)
		}
		return true
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	if validationErr != nil {
		errs = append(errs, validationErr)
	}
	var entries []*Entry
	claimed := make(map[runtime.ID]bool, len(instances))
	for id := range rejected {
		claimed[id] = true
	}

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
			wg.Go(func() {
				entry, err := m.importInstance(ctx, checkout, c, branch, taskIDVal, metadataResolved, branchIDs, allLogs)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				if entry != nil {
					mu.Lock()
					entries = append(entries, entry)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()

	// Import no-repo runtime instances.
	for i := range instances {
		c := &instances[i]
		if claimed[c.ID] || !strings.HasPrefix(string(c.ID.InstanceID()), "md-agent-") {
			continue
		}
		taskIDVal, metadataResolved := resolvedTaskIDs[c.ID]
		wg.Go(func() {
			entry, err := m.importInstance(ctx, nil, c, "", taskIDVal, metadataResolved, branchIDs, allLogs)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			if entry != nil {
				mu.Lock()
				entries = append(entries, entry)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	return entries, errors.Join(errs...)
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

// needsTitleRegen reports whether an imported task needs an LLM title regeneration.
func needsTitleRegen(t *task.Task, lt *taskslog.LoadedTask, resolver taskslog.WireResolver) bool {
	if lt == nil || lt.Title == "" {
		return true
	}
	// Skip the full log parse for large files — it would block startup
	// for minutes. The title from the log header is good enough.
	const maxLogSize = 100 << 20 // 100 MiB
	if lt.LogSize > maxLogSize {
		return false
	}
	logResults := 0
	if err := lt.LoadMessagesWithResolver(resolver); err == nil {
		logResults = countTimelineResults(lt.Timeline)
	}
	restoredResults := countResultMessages(t.Messages())
	return restoredResults > logResults
}

func countTimelineResults(entries []agent.TimedMessage) int {
	n := 0
	for _, entry := range entries {
		if _, ok := entry.Message.(*agent.ResultMessage); ok {
			n++
		}
	}
	return n
}

// countResultMessages counts the number of ResultMessages in msgs.
func countResultMessages(msgs []agent.Message) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(*agent.ResultMessage); ok {
			n++
		}
	}
	return n
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
		if st == taskslog.StateWaiting || st == taskslog.StateStopped || st == taskslog.StateCrashed || st == taskslog.StateFailed || st == taskslog.StatePurged {
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
func (m *Manager) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	entry, ok := m.GetEntry(taskID)
	if !ok {
		return "", "", notFoundf("task %s not found", taskID)
	}
	for {
		st := entry.task.GetState()
		switch st {
		case taskslog.StateWaiting, taskslog.StateStopped, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StatePurged:
			return st.String(), lastResultText(entry.task), nil
		case taskslog.StatePending, taskslog.StateBranching, taskslog.StateProvisioning, taskslog.StateStarting, taskslog.StateRunning, taskslog.StateAsking, taskslog.StateHasPlan, taskslog.StatePulling, taskslog.StatePushing, taskslog.StateStopping, taskslog.StatePurging:
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
func lastResultText(t *task.Task) string {
	msgs := t.Messages()
	for _, msg := range slices.Backward(msgs) {
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
func (m *Manager) LoadUnsettledTasks(plain []*taskslog.LoadedTask) error {
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
func (m *Manager) LoadPurgedTasks(purged []*taskslog.LoadedTask) error {
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
func (m *Manager) HistorySource(entry *Entry) (*taskslog.LoadedTask, error) {
	if entry == nil {
		return nil, errors.New("task history entry is nil")
	}
	path := entry.LogPath.Get()
	if path == "" {
		return nil, taskslog.ErrNoLog
	}
	loaded, err := taskslog.LoadHistorySource(path)
	if err != nil {
		return nil, fmt.Errorf("load task history source: %w", err)
	}
	loaded.SetWireResolver(m)
	entry.SetLoadedTask(loaded)
	return loaded, nil
}

// BackwardMessages selects the task's authoritative history source and yields
// messages newest first. Live and adopted tasks read their in-memory snapshot.
// Restored cold tasks parse through EOF into a bounded-memory temporary spool
// before yielding, because compressed semantic history cannot be decoded in
// reverse directly.
func (m *Manager) BackwardMessages(ctx context.Context, entry *Entry) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		if entry == nil {
			yield(nil, errors.New("task history entry is nil"))
			return
		}
		if entry.historyInMemory {
			for message := range entry.Task().BackwardMessages() {
				if !yield(message, nil) {
					return
				}
			}
			return
		}
		source, err := m.HistorySource(entry)
		if err != nil {
			yield(nil, err)
			return
		}
		for parsed, streamErr := range source.BackwardMessages(ctx) {
			if !yield(parsed.Message, streamErr) {
				return
			}
			if streamErr != nil {
				return
			}
		}
	}
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

// newTask creates a task and attaches the manager's model pricer.
func (m *Manager) newTask(prompt agent.Prompt, h harness.Name, model, effort, baseImage, containerPlatform, title string) (*task.Task, error) {
	t, err := task.NewTask(ksid.NewID(), prompt, h, model, effort, baseImage, containerPlatform, title)
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
func (m *Manager) insertLoadedTasks(lts []*taskslog.LoadedTask) (int, error) {
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
			lt.LastTrailer = &taskslog.Result{State: taskslog.StateFailed}
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
			if _, exists := m.tasks[taskID.String()]; exists {
				continue
			}
		}
		if p := lt.Primary(); p != nil && p.Branch != "" {
			if _, exists := existingBranches[p.Name+"\x00"+p.Branch]; exists {
				continue
			}
		}
		lt.SetWireResolver(m)
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
		t, err := task.NewTask(taskID, agent.Prompt{Text: lt.Prompt}, lt.Harness, lt.RequestedModel, lt.RequestedEffort, lt.BaseImage, lt.ContainerPlatform, lt.Title)
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
		if lt.SessionID != "" || lt.ReportedModel != "" || lt.ReportedEffort != "" || lt.AgentVersion != "" {
			t.SetSessionMetadata(lt.SessionID, lt.ReportedModel, lt.ReportedEffort, lt.AgentVersion)
		}
		if lt.State == taskslog.StateRunning {
			// Nothing adopted the instance that was running this log, so the task can
			// never settle. Name the downgrade: a bare failure with no error gives the
			// task list no way to explain what happened.
			m.log.Warn("loaded running task log without an adopted instance; marking failed",
				"task", lt.TaskID, "harness", lt.Harness, "log", lt.LogPath(), "state", lt.State)
			t.SetState(taskslog.StateFailed)
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
		entry := m.NewEntry(t, lt)
		entry.Finish(lt.LastTrailer)
		m.tasks[t.ID.String()] = entry
		loaded++
	}
	return loaded, nil
}

func (m *Manager) writeTaskResultTrailer(entry *Entry, res *taskslog.Result) error {
	t := entry.Task()
	// A tracked path preserves the filename actually on disk; only recompute
	// it from the task when no log has been opened yet in this process.
	name := t.LogFilename()
	if path := entry.LogPath.Get(); path != "" {
		name = filepath.Base(path)
	}
	log, path, err := m.logStore.Reopen(name, t.LogHeader())
	if err != nil {
		return err
	}
	entry.LogPath.Set(path)
	return errors.Join(m.logStore.WriteResultTrailer(log, t.Title(), res), log.Close())
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

// allocateBranches assigns every repo of a task its final branch. Fresh tasks
// adopt an explicitly selected local branch when it is available; remote-only,
// default, and already-associated branches receive a new caic-N branch. Forks
// always receive new branches. mounts[:reserveOnly] are repos whose new branch is created elsewhere
// (by md.Fork for a fork's source repos, or by the agent runtime before launch for
// a fresh task's primary), so they only need a name reserved.
// mounts[reserveOnly:] are new to the host, so their branch is created here from
// their own checkout.
//
// Every non-adopted repo of the task shares one branch name — the highest next
// sequence number across their checkouts — so checking out the task's branch in
// another mapped repo takes the same command. Reserving the number in every
// checkout before any branch is created keeps concurrent single-repo tasks from
// reissuing it; a repo whose shared name already exists in its git (a stale
// branch from an older task) falls back to its own next caic-N, so mixed names
// within one task remain possible.
//
// Reservation happens before any task log opens: the log filename and metadata
// header embed each repo's final branch name, and md.Fork creates the git
// branches using these names verbatim (the caller owns their uniqueness).
func (m *Manager) allocateBranches(ctx context.Context, t *task.Task, mounts []taskslog.RepoMount, reserveOnly int, adoptLocal bool) error {
	m.branchAllocationMu.Lock()
	defer m.branchAllocationMu.Unlock()

	// Resolve adoption first; adopted repos keep their own branch name.
	var shared []int
	var checkouts []*repo.Checkout
	for i := range mounts {
		ws, ok := m.Checkouts.Checkout(mounts[i].Name)
		if !ok {
			return fmt.Errorf("repo %q is not registered", mounts[i].Name)
		}
		if adoptLocal && mounts[i].BaseBranch != "" && !m.BranchAssociated(mounts[i].Name, mounts[i].BaseBranch) {
			branches, err := ws.AdoptableBranches(ctx, m.log)
			if err != nil {
				return fmt.Errorf("inspect branch for %s: %w", mounts[i].Name, err)
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
		return nil
	}

	// One branch name for every repo that receives a fresh caic-N branch: the
	// highest next sequence number across their checkouts, so it is free in
	// each. branchAllocationMu makes the peek-and-reserve below atomic with
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
			return fmt.Errorf("allocate branch for %s: %w", mounts[i].Name, err)
		}
		t.SetRepoBranch(i, branch)
	}
	return nil
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
		changedDone := make(chan struct{})
		go func() {
			defer close(changedDone)
			select {
			case <-changed:
				cancel()
			case <-ctx.Done():
				cancel()
			case <-streamCtx.Done():
			}
		}()

		stats, err := m.Runtimes.WatchStats(streamCtx, ids)
		if err != nil {
			cancel()
			<-changedDone
			m.log.WarnContext(ctx, "stats stream failed", "err", err)
			if !waitStatsRetry(ctx, changed, retryDelay) {
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
		cancel()
		<-changedDone
		if ctx.Err() != nil {
			return
		}
		if streamCtx.Err() == nil && !waitStatsRetry(ctx, changed, retryDelay) {
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
	tasks := make(map[runtime.ID]*task.Task, len(usage))
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

func waitStatsRetry(ctx context.Context, changed <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return true
	case <-timer.C:
		return true
	}
}

func (m *Manager) pushStatsSample(sample *runtime.StatsSample) {
	m.mu.Lock()
	var target *task.Task
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

func statsStateActive(st taskslog.State) bool {
	switch st {
	case taskslog.StatePurged, taskslog.StateFailed, taskslog.StateCrashed, taskslog.StateStopped, taskslog.StateStopping, taskslog.StatePurging:
		return false
	default:
		return true
	}
}

func diskUsageStateActive(st taskslog.State) bool {
	switch st {
	case taskslog.StatePurged, taskslog.StateFailed, taskslog.StateCrashed, taskslog.StateStopped, taskslog.StatePurging:
		return false
	default:
		return true
	}
}

// watchRuntimeEvents reconnects the runtime event stream after interruption.
func (m *Manager) watchRuntimeEvents(ctx context.Context, events <-chan runtime.Event) {
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
		streamEnded := false
		for !streamEnded {
			select {
			case event, ok := <-events:
				if !ok {
					streamEnded = true
					continue
				}
				m.handleRuntimeEvent(ctx, event)
			case <-ctx.Done():
				return
			}
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
	t := found.task
	// Atomically archive as stopped unless the task is already terminal or in
	// cleanup. StateStopping and StatePurging are both self-inflicted: Stop and
	// Purge stop/remove the runtime instance, which emits the "die" event we are
	// handling here. Acting on them would flap the task to StateStopped
	// mid-cleanup and race the cleanup goroutine. Guarding and transitioning in
	// one lock also prevents clobbering a state a concurrent Stop/Purge sets
	// between our check and our write.
	prevState, changed := t.SetStateUnless(taskslog.StateStopped,
		taskslog.StatePurged, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StateStopped,
		taskslog.StateStopping, taskslog.StatePurging)
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
	if _, changed := t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurged, taskslog.StatePurging, taskslog.StateFailed, taskslog.StateStopping); !changed {
		return
	}
	err := errors.New("runtime instance was destroyed")
	t.DetachSession()
	entry.Finish(&taskslog.Result{State: taskslog.StateFailed, Err: err})
	m.log.WarnContext(ctx, "runtime instance destroyed", "instance", instanceID, "task", t.ID)
	m.NotifyTaskChange()
}

func (m *Manager) handleRuntimeOOM(ctx context.Context, instanceID runtime.ID) {
	entry := m.entryForRuntime(instanceID)
	if entry == nil {
		return
	}
	t := entry.Task()
	if !t.RecordSessionCrash(ctx, errors.New("runtime instance ran out of memory")) {
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
	if _, changed := t.SetStateIfAny(taskslog.StateWaiting, taskslog.StateStopped); !changed {
		return
	}
	entry.Lifecycle.reconnectImportedSession()
	m.NotifyTaskChange()
}

// insertEntry adds an entry under m.mu and signals the change.
func (m *Manager) insertEntry(id string, entry *Entry) {
	m.mu.Lock()
	m.tasks[id] = entry
	m.taskChangedLocked()
	m.mu.Unlock()
	m.watchRateLimitEvents(entry.Task())
}

// insertDelegatedEntry registers a direct child while enforcing the temporary
// per-parent limit atomically with registration.
func (m *Manager) insertDelegatedEntry(entry *Entry) error {
	t := entry.Task()
	m.mu.Lock()
	children := 0
	for _, candidate := range m.tasks {
		child := candidate.Task()
		if child.ParentTaskID == t.ParentTaskID && child.GetState() != taskslog.StatePurged {
			children++
		}
	}
	if children >= maxDelegatedTasksPerParent {
		m.mu.Unlock()
		return conflict(fmt.Sprintf("delegating task already has %d non-purged child tasks", maxDelegatedTasksPerParent))
	}
	m.tasks[t.ID.String()] = entry
	m.taskChangedLocked()
	m.mu.Unlock()
	m.watchRateLimitEvents(t)
	return nil
}

// watchRateLimitEvents forwards task quota changes to the shared change
// channel, letting task and usage SSE subscribers refresh immediately.
func (m *Manager) watchRateLimitEvents(t *task.Task) {
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
	return m.QuotaTracker.Apply(&quotausage.TaskQuotaUpdate{
		Provider:      rateLimit.QuotaProvider,
		ProviderLabel: rateLimit.QuotaLabel,
		Window:        rateLimit.QuotaWindow,
		Status:        rateLimit.Status,
		UsedPct:       rateLimit.Utilization * 100,
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
func (m *Manager) resolveCheckout(t *task.Task) *repo.Checkout {
	if p := t.Primary(); p != nil {
		r, _ := m.Checkouts.Checkout(p.Name)
		return r
	}
	return nil
}

func applyLoadedSessionMetadata(t *task.Task, lt *taskslog.LoadedTask) {
	if lt == nil {
		return
	}
	sessionID := lt.SessionID
	if t.GetSessionID() != "" {
		sessionID = ""
	}
	t.SetSessionMetadata(sessionID, lt.ReportedModel, lt.ReportedEffort, lt.AgentVersion)
}

// importInstance investigates a single runtime instance and registers it as a task.
func (m *Manager) importInstance(ctx context.Context, checkout *repo.Checkout, c *runtime.Instance, branch, taskIDVal string, metadataResolved bool, branchIDs map[string][]string, allLogs []*taskslog.LoadedTask) (*Entry, error) {
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
	var matchingLogs []*taskslog.LoadedTask
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
	lt.SetWireResolver(m)
	// Check relay liveness.
	var relayAlive bool
	var relayTimeline agent.ParsedTimeline
	var relaySize int64
	var relayDiag string
	var relaySnapshotRead bool
	relayTarget := c.AgentTarget
	if !isExited {
		var relayErr error
		relayAlive, relayDiag, relayErr = m.relay.Status(ctx, relayTarget)
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "check failed during import", "repo", relPath, "br", branch, "instance", c.ID, "err", relayErr, "diag", relayDiag)
		}
		parser, parserErr := agent.NewLogRecordParser(lt.LogVersion, backend.NewWire().ParseMessage)
		if parserErr != nil {
			return nil, fmt.Errorf("construct relay parser: %w", parserErr)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
		relayTimeline, relaySize, relayErr = m.relay.ReadTail(readCtx, relayTarget, parser, 10<<20) // 10 MiB tail
		readCancel()
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "read output failed", "repo", relPath, "br", branch, "instance", c.ID, "err", relayErr)
			relayAlive = false
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
	if lt.SessionID == "" || lt.AgentVersion == "" {
		if err := lt.LoadSessionMetadataWithResolver(m); err != nil {
			m.log.WarnContext(ctx, "load session metadata failed", "repo", relPath, "br", branch, "err", err)
		}
	}

	var mounts []taskslog.RepoMount
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
		mounts = []taskslog.RepoMount{{Name: relPath, BaseBranch: primaryBaseBranch, GitRoot: checkout.Dir, Branch: branch, ContainerPath: containerPath}}
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
			mounts = append(mounts, taskslog.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, ContainerPath: containerPath})
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

	t, err := task.NewTask(taskID, agent.Prompt{Text: prompt}, lt.Harness, lt.RequestedModel, lt.RequestedEffort, lt.BaseImage, lt.ContainerPlatform, lt.Title)
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
	t.SetStateAt(taskslog.StateRunning, stateUpdatedAt)
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

	// Restore messages from both the local log and the relay tail. Unlike a
	// completed-task restore, adoption needs the full timeline to resume live
	// operation. The local log has the full pre-restart history; the relay tail
	// has output produced while the server was down plus some overlap. Merge them
	// so the UI does not collapse to the bounded relay tail after a server
	// restart. Fail closed: malformed persistent history must not attach a live
	// task with untrusted state.
	if err := lt.LoadMessagesWithResolver(m); err != nil {
		return nil, fmt.Errorf("load messages for imported task %s: %w", taskID, err)
	}
	if len(relayTimeline.Messages) > 0 || len(relayTimeline.RelayRecords) > 0 {
		logTimeline := agent.ParsedTimeline{Messages: lt.Timeline, RelayRecords: lt.RelayRecords}
		merger := newLogRelayMessageMerger(logTimeline, lt.Harness)
		merger.logGeneration = lt.RelayGeneration
		merger.strictRelay = lt.LogVersion != agent.LogVersionV1
		timeline := merger.merge(relayTimeline)
		if merger.err != nil {
			if lt.LogVersion != agent.LogVersionV2 || !relayAlive || !relaySnapshotRead {
				return nil, fmt.Errorf("reconcile imported relay snapshot %s: %w", taskID, merger.err)
			}
			// V2 recorded both relay output and local stdin as agent records. A
			// live relay snapshot that cannot prove physical overlap may therefore
			// be valid even though its durable endpoint is ambiguous. Preserve the
			// trusted local history, skip the unverified offline tail, and resume at
			// the inspected end so only future relay output is appended.
			timeline = append(slices.Clone(lt.Timeline), agent.TimedMessage{Message: &agent.LogMessage{
				Line: "Recovered legacy relay session; output produced while caic was unavailable could not be verified and was not retained.",
			}})
			m.log.WarnContext(ctx, "relay", "msg", "legacy recovery skipped unverified relay tail",
				"repo", relPath, "br", branch, "instance", c.ID, "reason", merger.err)
		} else if encoded := merger.relayAppend(relayTimeline); len(encoded) > 0 {
			log, _, err := m.logStore.Reopen(filepath.Base(lt.LogPath()), t.LogHeader())
			if err != nil {
				return nil, fmt.Errorf("reopen imported task log %s: %w", taskID, err)
			}
			if err := errors.Join(log.AppendNative(encoded), log.Close()); err != nil {
				return nil, fmt.Errorf("persist imported relay snapshot %s: %w", taskID, err)
			}
		}
		t.SeedTimelineEntries(timeline)
		m.log.DebugContext(ctx, "relay", "msg", "restored from", "repo", relPath, "br", branch, "instance", c.ID, "alive", relayAlive, "msgs", len(timeline), "relayMsgs", len(relayTimeline.Messages))
	} else if len(lt.Timeline) > 0 {
		t.SeedTimelineEntries(lt.Timeline)
		m.log.WarnContext(ctx, "relay", "msg", "restored from log", "repo", relPath, "br", branch, "instance", c.ID, "msgs", len(lt.Timeline))
	}
	if relaySnapshotRead {
		t.SetRelayOffset(relaySize)
	}
	// The durable log only retains a sticky diff-created signal, and relay-tail
	// overlap filtering omits diff-stat controls. Restore the authoritative full
	// branch diff and compact per-repo states before publishing the adopted
	// task, including for exited and mid-turn instances that do not run the
	// post-reconnect refresh below. Without this restore an adopted task's card
	// stays blank until its next mutating tool call.
	if checkout != nil {
		if ds, states, err := checkout.DiffStatAndRepoStates(ctx, m.log, m.Runtimes, t.RuntimeInstanceID(), t.RuntimeRepos()); err == nil {
			if len(ds) > 0 {
				t.SetLiveDiffStat(ds)
			}
			if len(states) > 0 {
				t.SetLiveRepoStates(states)
			}
		} else {
			m.log.WarnContext(ctx, "adopt", "msg", "restore diff stat failed", "task", t.ID, "err", err)
		}
	}
	applyLoadedSessionMetadata(t, lt)
	// Restore the persisted diff signal. SeedTimeline recomputes diffCreated
	// from replayed history, but that replay is skipped when neither the relay
	// tail nor the log messages load; the summary flag (scanned from the log)
	// carries it through regardless. Sticky: only ever set, never cleared.
	if lt.DiffCreated {
		t.MarkDiffCreated()
	}
	t.SetStateAt(t.GetState(), stateUpdatedAt)
	if lt.State == taskslog.StateFailed {
		t.SetStateAt(lt.State, stateUpdatedAt)
	}
	if lt.State == taskslog.StateCrashed && t.LastExitError() != "" {
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
		if t.GetState() != taskslog.StateCrashed {
			if t.LastExitError() != "" {
				t.RecordSessionCrash(ctx, errors.New("agent subprocess exited before import"))
			} else {
				t.SetState(taskslog.StateStopped)
			}
		}
	} else if !relayAlive {
		relayLog := m.relay.ReadLog(ctx, relayTarget, 4096)
		if relayLog != "" {
			m.log.WarnContext(ctx, "relay", "msg", "log from dead relay", "instance", c.ID, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		trace.Logf(ctx, "import", "%s: relay-dead", c.ID)
		if t.LastExitError() != "" {
			t.RecordSessionCrash(ctx, errors.New("relay exited before import"))
			if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // import must outlive request
				m.log.ErrorContext(ctx, "stop failed after imported relay crash", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
			}
		} else if t.GetState() == taskslog.StateRunning {
			t.SetStateAt(taskslog.StateWaiting, stateUpdatedAt)
			m.log.WarnContext(ctx, "relay", "msg", "dead, marking waiting",
				"repo", relPath, "br", branch, "instance", c.ID,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entry := m.NewEntry(t, lt)
	// Import reconstructs the full timeline because the task may resume live;
	// subsequent lookups must use that authoritative in-memory fold.
	entry.historyInMemory = true
	if t.GetState() == taskslog.StateCrashed || t.GetState() == taskslog.StateFailed {
		resultErr := errors.New("agent session failed")
		if t.GetState() == taskslog.StateCrashed {
			resultErr = errors.New("agent session crashed")
		}
		if exitErr := t.LastExitError(); exitErr != "" {
			resultErr = errors.New(exitErr)
		}
		costUSD, numTurns, duration, usage, _ := t.LiveStats()
		result := &taskslog.Result{
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
		if err := m.writeTaskResultTrailer(entry, result); err != nil {
			m.log.WarnContext(ctx, "write imported result trailer failed", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
		}
	}

	// Register the entry, replacing stale log entries for the same branch.
	m.mu.Lock()
	if relPath != "" || branch != "" {
		for _, oldID := range branchIDs[relPath+"\x00"+branch] {
			delete(m.tasks, oldID)
		}
	}
	m.tasks[t.ID.String()] = entry
	m.taskChangedLocked()
	m.mu.Unlock()
	m.watchRateLimitEvents(t)

	m.log.InfoContext(ctx, "instance", "msg", "imported",
		"repo", relPath, "instance", c.ID, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed.
	if needsTitleRegen(t, lt, m) {
		entry.Lifecycle.generateTitle()
	}

	// Auto-reconnect immediately so imported live tasks can accept input as
	// soon as startup returns. EnsureSession may still replace an already-exited
	// attach in the background, but the attach itself must not race the first
	// user reply after restart.
	if t.GetState() != taskslog.StateStopped && relayAlive {
		entry.Lifecycle.reconnectImportedSession() //nolint:contextcheck // imported watcher uses the Manager lifetime.
	} else if !relayAlive && t.GetState() != taskslog.StateStopped && t.GetState() != taskslog.StateCrashed && t.GetState() != taskslog.StateFailed {
		m.log.ErrorContext(ctx, "relay dead, stopping instance",
			"repo", relPath, "br", branch, "instance", c.ID,
			"state", t.GetState())
		t.SetState(taskslog.StateStopping)
		if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // import must outlive request
			m.log.ErrorContext(ctx, "stop failed", "repo", relPath, "br", branch, "instance", c.ID, "err", err)
		}
		t.SetState(taskslog.StateStopped)
	}

	return entry, nil
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

// logRelayMessageMerger owns the import-time overlap rules for disk-log
// messages and relay-tail messages. The durable log contains the full
// pre-restart history, while the relay tail contains output produced while caic
// was down plus some overlapping history. Pi replay can reconstruct the same
// logical messages with small metadata differences, so overlap matching uses
// semantic equivalence instead of strict equality.
type logRelayMessageMerger struct {
	logEntries      []agent.TimedMessage
	logRecords      []agent.RelayRecordBoundary
	logGeneration   string
	strictRelay     bool
	ignoreRelayInit bool
	relayByteStart  int
	recordOverlap   bool
	err             error
}

func newLogRelayMessageMerger(logTimeline agent.ParsedTimeline, h harness.Name) *logRelayMessageMerger {
	logHasInit := false
	if h == harness.Pi {
		for _, entry := range logTimeline.Messages {
			if _, ok := entry.Message.(*agent.InitMessage); ok {
				logHasInit = true
				break
			}
		}
	}
	logGeneration := ""
	if len(logTimeline.RelayRecords) > 0 {
		logGeneration = logTimeline.RelayRecords[len(logTimeline.RelayRecords)-1].Generation
	}
	return &logRelayMessageMerger{
		logEntries:      logTimeline.Messages,
		logRecords:      logTimeline.RelayRecords,
		logGeneration:   logGeneration,
		ignoreRelayInit: logHasInit,
	}
}

func (m *logRelayMessageMerger) merge(relayTimeline agent.ParsedTimeline) []agent.TimedMessage {
	relayEntries := relayTimeline.Messages
	if (m.strictRelay || m.logGeneration != "") && len(m.logRecords) == 0 {
		if len(relayTimeline.RelayRecords) == 0 {
			return slices.Clone(m.logEntries)
		}
		first := relayTimeline.RelayRecords[0]
		if first.RelayEnd != int64(first.ByteEnd) {
			m.err = errors.New("marked empty relay generation has a truncated snapshot")
			return slices.Clone(m.logEntries)
		}
		m.recordOverlap = true
		return append(slices.Clone(m.logEntries), m.comparableRelayTimeline(relayEntries)...)
	}
	if len(relayTimeline.RelayRecords) > 0 {
		relayGeneration := relayTimeline.RelayRecords[0].Generation
		if relayGeneration != "" && relayGeneration != m.logGeneration {
			first := relayTimeline.RelayRecords[0]
			if first.RelayEnd == int64(first.ByteEnd) {
				m.recordOverlap = true
				return append(slices.Clone(m.logEntries), m.comparableRelayTimeline(relayEntries)...)
			}
			if m.logGeneration != "" {
				m.err = errors.New("new relay generation has a truncated snapshot")
				return slices.Clone(m.logEntries)
			}
		}
	}
	if merged, ok := m.mergeByRelayPosition(relayTimeline); ok {
		return merged
	}
	if m.err != nil {
		return slices.Clone(m.logEntries)
	}
	if len(m.logRecords) > 0 && len(relayTimeline.RelayRecords) > 0 &&
		m.logRecords[len(m.logRecords)-1].Generation != "" {
		m.err = errors.New("marked relay generation has no physical position overlap")
		return slices.Clone(m.logEntries)
	}
	if merged, ok := m.mergeByRelayFingerprint(relayTimeline); ok {
		return merged
	}
	if m.err != nil {
		return slices.Clone(m.logEntries)
	}
	if len(m.logEntries) == 0 {
		return slices.Clone(m.comparableRelayTimeline(relayEntries))
	}
	if len(relayEntries) == 0 {
		return slices.Clone(m.logEntries)
	}
	relayEntries = m.comparableRelayTimeline(relayEntries)
	comparableLogEntries := m.comparableLogTimeline()
	maxOverlap := min(len(comparableLogEntries), len(relayEntries))
	for n := maxOverlap; n > 0; n-- {
		if m.messagesEqual(comparableLogEntries[len(comparableLogEntries)-n:], relayEntries[:n]) {
			return append(slices.Clone(m.logEntries), relayEntries[n:]...)
		}
	}
	return append(slices.Clone(m.logEntries), relayEntries...)
}

// mergeByRelayPosition matches physical relay records shared by the durable
// log and relay snapshot. The snapshot can begin before the durable endpoint,
// so the overlap may end anywhere within it. Absolute offsets remain stable
// when stateful parsing produces different semantic fields or message counts
// across the two scans.
func (m *logRelayMessageMerger) mergeByRelayPosition(relayTimeline agent.ParsedTimeline) ([]agent.TimedMessage, bool) {
	if len(m.logRecords) == 0 || len(relayTimeline.RelayRecords) == 0 {
		return nil, false
	}
	generation := m.logRecords[len(m.logRecords)-1].Generation
	if generation == "" {
		return nil, false
	}
	logEnd := m.logRecords[len(m.logRecords)-1]
	matchEnd := 0
	matches := 0
	for relayEnd := range relayTimeline.RelayRecords {
		candidate := relayTimeline.RelayRecords[relayEnd]
		if candidate.Generation != generation || candidate.RelayEnd != logEnd.RelayEnd {
			continue
		}
		overlap := min(len(m.logRecords), relayEnd+1)
		logStart := len(m.logRecords) - overlap
		relayStart := relayEnd + 1 - overlap
		matched := true
		for i := range overlap {
			if m.logRecords[logStart+i].Generation != generation ||
				relayTimeline.RelayRecords[relayStart+i].Generation != generation ||
				m.logRecords[logStart+i].RelayEnd != relayTimeline.RelayRecords[relayStart+i].RelayEnd {
				matched = false
				break
			}
		}
		if matched {
			matchEnd = relayEnd + 1
			matches++
		}
	}
	if matches == 1 {
		return m.finishPhysicalMerge(relayTimeline, matchEnd)
	}
	if matches > 1 {
		m.err = errors.New("marked relay generation has ambiguous physical position overlap")
	}
	return nil, false
}

// mergeByRelayFingerprint upgrades logs written before relay-generation
// markers existed. The relay snapshot can contain records before the durable
// endpoint, and old adoption runs may have left discontinuities earlier in the
// log. A unique endpoint with two adjacent exact records is still authoritative;
// a shorter match is accepted only when it reaches the start of either input.
// Future launches use generation-local offsets instead.
func (m *logRelayMessageMerger) mergeByRelayFingerprint(relayTimeline agent.ParsedTimeline) ([]agent.TimedMessage, bool) {
	if len(m.logRecords) == 0 || len(relayTimeline.RelayRecords) == 0 {
		return nil, false
	}
	for _, record := range m.logRecords {
		if record.Fingerprint == ([32]byte{}) {
			return nil, false
		}
	}
	for _, record := range relayTimeline.RelayRecords {
		if record.Fingerprint == ([32]byte{}) {
			return nil, false
		}
	}
	logEnd := m.logRecords[len(m.logRecords)-1].Fingerprint
	matchEnd := 0
	matches := 0
	for relayEnd := range relayTimeline.RelayRecords {
		if relayTimeline.RelayRecords[relayEnd].Fingerprint != logEnd {
			continue
		}
		overlap := 0
		for overlap < len(m.logRecords) && overlap <= relayEnd &&
			m.logRecords[len(m.logRecords)-1-overlap].Fingerprint == relayTimeline.RelayRecords[relayEnd-overlap].Fingerprint {
			overlap++
		}
		if overlap < 2 && overlap != len(m.logRecords) && overlap != relayEnd+1 {
			continue
		}
		matchEnd = relayEnd + 1
		matches++
	}
	if matches == 1 {
		return m.finishPhysicalMerge(relayTimeline, matchEnd)
	}
	if matches == 0 {
		m.err = errors.New("unmarked relay history has no exact physical overlap")
	} else {
		m.err = errors.New("unmarked relay history has ambiguous repeated physical overlap")
	}
	return nil, false
}

func (m *logRelayMessageMerger) finishPhysicalMerge(relayTimeline agent.ParsedTimeline, relayRecordEnd int) ([]agent.TimedMessage, bool) {
	messageEnd := relayTimeline.RelayRecords[relayRecordEnd-1].MessageEnd
	if messageEnd < 0 || messageEnd > len(relayTimeline.Messages) {
		return nil, false
	}
	byteEnd := relayTimeline.RelayRecords[relayRecordEnd-1].ByteEnd
	if byteEnd < 0 || byteEnd > len(relayTimeline.Encoded) {
		return nil, false
	}
	m.relayByteStart = byteEnd
	m.recordOverlap = true
	relaySuffix := m.comparableRelayTimeline(relayTimeline.Messages[messageEnd:])
	return append(slices.Clone(m.logEntries), relaySuffix...), true
}

func (m *logRelayMessageMerger) relayAppend(relayTimeline agent.ParsedTimeline) []byte {
	if !m.recordOverlap || m.relayByteStart >= len(relayTimeline.Encoded) {
		return nil
	}
	return relayTimeline.Encoded[m.relayByteStart:]
}

// comparableLogTimeline drops caic controls that exist only in the durable
// log. They remain in the merged output but cannot prevent adjacent native
// messages from matching the relay overlap.
func (m *logRelayMessageMerger) comparableLogTimeline() []agent.TimedMessage {
	for i, entry := range m.logEntries {
		if _, logOnly := entry.Message.(*agent.TurnCommitSnapshotMessage); !logOnly {
			continue
		}
		entries := slices.Clone(m.logEntries[:i])
		for _, entry := range m.logEntries[i+1:] {
			if _, logOnly := entry.Message.(*agent.TurnCommitSnapshotMessage); !logOnly {
				entries = append(entries, entry)
			}
		}
		return entries
	}
	return m.logEntries
}

// comparableRelayTimeline drops relay-only metadata before overlap matching.
func (m *logRelayMessageMerger) comparableRelayTimeline(relayEntries []agent.TimedMessage) []agent.TimedMessage {
	for i, entry := range relayEntries {
		if m.relayMessageComparableToLog(entry.Message) {
			continue
		}
		entries := slices.Clone(relayEntries[:i])
		for _, entry := range relayEntries[i+1:] {
			if m.relayMessageComparableToLog(entry.Message) {
				entries = append(entries, entry)
			}
		}
		return entries
	}
	return relayEntries
}

func (m *logRelayMessageMerger) relayMessageComparableToLog(msg agent.Message) bool {
	switch msg.(type) {
	case *agent.DiffStatMessage:
		// caic_diff_stat is emitted by caic, not the agent conversation. It may be
		// present in the relay output but absent from the task log replay stream.
		return false
	case *agent.InitMessage:
		// Pi can synthesize an init from message_start during relay-tail parsing. If
		// the durable log already has an init, keep the log's session boundary and
		// ignore the relay one for overlap matching. Other harnesses emit genuine
		// init records that must remain as overlap anchors.
		return !m.ignoreRelayInit
	default:
		return true
	}
}

func (m *logRelayMessageMerger) messagesEqual(a, b []agent.TimedMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !m.messagesEquivalent(a[i].Message, b[i].Message) {
			return false
		}
	}
	return true
}

// messagesEquivalent reports whether two parsed messages represent the same
// agent event for overlap detection.
func (m *logRelayMessageMerger) messagesEquivalent(a, b agent.Message) bool {
	switch av := a.(type) {
	case *agent.UsageMessage:
		bv, ok := b.(*agent.UsageMessage)
		// ContextWindow is restored from caic_model_info in the durable task log.
		// A bounded relay tail can start after that record, leaving the replayed
		// usage with the same token counts but no context-window metadata.
		return ok && av.Usage == bv.Usage && av.ReportedModel == bv.ReportedModel
	case *agent.ResultMessage:
		bv, ok := b.(*agent.ResultMessage)
		if !ok {
			return false
		}
		aa := *av
		bb := *bv
		// Pi derives duration and turn count from parser/session-local state. The
		// same completed turn can therefore have different values when parsed from
		// the durable log and from the relay tail during restart import.
		aa.DurationMs = 0
		bb.DurationMs = 0
		aa.DurationAPIMs = 0
		bb.DurationAPIMs = 0
		aa.NumTurns = 0
		bb.NumTurns = 0
		// The context window is derived from the model the wire has seen. A bounded
		// relay tail can resume after that record, so like the usage message above
		// the window cannot anchor the overlap.
		aa.ContextWindow = 0
		bb.ContextWindow = 0
		return reflect.DeepEqual(&aa, &bb)
	default:
		return reflect.DeepEqual(a, b)
	}
}
