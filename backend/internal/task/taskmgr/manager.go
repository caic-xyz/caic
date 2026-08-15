// Package taskmgr owns task registry state, creation, stats streaming, and
// runtime-instance import.
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
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
)

// errTaskNotFound is returned when a task ID doesn't exist.
var errTaskNotFound = &Error{Kind: KindNotFound, Msg: "task not found"}

type relayReader interface {
	Status(ctx context.Context, target runtime.ConnectionTarget) (bool, string, error)
	ReadTail(ctx context.Context, target runtime.ConnectionTarget, parser *agent.LogRecordParser, maxBytes int64) ([]agent.ParsedMessage, int64, error)
	ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string
}

type agentRelayReader struct{}

func (agentRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	if target.SSHHost == "" {
		return false, "", errors.New("agent connection target missing SSH host")
	}
	return agent.RelayStatus(ctx, target.SSHHost)
}

func (agentRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parser *agent.LogRecordParser, maxBytes int64) (msgs []agent.ParsedMessage, size int64, err error) {
	if target.SSHHost == "" {
		return nil, 0, errors.New("agent connection target missing SSH host")
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
	LogDir    string
	CacheDir  string
	// Runtimes validates runtime selection and dispatches task runtime operations.
	Runtimes            *runtime.Router
	Backends            map[harness.Name]agent.Backend
	HarnessEnv          map[string][]string
	RuntimeMetadata     runtime.Metadata
	RuntimeStartTimeout time.Duration
	Provider            genai.Provider // nil-safe
	Checkouts           *repo.Registry
}

// Manager owns task lifecycle state, runtime import, session watching, and
// stats streaming.
type Manager struct {
	// Immutable.
	Runtimes     *runtime.Router
	QuotaTracker *quotausage.Tracker
	Backends     map[harness.Name]agent.Backend
	Checkouts    *repo.Registry
	Logs         task.LogStore

	log                 *slog.Logger
	serverCtx           context.Context // lifetime of the Manager; for goroutines that outlive requests
	cancelServerCtx     context.CancelFunc
	cacheDir            string
	harnessEnv          map[string][]string
	runtimeMetadata     runtime.Metadata
	runtimeStartTimeout time.Duration
	provider            genai.Provider
	relay               relayReader

	// Guarded by eventMu.
	eventMu              sync.Mutex
	eventWatchStarted    bool
	importing            bool
	pendingRuntimeEvents []runtime.Event

	// Guarded by quotaWatchMu.
	quotaWatchMu     sync.Mutex
	quotaWatchers    sync.WaitGroup
	quotaWatchClosed bool

	background sync.WaitGroup

	// Guarded by mu.
	mu      sync.Mutex
	tasks   map[string]*Entry
	changed chan struct{} // closed on mutation, replaced under mu
}

// New creates a Manager. Register each repo checkout in Checkouts, then Start.
func New(cfg Config) (*Manager, error) { //nolint:gocritic // Config is a value bag passed once at construction
	if cfg.ServerCtx == nil {
		return nil, errors.New("task manager server context is required")
	}
	if cfg.Runtimes == nil {
		return nil, errors.New("task manager runtime router is required")
	}
	if cfg.Checkouts == nil {
		return nil, errors.New("task manager checkout registry is required")
	}
	if cfg.RuntimeStartTimeout <= 0 {
		return nil, errors.New("task manager runtime start timeout is required")
	}
	if cfg.Log == nil {
		return nil, errors.New("task manager logger is required")
	}
	serverCtx, cancelServerCtx := context.WithCancel(cfg.ServerCtx)
	m := &Manager{
		Runtimes:            cfg.Runtimes,
		QuotaTracker:        quotausage.NewTracker(),
		log:                 cfg.Log.With("cmp", "taskmgr"),
		serverCtx:           serverCtx,
		cancelServerCtx:     cancelServerCtx,
		cacheDir:            cfg.CacheDir,
		Backends:            maps.Clone(cfg.Backends),
		Logs:                task.LogStore{LogDir: cfg.LogDir},
		harnessEnv:          cfg.HarnessEnv,
		runtimeMetadata:     maps.Clone(cfg.RuntimeMetadata),
		runtimeStartTimeout: cfg.RuntimeStartTimeout,
		provider:            cfg.Provider,
		Checkouts:           cfg.Checkouts,
		relay:               agentRelayReader{},
		tasks:               make(map[string]*Entry),
		changed:             make(chan struct{}),
	}
	return m, nil
}

// Close stops background work and waits for task lifecycles and all Manager
// watchers to finish. It currently always returns nil.
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
	return nil
}

// BeginImport subscribes to runtime events before startup inventory is listed.
// ImportInstances applies any buffered events after it registers the snapshot.
func (m *Manager) BeginImport() error {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	if m.eventWatchStarted {
		return errors.New("runtime event watch already started")
	}
	m.importing = true
	events, err := m.Runtimes.WatchEvents(m.serverCtx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
	if err != nil {
		m.importing = false
		return fmt.Errorf("watch runtime events: %w", err)
	}
	m.eventWatchStarted = true
	m.background.Go(func() { m.watchRuntimeEvents(m.serverCtx, events) })
	return nil
}

// Start launches the runtime event and stats watchers. BeginImport may start
// the event watcher first to fence startup inventory from incoming events.
func (m *Manager) Start() {
	m.eventMu.Lock()
	watchStarted := m.eventWatchStarted
	m.eventWatchStarted = true
	m.eventMu.Unlock()
	if !watchStarted {
		m.background.Go(func() { m.watchRuntimeEvents(m.serverCtx, nil) })
	}
	m.background.Go(func() { m.watchStats(m.serverCtx) })
}

// NewEntry creates an unregistered entry with its immutable lifecycle.
func (m *Manager) NewEntry(t *task.Task, lt *task.LoadedTask) *Entry {
	e := &Entry{
		task:       t,
		loadedTask: lt,
		done:       make(chan struct{}),
	}
	e.Lifecycle = &Lifecycle{
		manager: m,
		entry:   e,
		ctx:     context.WithoutCancel(m.serverCtx),
		agentRuntime: task.AgentRuntime{
			Backends:            m.Backends,
			Logs:                m.Logs,
			Runtimes:            m.Runtimes,
			Log:                 m.log,
			NotifyTaskChange:    m.NotifyTaskChange,
			Checkout:            m.resolveCheckout(t),
			RuntimeMetadata:     m.runtimeMetadata,
			RuntimeStartTimeout: m.runtimeStartTimeout,
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

// NotifyTaskChange signals that task data may have changed.
func (m *Manager) NotifyTaskChange() {
	m.mu.Lock()
	m.taskChanged()
	m.mu.Unlock()
}

// Changed returns a channel that is closed on task mutation.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// Create handles the HTTP task creation path.
func (m *Manager) Create(ctx context.Context, p CreateParams) (string, error) { //nolint:gocritic // CreateParams is a request-shaped value bag
	// Resolve primary checkout.
	if len(p.Repos) > 0 {
		_, ok := m.Checkouts.Checkout(p.Repos[0].Name)
		if !ok {
			return "", badRequestf("unknown repo: %s", p.Repos[0].Name)
		}
	}

	// Validate that every extra repo has a registered checkout (branches are
	// allocated later, in one pass, by allocateBranches).
	for _, rs := range p.Repos[min(1, len(p.Repos)):] {
		if _, ok := m.Checkouts.Checkout(rs.Name); !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
	}

	runtimeName, err := resolveRuntimeName(m.Runtimes, p.RuntimeName)
	if err != nil {
		return "", err
	}

	backend, ok := m.Backends[p.Harness]
	if !ok {
		return "", badRequestf("unknown harness: %s", string(p.Harness))
	}

	if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
		return "", badRequestf("unsupported model for %s: %s", string(p.Harness), p.Model)
	}

	if len(p.Prompt.Images) > 0 && !backend.SupportsImages() {
		return "", badRequestf("%s does not support images", string(p.Harness))
	}

	// Build RepoMount slice. ContainerPath follows the fixed "~/src/<name>"
	// convention used by the instance provisioner. Use the basename unless
	// another registered repo shares it.
	mounts := make([]task.RepoMount, len(p.Repos))
	for i, rs := range p.Repos {
		r, _ := m.Checkouts.Checkout(rs.Name)
		mounts[i] = task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir, ContainerPath: m.containerPathForRepo(rs.Name)}
	}

	t := &task.Task{
		ID:                ksid.NewID(),
		InitialPrompt:     p.Prompt,
		Repos:             mounts,
		Harness:           p.Harness,
		Model:             p.Model,
		Effort:            p.Effort,
		RuntimeName:       runtimeName,
		BaseImage:         p.BaseImage,
		ContainerPlatform: p.ContainerPlatform,
		MaxCPUs:           p.MaxCPUs,
		CacheMounts:       slices.Clone(p.CacheMounts),
		Mounts:            slices.Clone(p.Mounts),
		GitHubToken:       p.GitHubToken,
		Tailscale:         p.Tailscale,
		USB:               p.USB,
		Display:           p.Display,
		Sudo:              p.Sudo,
		StartedAt:         time.Now().UTC(),
		OwnerID:           p.OwnerID,
		Provider:          m.provider,
	}
	t.ForgeIssue = p.ForgeIssue
	if p.ForgeOwner != "" {
		// Set forge owner/repo so ListPendingBotTasks can resolve the commenter.
		t.SetPR(p.ForgeOwner, p.ForgeRepo, 0)
	}
	t.SetTitle(p.Prompt.Text)
	entry := m.NewEntry(t, nil)

	m.insertEntry(t.ID.String(), entry)
	entry.Lifecycle.generateTitle()

	// Run setup under the lifecycle context.
	entry.Lifecycle.wg.Go(func() {
		// The primary's branch is created by the agent runtime concurrently with instance
		// launch, so it only needs a name reserved; extras are created here.
		if err := m.allocateBranches(entry.Lifecycle.ctx, t, mounts, 1); err != nil {
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "allocate branch")})
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
func (m *Manager) ImportInstances(ctx context.Context, instances []runtime.Instance, allLogs []*task.LoadedTask) ([]*Entry, error) {
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
func needsTitleRegen(t *task.Task, lt *task.LoadedTask, resolver task.NativeParserResolver) bool {
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
		logResults = countResultMessages(lt.Msgs)
	}
	restoredResults := countResultMessages(t.Messages())
	return restoredResults > logResults
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
func (m *Manager) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	entry, ok := m.GetEntry(taskID)
	if !ok {
		return "", "", notFoundf("task %s not found", taskID)
	}
	for {
		st := entry.task.GetState()
		switch st { //nolint:exhaustive // only terminal/idle states are relevant
		case task.StateWaiting, task.StateStopped, task.StateCrashed, task.StateFailed, task.StatePurged:
			return st.String(), lastResultText(entry.task), nil
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

// LoadPurgedTasks loads purged tasks from pre-loaded logs.
func (m *Manager) LoadPurgedTasks(all []*task.LoadedTask) error {
	const oldest = 14 * 24 * time.Hour
	const maxPurgedPerRepo = 5
	var purged []*task.LoadedTask
	now := time.Now().UTC()
	for _, lt := range all {
		if now.Sub(lt.LastStateUpdateAt) > oldest {
			continue
		}
		if lt.Result == nil {
			lt.Result = &task.Result{State: task.StateFailed}
		}
		purged = append(purged, lt)
	}
	slices.SortFunc(purged, func(a, b *task.LoadedTask) int {
		return b.LastStateUpdateAt.Compare(a.LastStateUpdateAt)
	})
	perRepo := make(map[string]int)
	kept := purged[:0]
	for _, lt := range purged {
		key := ""
		if p := lt.Primary(); p != nil {
			key = p.Name
		}
		if perRepo[key] < maxPurgedPerRepo {
			perRepo[key]++
			kept = append(kept, lt)
		}
	}
	purged = kept
	if len(purged) == 0 {
		m.log.Info("no purged tasks to load", "candidates", len(all))
		return nil
	}
	// Do not scan full logs here. Some compressed histories are multi-GB, and
	// historical session metadata must not block the task list becoming usable.
	m.mu.Lock()
	defer m.mu.Unlock()
	existingBranches := make(map[string]struct{})
	for _, e := range m.tasks {
		if p := e.task.Primary(); p != nil && p.Branch != "" {
			existingBranches[p.Name+"\x00"+p.Branch] = struct{}{}
		}
	}
	loaded := 0
	for _, lt := range purged {
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
		lt.SetNativeParserResolver(m.resolveNativeParser)
		rt := m.Runtimes.Runtimes[0].Name()
		if lt.RuntimeName != "" {
			rt = lt.RuntimeName
		} else {
			lt.RuntimeName = rt
		}
		var forkedFromTaskID ksid.ID
		if lt.ForkedFromTaskID != "" {
			var err error
			forkedFromTaskID, err = ksid.Parse(lt.ForkedFromTaskID)
			if err != nil {
				return fmt.Errorf("load purged task %q: invalid forkedFromTaskID %q: %w", lt.TaskID, lt.ForkedFromTaskID, err)
			}
		}
		t := &task.Task{
			ID:                taskID,
			InitialPrompt:     agent.Prompt{Text: lt.Prompt},
			RuntimeName:       rt,
			Model:             lt.Model,
			Effort:            lt.Effort,
			Repos:             lt.Repos,
			Harness:           lt.Harness,
			BaseImage:         lt.BaseImage,
			ContainerPlatform: lt.ContainerPlatform,
			MaxCPUs:           lt.MaxCPUs,
			CacheMounts:       slices.Clone(lt.CacheMounts),
			Mounts:            slices.Clone(lt.Mounts),
			StartedAt:         lt.StartedAt,
			ForkedFromTaskID:  forkedFromTaskID,
			Tailscale:         lt.Tailscale,
			USB:               lt.USB,
			Display:           lt.Display,
			Sudo:              lt.Sudo,
			GitHubToken:       lt.GitHubToken,
		}
		t.SetStateAt(lt.State, lt.LastStateUpdateAt)
		if lt.SessionID != "" || lt.AgentVersion != "" {
			t.SetSessionMetadata(lt.SessionID, "", lt.AgentVersion)
		}
		if lt.LogPath() != "" {
			t.SetLogPath(lt.LogPath())
		}
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		if lt.State == task.StateRunning {
			t.SetState(task.StateFailed)
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
		entry := m.NewEntry(t, lt)
		entry.Finish(lt.Result)
		m.tasks[t.ID.String()] = entry
		loaded++
	}
	if loaded > 0 {
		m.taskChanged()
	}
	m.log.Info("loaded purged tasks from logs", "n", loaded, "candidates", len(purged))
	return nil
}

// LoadMessagesOnDemand triggers lazy message loading for purged tasks.
func (m *Manager) LoadMessagesOnDemand(entry *Entry) {
	m.loadTaskMessagesOnDemand(entry)
}

// HistorySource opens a header-only raw-log reader for stopped or terminal history.
func (m *Manager) HistorySource(entry *Entry) (*task.LoadedTask, error) {
	if entry == nil {
		return nil, errors.New("task history entry is nil")
	}
	if entry.Task().LogPath() == "" {
		return entry.LoadedTask(), nil
	}
	loaded, err := entry.Task().LoadHistorySource()
	if err != nil {
		return nil, fmt.Errorf("load task history source: %w", err)
	}
	loaded.SetNativeParserResolver(m.resolveNativeParser)
	entry.SetLoadedTask(loaded)
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

// allocateBranches assigns every repo of a task its own branch name, uniformly —
// the Manager owns branch-name allocation for all repos, with no special case for
// the primary. mounts[:reserveOnly] are repos whose branch is created elsewhere
// (by md.Fork for a fork's source repos, or by the agent runtime concurrently with
// launch for a fresh task's primary), so they only need a name reserved.
// mounts[reserveOnly:] are new to the host, so their branch is created here from
// their own checkout.
func (m *Manager) allocateBranches(ctx context.Context, t *task.Task, mounts []task.RepoMount, reserveOnly int) error {
	for i := range mounts {
		ws, ok := m.Checkouts.Checkout(mounts[i].Name)
		if !ok {
			return fmt.Errorf("repo %q is not registered", mounts[i].Name)
		}
		if i < reserveOnly {
			t.SetRepoBranch(i, ws.ReserveBranchName())
			continue
		}
		branch, err := ws.AllocateBranch(ctx, m.log)
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

func resolveRuntimeName(router *runtime.Router, id runtime.Name) (runtime.Name, error) {
	if id == "" {
		return router.Runtimes[0].Name(), nil
	}
	if _, ok := router.ByName[id]; !ok {
		return "", badRequestf("unknown runtime: %s", id)
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

func statsStateActive(st task.State) bool {
	switch st {
	case task.StatePurged, task.StateFailed, task.StateCrashed, task.StateStopped, task.StateStopping:
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
	prevState, changed := t.SetStateUnless(task.StateStopped,
		task.StatePurged, task.StateCrashed, task.StateFailed, task.StateStopped,
		task.StateStopping, task.StatePurging)
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
	if _, changed := t.SetStateIfAny(task.StateWaiting, task.StateStopped); !changed {
		return
	}
	entry.Lifecycle.reconnectImportedSession()
	m.NotifyTaskChange()
}

// insertEntry adds an entry under m.mu and signals the change.
func (m *Manager) insertEntry(id string, entry *Entry) {
	m.mu.Lock()
	m.tasks[id] = entry
	m.taskChanged()
	m.mu.Unlock()
	m.watchRateLimitEvents(entry.Task())
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

// taskChanged closes the current changed channel and replaces it.
// Must be called while holding m.mu.
func (m *Manager) taskChanged() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// resolveCheckout returns the checkout for a task's primary repo, if any.
func (m *Manager) resolveCheckout(t *task.Task) *repo.Checkout {
	if p := t.Primary(); p != nil {
		r, _ := m.Checkouts.Checkout(p.Name)
		return r
	}
	return nil
}

func applyLoadedSessionMetadata(t *task.Task, lt *task.LoadedTask) {
	if lt == nil {
		return
	}
	sessionID := lt.SessionID
	if t.GetSessionID() != "" {
		sessionID = ""
	}
	t.SetSessionMetadata(sessionID, lt.Model, lt.AgentVersion)
}

// mergeLogAndRelayMessages joins durable log history with the relay tail read
// during import.
//
// The durable log contains the full pre-restart history. The relay tail contains
// output produced while caic was down, plus some overlapping history. Pi replay
// can reconstruct the same logical messages with small metadata differences, so
// overlap matching uses semantic equivalence instead of strict equality.
func mergeLogAndRelayMessages(logMsgs, relayMsgs []agent.Message) []agent.Message {
	return newLogRelayMessageMerger(logMsgs).merge(relayMsgs)
}

// importInstance investigates a single runtime instance and registers it as a task.
func (m *Manager) importInstance(ctx context.Context, checkout *repo.Checkout, c *runtime.Instance, branch, taskIDVal string, metadataResolved bool, branchIDs map[string][]string, allLogs []*task.LoadedTask) (*Entry, error) {
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

	isExited := c.State == "exited"
	if isExited {
		m.log.InfoContext(ctx, "instance", "msg", "importing exited instance as stopped", "instance", c.ID, "br", branch)
	}

	// Find the log file for this task.
	var matchingLogs []*task.LoadedTask
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
	lt.SetNativeParserResolver(m.resolveNativeParser)
	// Check relay liveness.
	var relayAlive bool
	var relayRecords []agent.ParsedMessage
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
		relayRecords, relaySize, relayErr = m.relay.ReadTail(readCtx, relayTarget, parser, 10<<20) // 10 MiB tail
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
		if err := lt.LoadSessionMetadataWithResolver(m.resolveNativeParser); err != nil {
			m.log.WarnContext(ctx, "load session metadata failed", "repo", relPath, "br", branch, "err", err)
		}
	}

	var mounts []task.RepoMount
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
		mounts = []task.RepoMount{{Name: relPath, BaseBranch: primaryBaseBranch, GitRoot: checkout.Dir, Branch: branch, ContainerPath: containerPath}}
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
			mounts = append(mounts, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, ContainerPath: containerPath})
		}
	}

	forgeIssue := lt.ForgeIssue
	rt := m.Runtimes.Runtimes[0].Name()
	var forkedFromTaskID ksid.ID
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
	if c.ID.RuntimeName() != "" {
		rt = c.ID.RuntimeName()
	}

	t := &task.Task{
		ID:                taskID,
		InitialPrompt:     agent.Prompt{Text: prompt},
		Repos:             mounts,
		Harness:           lt.Harness,
		Model:             lt.Model,
		Effort:            lt.Effort,
		RuntimeName:       rt,
		BaseImage:         lt.BaseImage,
		ContainerPlatform: lt.ContainerPlatform,
		MaxCPUs:           lt.MaxCPUs,
		CacheMounts:       lt.CacheMounts,
		Mounts:            lt.Mounts,
		StartedAt:         startedAt,
		ForkedFromTaskID:  forkedFromTaskID,
		Tailscale:         c.Tailscale,
		TailscaleFQDN:     c.TailscaleFQDN,
		USB:               c.USB,
		Display:           c.Display,
		Sudo:              c.Sudo,
		VNCPort:           c.VNCPort,
		Provider:          m.provider,
		ForgeIssue:        forgeIssue,
	}
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
	if lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
	}
	t.SetLogPath(lt.LogPath())
	t.SetLogValidationSnapshot(lt.ValidatedSnapshot())
	if relaySnapshotRead {
		t.SetRelayOffset(relaySize)
	}

	switch {
	case lt.ForgePR > 0:
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
	case forgeIssue > 0 && checkout != nil && checkout.Repository != nil:
		t.SetPR(checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, 0)
	}

	// Restore messages from both the local log and the relay tail. The local log
	// has the full pre-restart history; the relay tail has any output produced
	// while the server was down. Merge them by overlap so the UI does not collapse
	// to the bounded relay tail after a server restart. Fail closed: a malformed
	// persistent history must not attach a live task with untrusted state.
	if err := lt.LoadMessagesWithResolver(m.resolveNativeParser); err != nil {
		return nil, fmt.Errorf("load messages for imported task %s: %w", taskID, err)
	}
	t.SetLogValidationSnapshot(lt.ValidatedSnapshot())
	if len(relayRecords) > 0 {
		relayMsgs := make([]agent.Message, len(relayRecords))
		for i, parsed := range relayRecords {
			relayMsgs[i] = parsed.Message
		}
		msgs := relayMsgs
		if len(lt.Msgs) > 0 {
			msgs = mergeLogAndRelayMessages(lt.Msgs, relayMsgs)
		}
		t.RestoreMessages(msgs)
		applyLoadedSessionMetadata(t, lt)
		m.log.DebugContext(ctx, "relay", "msg", "restored from", "repo", relPath, "br", branch, "instance", c.ID, "alive", relayAlive, "msgs", len(msgs), "relayMsgs", len(relayRecords))
	} else if len(lt.Msgs) > 0 {
		t.RestoreMessages(lt.Msgs)
		applyLoadedSessionMetadata(t, lt)
		m.log.WarnContext(ctx, "relay", "msg", "restored from log", "repo", relPath, "br", branch, "instance", c.ID, "msgs", len(lt.Msgs))
	}
	applyLoadedSessionMetadata(t, lt)
	// Restore the persisted diff signal. RestoreMessages recomputes diffCreated
	// from replayed history, but that replay is skipped when neither the relay
	// tail nor the log messages load; the summary flag (scanned from the log)
	// carries it through regardless. Sticky: only ever set, never cleared.
	if lt.DiffCreated {
		t.MarkDiffCreated()
	}
	t.SetStateAt(t.GetState(), stateUpdatedAt)
	if lt.State == task.StateFailed {
		t.SetStateAt(lt.State, stateUpdatedAt)
	}
	if lt.State == task.StateCrashed && t.LastExitError() != "" {
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
		if t.GetState() != task.StateCrashed {
			if t.LastExitError() != "" {
				t.RecordSessionCrash(ctx, errors.New("agent subprocess exited before import"))
			} else {
				t.SetState(task.StateStopped)
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
		} else if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			m.log.WarnContext(ctx, "relay", "msg", "dead, marking waiting",
				"repo", relPath, "br", branch, "instance", c.ID,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entry := m.NewEntry(t, lt)
	if t.GetState() == task.StateCrashed || t.GetState() == task.StateFailed {
		resultErr := errors.New("agent session failed")
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
		if err := m.Logs.WriteTaskResultTrailer(t, result); err != nil {
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
	m.taskChanged()
	m.mu.Unlock()
	m.watchRateLimitEvents(t)

	m.log.InfoContext(ctx, "instance", "msg", "imported",
		"repo", relPath, "instance", c.ID, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed.
	if needsTitleRegen(t, lt, m.resolveNativeParser) {
		entry.Lifecycle.generateTitle()
	}

	// Auto-reconnect immediately so imported live tasks can accept input as
	// soon as startup returns. EnsureSession may still replace an already-exited
	// attach in the background, but the attach itself must not race the first
	// user reply after restart.
	if t.GetState() != task.StateStopped && relayAlive {
		entry.Lifecycle.reconnectImportedSession() //nolint:contextcheck // imported watcher uses the Manager lifetime.
	} else if !relayAlive && t.GetState() != task.StateStopped && t.GetState() != task.StateCrashed && t.GetState() != task.StateFailed {
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

// resolveNativeParser constructs one fresh native parser for a validated task
// log harness.
func (m *Manager) resolveNativeParser(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
	backend := m.Backends[h]
	if backend == nil {
		return nil, fmt.Errorf("unknown harness %q", h)
	}
	return backend.NewWire().ParseMessage, nil
}

// loadTaskMessagesOnDemand triggers lazy message loading for purged tasks.
func (m *Manager) loadTaskMessagesOnDemand(entry *Entry) {
	entry.LoadMessagesOnce(func() {
		lt := entry.LoadedTask()
		if err := lt.LoadMessagesTailWithResolver(m.resolveNativeParser); err != nil {
			m.log.Warn("lazy load messages failed", "task", entry.Task().ID, "err", err)
			return
		}
		entry.Task().RestoreMessages(lt.Msgs)
	})
}

// logRelayMessageMerger owns the import-time overlap rules for disk-log
// messages and relay-tail messages.
type logRelayMessageMerger struct {
	logMsgs    []agent.Message
	logHasInit bool
}

func newLogRelayMessageMerger(logMsgs []agent.Message) *logRelayMessageMerger {
	return &logRelayMessageMerger{
		logMsgs:    logMsgs,
		logHasInit: messagesContainInit(logMsgs),
	}
}

func (m *logRelayMessageMerger) merge(relayMsgs []agent.Message) []agent.Message {
	relayMsgs = m.comparableRelayMessages(relayMsgs)
	if len(m.logMsgs) == 0 {
		return append([]agent.Message(nil), relayMsgs...)
	}
	if len(relayMsgs) == 0 {
		return append([]agent.Message(nil), m.logMsgs...)
	}
	maxOverlap := min(len(m.logMsgs), len(relayMsgs))
	for n := maxOverlap; n > 0; n-- {
		if m.messagesEqual(m.logMsgs[len(m.logMsgs)-n:], relayMsgs[:n]) {
			merged := append([]agent.Message(nil), m.logMsgs...)
			return append(merged, relayMsgs[n:]...)
		}
	}
	merged := append([]agent.Message(nil), m.logMsgs...)
	return append(merged, relayMsgs...)
}

// comparableRelayMessages drops relay-only metadata before overlap matching.
//
// The returned slice is either the original relayMsgs slice or a filtered copy.
func (m *logRelayMessageMerger) comparableRelayMessages(relayMsgs []agent.Message) []agent.Message {
	for i, msg := range relayMsgs {
		if !m.relayMessageComparableToLog(msg) {
			out := append([]agent.Message(nil), relayMsgs[:i]...)
			for _, msg := range relayMsgs[i+1:] {
				if m.relayMessageComparableToLog(msg) {
					out = append(out, msg)
				}
			}
			return out
		}
	}
	return relayMsgs
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
		// ignore the relay one for overlap matching.
		return !m.logHasInit
	default:
		return true
	}
}

func (m *logRelayMessageMerger) messagesEqual(a, b []agent.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !m.messagesEquivalent(a[i], b[i]) {
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
		return ok && av.Usage == bv.Usage && av.Model == bv.Model
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
		return reflect.DeepEqual(&aa, &bb)
	default:
		return reflect.DeepEqual(a, b)
	}
}

func messagesContainInit(msgs []agent.Message) bool {
	for _, msg := range msgs {
		if _, ok := msg.(*agent.InitMessage); ok {
			return true
		}
	}
	return false
}
