// Package taskmgr orchestrates task lifecycle management: creation, execution,
// session watching, stats streaming, and instance adoption.
//
// It sits between the HTTP adapter (internal/server) and the domain layer
// (internal/task): task.Task / repowork.Workspace are domain types, while
// taskmgr.Manager is the orchestration layer built on top of them.
//
// Two contexts coexist here. Methods accept a request-scoped ctx that is
// honored for synchronous work (state checks, logging). Background goroutines
// that must outlive any single request (session watchers, stats poller, fire-
// and-forget title generation) use serverCtx instead — it tracks the lifetime
// of the Manager itself.
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
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
)

// errTaskNotFound is returned when a task ID doesn't exist.
var errTaskNotFound = &Error{Kind: KindNotFound, Msg: "task not found"}

type relayReader interface {
	Status(ctx context.Context, target runtime.ConnectionTarget) (bool, string, error)
	ReadTail(ctx context.Context, target runtime.ConnectionTarget, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) ([]agent.Message, int64, error)
	ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string
}

type agentRelayReader struct{}

func (agentRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	if target.SSHHost == "" {
		return false, "", errors.New("agent connection target missing SSH host")
	}
	return agent.RelayStatus(ctx, target.SSHHost)
}

func (agentRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) (msgs []agent.Message, size int64, err error) {
	if target.SSHHost == "" {
		return nil, 0, errors.New("agent connection target missing SSH host")
	}
	return agent.ReadRelayTail(ctx, target.SSHHost, parseFn, maxBytes)
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
	LogDir    string
	CacheDir  string
	// Runtimes validates runtime selection and dispatches task runtime operations.
	Runtimes            *runtime.Router
	Backends            map[harness.Name]agent.Backend
	EventReplayFactory  func(logPath string) (task.EventReplayWriter, error)
	HarnessEnv          map[string][]string
	RuntimeStartTimeout time.Duration
	Prefs               *preferences.Store
	Provider            genai.Provider // nil-safe
	WorkspaceRegistry   *repowork.Registry
	QuotaTracker        *quotausage.Tracker
}

// Manager owns task lifecycle state, instance adoption, session watching, and
// stats streaming.
type Manager struct {
	Runtimes *runtime.Router
	// QuotaTracker holds canonical quota updates received from tasks.
	QuotaTracker *quotausage.Tracker

	// Immutable.
	log                 *slog.Logger
	serverCtx           context.Context // lifetime of the Manager; for goroutines that outlive requests
	cancelServerCtx     context.CancelFunc
	logDir              string
	cacheDir            string
	backends            map[harness.Name]agent.Backend
	eventReplayFactory  func(logPath string) (task.EventReplayWriter, error)
	harnessEnv          map[string][]string
	runtimeStartTimeout time.Duration
	prefs               *preferences.Store
	provider            genai.Provider
	workspaceRegistry   *repowork.Registry
	relay               relayReader

	// Guarded by quotaWatchMu.
	quotaWatchMu     sync.Mutex
	quotaWatchers    sync.WaitGroup
	quotaWatchClosed bool

	// Guarded by mu.
	mu      sync.Mutex
	tasks   map[string]*Entry
	changed chan struct{} // closed on mutation, replaced under mu
}

// New creates a Manager. A no-repo workspace is always registered.
// Register each repo workspace in the configured WorkspaceRegistry, then Start.
func New(cfg Config) *Manager { //nolint:gocritic // Config is a value bag passed once at construction
	log := slog.Default().With(slog.String("cmp", "taskmgr"))
	serverCtx, cancelServerCtx := context.WithCancel(cfg.ServerCtx)
	workspaceRegistry := cfg.WorkspaceRegistry
	if workspaceRegistry == nil {
		workspaceRegistry = repowork.NewRegistry(cfg.ServerCtx, cfg.Runtimes)
	}
	quotaTracker := cfg.QuotaTracker
	if quotaTracker == nil {
		quotaTracker = quotausage.NewTracker()
	}
	m := &Manager{
		log:                 log,
		Runtimes:            cfg.Runtimes,
		QuotaTracker:        quotaTracker,
		serverCtx:           serverCtx,
		cancelServerCtx:     cancelServerCtx,
		logDir:              cfg.LogDir,
		cacheDir:            cfg.CacheDir,
		backends:            maps.Clone(cfg.Backends),
		eventReplayFactory:  cfg.EventReplayFactory,
		harnessEnv:          cfg.HarnessEnv,
		runtimeStartTimeout: managerRuntimeStartTimeout(cfg.RuntimeStartTimeout),
		prefs:               cfg.Prefs,
		provider:            cfg.Provider,
		workspaceRegistry:   workspaceRegistry,
		relay:               agentRelayReader{},
		tasks:               make(map[string]*Entry),
		changed:             make(chan struct{}),
	}
	if _, ok := workspaceRegistry.Workspace(""); !ok {
		workspaceRegistry.RegisterWorkspace("", &repowork.Workspace{
			GitTimeout: time.Minute,
			Runtimes:   cfg.Runtimes,
			Log:        m.log.With(slog.String("repo", "(none)")),
		})
	}
	return m
}

// Close stops quota-event watchers and waits for their live task subscriptions
// to close. It currently always returns nil.
func (m *Manager) Close() error {
	m.quotaWatchMu.Lock()
	m.quotaWatchClosed = true
	m.cancelServerCtx()
	m.quotaWatchMu.Unlock()
	m.quotaWatchers.Wait()
	return nil
}

// Start launches background goroutines: instance event watching and stats streaming.
// Must be called once after New, after workspaces have been registered.
func (m *Manager) Start() {
	go m.watchRuntimeEvents(m.serverCtx)
	go m.watchStats(m.serverCtx)
}

// RegisterWorkspace registers a task repo workspace keyed by relPath.
// "" registers the no-repo workspace.
func (m *Manager) RegisterWorkspace(relPath string, r *repowork.Workspace) {
	m.workspaceRegistry.RegisterWorkspace(relPath, r)
}

// Workspace returns the repo workspace for relPath, or nil.
func (m *Manager) Workspace(relPath string) (*repowork.Workspace, bool) {
	return m.workspaceRegistry.Workspace(relPath)
}

// RangeWorkspaces iterates over every registered repo workspace. It snapshots
// the registry and invokes fn unlocked, so fn may safely call back into the
// Manager. The workspace set is a point-in-time snapshot. Stops iteration if fn
// returns false.
func (m *Manager) RangeWorkspaces(fn func(relPath string, r *repowork.Workspace) bool) {
	m.workspaceRegistry.RangeWorkspaces(fn)
}

// UnregisterWorkspace removes the repo workspace registered for relPath.
func (m *Manager) UnregisterWorkspace(relPath string) {
	m.workspaceRegistry.UnregisterWorkspace(relPath)
}

// Backends returns a copy of the configured agent backend map.
func (m *Manager) Backends() map[harness.Name]agent.Backend {
	return maps.Clone(m.backends)
}

// Insert registers a pre-built entry. Production task creation goes through
// Create/Fork; Insert is retained for tests (in internal/tasks and
// internal/server) that seed the registry without a real workspace.
func (m *Manager) Insert(id string, entry *Entry) {
	m.insertEntry(id, entry)
}

// Range iterates over every registered entry. It snapshots the registry under
// m.mu and invokes fn unlocked, so fn may safely call back into the Manager
// (e.g. RepoWorkspace). The entry set is a point-in-time snapshot; Entry pointers are
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
	// Resolve primary workspace.
	var primaryWorkspace *repowork.Workspace
	if len(p.Repos) > 0 {
		r, ok := m.Workspace(p.Repos[0].Name)
		if !ok {
			return "", badRequestf("unknown repo: %s", p.Repos[0].Name)
		}
		primaryWorkspace = r
	} else {
		// New() always registers the no-repo "" workspace, so this never fails.
		primaryWorkspace, _ = m.Workspace("")
	}

	// Validate that every extra repo has a registered workspace (branches are
	// allocated later, in one pass, by allocateBranches).
	for _, rs := range p.Repos[min(1, len(p.Repos)):] {
		if _, ok := m.Workspace(rs.Name); !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
	}

	runtimeName, err := resolveRuntimeName(m.Runtimes, p.RuntimeName)
	if err != nil {
		return "", err
	}

	backend, ok := m.backends[p.Harness]
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
		r, _ := m.Workspace(rs.Name)
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
	go t.GenerateTitle(m.serverCtx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := NewEntry(t, nil)

	m.insertEntry(t.ID.String(), entry)

	// Run in background using server context.
	go func() {
		// The primary's branch is created by the runner concurrently with instance
		// launch, so it only needs a name reserved; extras are created here.
		if err := m.allocateBranches(m.serverCtx, t, mounts, 1); err != nil {
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "allocate branch")})
			m.NotifyTaskChange()
			return
		}

		ghToken := p.ResolvedGitHubToken

		h, err := m.runner(primaryWorkspace).Start(m.serverCtx, t, ghToken)
		if err != nil {
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "start task")})
			m.NotifyTaskChange()
			return
		}
		if t.Sudo {
			t.SetSudoPassword(m.SudoPassword(m.serverCtx, t))
		}
		m.NotifyTaskChange()
		m.watchSession(entry, primaryWorkspace, h)
	}()
	return t.ID.String(), nil
}

// Purge transitions a task to purging.
func (m *Manager) Purge(ctx context.Context, entry *Entry) error {
	state, changed := entry.task.SetStateIfAny(task.StatePurging,
		task.StateWaiting, task.StateAsking, task.StateHasPlan,
		task.StateRunning, task.StateStopping, task.StateStopped, task.StateCrashed)
	if !changed {
		return conflict("task is not running, waiting, stopped, or crashed")
	}
	m.NotifyTaskChange()
	workspace := m.resolveWorkspace(entry.task)
	m.log.InfoContext(ctx, "purge requested", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "state", state)
	go func() {
		m.cleanupTask(entry, workspace, task.StatePurged)
		m.log.InfoContext(m.serverCtx, "purge completed", "task", entry.task.ID, "final_state", entry.task.GetState())
	}()
	return nil
}

// Stop transitions a task to stopping.
func (m *Manager) Stop(ctx context.Context, entry *Entry) error {
	state, changed := entry.task.SetStateIfAny(task.StateStopping,
		task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateRunning)
	if !changed {
		return conflict("task is not running or waiting")
	}
	m.NotifyTaskChange()
	workspace := m.resolveWorkspace(entry.task)
	m.log.InfoContext(ctx, "stop requested", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "state", state)
	go func() {
		m.runner(workspace).StopTask(m.serverCtx, entry.task)
		m.log.InfoContext(m.serverCtx, "stop completed", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "final_state", entry.task.GetState())
		m.NotifyTaskChange()
	}()
	return nil
}

// Revive restarts a stopped or crashed task.
func (m *Manager) Revive(ctx context.Context, entry *Entry) error {
	if _, changed := entry.task.SetStateIfAny(task.StateProvisioning, task.StateStopped, task.StateCrashed); !changed {
		return conflict("task is not stopped or crashed")
	}
	workspace := m.resolveWorkspace(entry.task)
	entry.Reset()
	m.NotifyTaskChange()
	go func() { //nolint:contextcheck // background goroutine roots its own trace task on serverCtx
		ctx, tk := trace.NewTask(m.serverCtx, "task.revive:"+entry.task.ID.String())
		defer tk.End()
		h, err := m.runner(workspace).ReviveTask(ctx, entry.task)
		if err != nil {
			m.log.WarnContext(ctx, "revive failed", "task", entry.task.ID, "err", err)
			entry.task.SetState(task.StateFailed)
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "revive task")})
			m.NotifyTaskChange()
			return
		}
		m.NotifyTaskChange()
		m.watchSession(entry, workspace, h)
	}()
	return nil
}

// Restart restarts the agent session with a new prompt.
func (m *Manager) Restart(ctx context.Context, entry *Entry, prompt agent.Prompt) error {
	t := entry.task
	prevState, changed := t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan)
	if !changed {
		return conflict("task is not waiting or asking")
	}
	if prompt.Text == "" {
		// No prompt provided: fall back to the plan file from the instance.
		target := t.RuntimeConnectionTarget()
		sshHost := target.SSHHost
		var err error
		if sshHost != "" {
			var plan string
			plan, err = agent.ReadPlan(m.serverCtx, sshHost, t.GetPlanFile()) //nolint:contextcheck // intentionally using server context
			prompt.Text = plan
		} else {
			err = errors.New("agent connection target missing SSH host")
		}
		if err != nil {
			t.SetStateIf(task.StateStarting, prevState)
			return &Error{Kind: KindBadRequest, Msg: "no prompt provided and failed to read plan from instance", Err: err}
		}
	}
	workspace := m.resolveWorkspace(t)
	h, err := m.sessions(workspace).RestartSession(m.serverCtx, t, prompt) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return internalErr(err, "restart session")
	}
	m.watchSession(entry, workspace, h)
	m.NotifyTaskChange()
	return nil
}

// ClearContext clears the agent session context.
func (m *Manager) ClearContext(ctx context.Context, entry *Entry) error {
	t := entry.task
	if _, changed := t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan); !changed {
		return conflict("task is not waiting or asking")
	}
	workspace := m.resolveWorkspace(t)
	h, err := m.sessions(workspace).ClearContextSession(m.serverCtx, t) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return internalErr(err, "clear context")
	}
	m.watchSession(entry, workspace, h)
	m.NotifyTaskChange()
	return nil
}

// Compact compacts the agent session context.
func (m *Manager) Compact(ctx context.Context, entry *Entry, instructions string) error {
	if err := entry.task.SendCompact(ctx, instructions); err != nil {
		// SendCompact fails when there's no active session to compact; the
		// HTTP layer surfaces this as a 409 conflict.
		return conflictErr(err, "no active session to compact")
	}
	return nil
}

// SendInput forwards user input to the agent session.
func (m *Manager) SendInput(ctx context.Context, entry *Entry, prompt agent.Prompt) error {
	// Validate image support.
	if len(prompt.Images) > 0 {
		if b := m.backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return badRequestf("%s does not support images", string(entry.task.Harness))
		}
	}
	if err := entry.task.SendInput(ctx, prompt); err != nil {
		if !errors.Is(err, task.ErrNoActiveSession) {
			return conflictErr(err, "send input")
		}
		var failedReconnect error
		if reconnectErr := m.reconnectForInput(entry); reconnectErr == nil {
			if retryErr := entry.task.SendInput(ctx, prompt); retryErr == nil {
				return nil
			} else if !errors.Is(retryErr, task.ErrNoActiveSession) {
				return conflictErr(retryErr, "send input")
			} else {
				err = retryErr
			}
		} else {
			failedReconnect = reconnectErr
			t := entry.task
			m.log.WarnContext(ctx, "reconnect before send input failed",
				"task", t.ID,
				"instance", t.RuntimeInstanceID(),
				"state", t.GetState(),
				"err", reconnectErr,
			)
		}
		t := entry.task
		taskState := t.GetState()
		m.log.WarnContext(ctx, "no active session",
			"task", t.ID,
			"instance", t.RuntimeInstanceID(),
			"state", taskState,
		)
		// Wrap so the handler can detect the no-session condition via
		// errors.Is while preserving the task diagnostic and any reconnect
		// failure.
		return &NoSessionError{Err: err, ReconnectErr: failedReconnect}
	}
	return nil
}

// Fork creates a new task from a source task's retained instance.
func (m *Manager) Fork(ctx context.Context, sourceEntry *Entry, p ForkParams) (string, error) { //nolint:gocritic // ForkParams is a request-shaped value bag
	source := sourceEntry.task
	state := source.GetState()
	switch state {
	case task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateStopped, task.StateCrashed:
	default:
		return "", conflict("task must be active, stopped, or crashed to fork")
	}
	if source.RuntimeInstanceID() == "" {
		return "", conflict("task has no instance")
	}
	sourceRepos := source.ReposSnapshot()
	if len(sourceRepos) == 0 {
		return "", badRequestf("cannot fork a no-repo task")
	}

	workspace := m.resolveWorkspace(source)

	// Resolve harness and model.
	forkHarness := source.Harness
	forkModel := source.Model
	forkEffort := source.Effort
	if p.Harness != "" {
		forkHarness = p.Harness
		backend, ok := m.backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(p.Harness))
		}
		if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
			return "", badRequestf("unsupported model for %s: %s", string(p.Harness), p.Model)
		}
		forkModel = p.Model
		forkEffort = p.Effort
	} else if p.Model != "" {
		backend, ok := m.backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(source.Harness))
		}
		if !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
			return "", badRequestf("unsupported model for %s: %s", string(source.Harness), p.Model)
		}
		forkModel = p.Model
		forkEffort = p.Effort
	}

	// Build mounts.
	sourceRepoNames := make(map[string]struct{}, len(sourceRepos))
	for _, r := range sourceRepos {
		sourceRepoNames[r.Name] = struct{}{}
	}
	var extraMounts []task.RepoMount
	for _, rs := range p.ExtraRepos {
		if _, overlap := sourceRepoNames[rs.Name]; overlap {
			return "", badRequestf("extraRepos contains repo already in source task: %s", rs.Name)
		}
		er, ok := m.Workspace(rs.Name)
		if !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
		extraMounts = append(extraMounts, task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: er.Dir, ContainerPath: m.containerPathForRepo(rs.Name)})
	}

	mounts := make([]task.RepoMount, len(sourceRepos), len(sourceRepos)+len(extraMounts))
	copy(mounts, sourceRepos)
	mounts = append(mounts, extraMounts...)

	t := &task.Task{
		ID:                ksid.NewID(),
		InitialPrompt:     p.Prompt,
		Repos:             mounts,
		Harness:           forkHarness,
		Model:             forkModel,
		Effort:            forkEffort,
		RuntimeName:       source.RuntimeName,
		BaseImage:         source.BaseImage,
		ContainerPlatform: source.ContainerPlatform,
		MaxCPUs:           source.MaxCPUs,
		CacheMounts:       slices.Clone(source.CacheMounts),
		Mounts:            slices.Clone(source.Mounts),
		GitHubToken:       p.GitHubToken,
		Tailscale:         p.Tailscale,
		USB:               p.USB,
		Display:           p.Display,
		Sudo:              p.Sudo,
		StartedAt:         time.Now().UTC(),
		OwnerID:           p.OwnerID,
		ForkedFromTaskID:  source.ID,
		Provider:          m.provider,
	}
	t.SetTitle(p.Prompt.Text)
	go t.GenerateTitle(m.serverCtx) //nolint:contextcheck // fire-and-forget; must outlive request
	forkEntry := NewEntry(t, nil)

	m.insertEntry(t.ID.String(), forkEntry)

	go func() { //nolint:contextcheck // background goroutine roots its own trace task on serverCtx
		ctx, tk := trace.NewTask(m.serverCtx, "task.fork:"+source.ID.String()+"->"+t.ID.String())
		defer tk.End()

		if err := m.allocateBranches(ctx, t, mounts, len(sourceRepos)); err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "allocate fork branch")})
			m.NotifyTaskChange()
			return
		}

		ghToken := p.ResolvedGitHubToken

		var extraEnv []string
		if ghToken != "" {
			extraEnv = append(extraEnv, "GITHUB_TOKEN="+ghToken)
		}

		forkOpts := &runtime.ForkOptions{
			RuntimeName: source.RuntimeName,
			Display:     p.Display,
			Tailscale:   p.Tailscale,
			USB:         p.USB,
			Sudo:        p.Sudo,
			Metadata:    task.MakeMetadata(t),
			Harness:     forkHarness,
			ExtraEnv:    extraEnv,
			Mounts:      slices.Clone(source.Mounts),
			MaxCPUs:     source.MaxCPUs,
		}
		h, err := m.runner(workspace).ForkTask(ctx, source, t, forkOpts, ghToken)
		if err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "fork task")})
			m.NotifyTaskChange()
			return
		}
		if t.Sudo {
			t.SetSudoPassword(m.SudoPassword(m.serverCtx, t))
		}
		m.NotifyTaskChange()
		m.watchSession(forkEntry, workspace, h)
	}()
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
	if workspace, ok := m.Workspace(p.Name); ok {
		return workspace.BaseBranch
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

// AdoptInstances discovers preexisting runtime instances and creates task entries
// for them. Returns the list of adopted tasks so the caller (Server) can wire
// up forge/CI post-adoption.
func (m *Manager) AdoptInstances(ctx context.Context, adoptRepos []AdoptRepo, instances []runtime.Instance, allLogs []*task.LoadedTask) ([]AdoptedTask, error) {
	if instances == nil {
		return nil, nil
	}
	resolvedTaskIDs, rejected, validationErr := m.resolveAdoptionTaskIDs(ctx, adoptRepos, instances)

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
	var adopted []AdoptedTask
	claimed := make(map[runtime.ID]bool, len(instances))
	for id := range rejected {
		claimed[id] = true
	}

	for i := range adoptRepos {
		ri := &adoptRepos[i]
		workspace, _ := m.Workspace(ri.RelPath)
		if workspace == nil {
			continue
		}
		for i := range instances {
			c := &instances[i]
			if claimed[c.ID] {
				continue
			}
			branch, matched := primaryBranchForAdoption(ri, c)
			if !matched {
				continue
			}
			claimed[c.ID] = true
			taskIDVal, metadataResolved := resolvedTaskIDs[c.ID]
			wg.Go(func() {
				at, err := m.adoptOne(ctx, *ri, workspace, c, branch, taskIDVal, metadataResolved, branchIDs, allLogs)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				if at != nil {
					mu.Lock()
					adopted = append(adopted, *at)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()

	// Adopt no-repo runtime instances.
	if noRepoWorkspace, ok := m.Workspace(""); ok {
		for i := range instances {
			c := &instances[i]
			if claimed[c.ID] || !strings.HasPrefix(string(c.ID.InstanceID()), "md-agent-") {
				continue
			}
			taskIDVal, metadataResolved := resolvedTaskIDs[c.ID]
			wg.Go(func() {
				at, err := m.adoptOne(ctx, AdoptRepo{}, noRepoWorkspace, c, "", taskIDVal, metadataResolved, branchIDs, allLogs)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				if at != nil {
					mu.Lock()
					adopted = append(adopted, *at)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}

	return adopted, errors.Join(errs...)
}

func primaryBranchForAdoption(ri *AdoptRepo, c *runtime.Instance) (string, bool) {
	if len(c.Repos) == 0 {
		return "", false
	}
	r := c.Repos[0]
	if !containerPathMatchesRepo(r.ContainerPath, ri) {
		return "", false
	}
	return r.Branch, true
}

func containerPathMatchesRepo(containerPath string, ri *AdoptRepo) bool {
	relPath := filepath.ToSlash(ri.RelPath)
	baseName := filepath.Base(ri.AbsPath)
	return slices.Contains([]string{
		"/home/user/src/" + relPath,
		"~/src/" + relPath,
		relPath,
		"/home/user/src/" + baseName,
		"~/src/" + baseName,
		baseName,
	}, containerPath)
}

// needsTitleRegen reports whether an adopted task needs an LLM title regeneration.
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
func (m *Manager) FindTasksMonitoringBranch(owner, repo string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		if e.MonitorBranch() == "" {
			continue
		}
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repo {
			if p := e.task.Primary(); p != nil && p.Branch == e.MonitorBranch() {
				out = append(out, e)
			}
		}
	}
	return out
}

// FindTasksByPR returns all entries matching the given forge owner/repo and PR number.
func (m *Manager) FindTasksByPR(owner, repo string, prNumber int) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repo && snap.ForgePR == prNumber {
			out = append(out, e)
		}
	}
	return out
}

// FindTasksMatchingBranch returns all entries for a forge owner/repo where the
// primary repo branch matches the given branch.
func (m *Manager) FindTasksMatchingBranch(owner, repo, branch string) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.tasks {
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repo {
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
		rt := m.defaultRuntimeName()
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
		m.tasks[t.ID.String()] = newPurgedEntry(t, lt.Result, lt)
		loaded++
	}
	if loaded > 0 {
		m.taskChanged()
	}
	m.log.Info("loaded purged tasks from logs", "n", loaded, "candidates", len(purged))
	return nil
}

// Cleanup is the exported variant of cleanupTask, idempotent per incarnation.
func (m *Manager) Cleanup(entry *Entry, workspace *repowork.Workspace, reason task.State) {
	m.cleanupTask(entry, workspace, reason)
}

// LoadMessagesOnDemand triggers lazy message loading for purged tasks.
func (m *Manager) LoadMessagesOnDemand(entry *Entry) {
	m.loadTaskMessagesOnDemand(entry)
}

// Sync performs git push operations. It does NOT start the PR flow.
func (m *Manager) Sync(ctx context.Context, entry *Entry, target SyncTarget, force bool) (*SyncResult, error) {
	t := entry.task
	switch t.GetState() { //nolint:exhaustive // only terminal/blocked states are relevant
	case task.StatePending:
		return nil, conflict("task has no instance yet")
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateCrashed, task.StateFailed, task.StatePurged:
		return nil, conflict("task is in a terminal state")
	}

	workspace := m.resolveWorkspace(t)
	syncPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		syncPrimaryBranch = p.Branch
	}

	if target == SyncTargetDefault {
		if force {
			return nil, badRequestf("force is not supported for default-branch sync")
		}
		baseBranch := workspace.BaseBranch
		message := t.Title()
		if message == "" {
			message = t.InitialPrompt.Text
		}
		ds, issues, err := workspace.SyncToDefault(ctx, t, message)
		if err != nil {
			return nil, internalErr(err, "sync to default")
		}
		status := "synced"
		if len(ds) == 0 {
			status = "empty"
		} else if len(issues) > 0 {
			status = "blocked"
		}
		return &SyncResult{Status: status, Branch: baseBranch, DiffStat: ds, SafetyIssues: issues}, nil
	}

	// Default: push to the task's own branch.
	ds, issues, err := workspace.SyncToOrigin(ctx, t, force)
	if err != nil {
		return nil, internalErr(err, "sync to origin")
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !force {
		status = "blocked"
	}
	return &SyncResult{Status: status, Branch: syncPrimaryBranch, DiffStat: ds, SafetyIssues: issues}, nil
}

// resolveAdoptionTaskIDs selects instances for configured workspaces, reads
// their task IDs, and rejects candidates with unavailable or duplicate IDs.
func (m *Manager) resolveAdoptionTaskIDs(ctx context.Context, adoptRepos []AdoptRepo, instances []runtime.Instance) (resolved map[runtime.ID]string, rejected map[runtime.ID]bool, retErr error) {
	candidates := make(map[runtime.ID]bool, len(instances))
	for i := range adoptRepos {
		ri := &adoptRepos[i]
		workspace, _ := m.Workspace(ri.RelPath)
		if workspace == nil {
			continue
		}
		for j := range instances {
			if _, matched := primaryBranchForAdoption(ri, &instances[j]); matched {
				candidates[instances[j].ID] = true
			}
		}
	}
	if noRepoWorkspace, ok := m.Workspace(""); ok && noRepoWorkspace != nil {
		for i := range instances {
			if strings.HasPrefix(string(instances[i].ID.InstanceID()), "md-agent-") {
				candidates[instances[i].ID] = true
			}
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

func (m *Manager) reconnectForInput(entry *Entry) error {
	t := entry.task
	if t.HasSession() {
		return nil
	}
	workspace := m.resolveWorkspace(t)
	h, err := m.sessions(workspace).Reconnect(m.serverCtx, t, false)
	if err != nil {
		if t.HasSession() {
			return nil
		}
		return err
	}
	tlog := m.log.With("task", t.ID, "instance", t.RuntimeInstanceID())
	h, err = m.sessions(workspace).EnsureSession(m.serverCtx, t, h, tlog)
	if err != nil {
		return err
	}
	m.watchSession(entry, workspace, h)
	return nil
}

func (m *Manager) logStore() *task.LogStore {
	return &task.LogStore{LogDir: m.logDir, EventReplayFactory: m.eventReplayFactory}
}

func (m *Manager) sessions(r *repowork.Workspace) *task.SessionRunner {
	return &task.SessionRunner{
		Backends:         m.backends,
		Logs:             m.logStore(),
		Workspace:        r,
		NotifyTaskChange: m.NotifyTaskChange,
	}
}

func (m *Manager) runner(r *repowork.Workspace) *task.Runner {
	return &task.Runner{Workspace: r, Sessions: m.sessions(r), RuntimeStartTimeout: m.runtimeStartTimeout}
}

func managerRuntimeStartTimeout(d time.Duration) time.Duration {
	if d != 0 {
		return d
	}
	return time.Hour
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
	m.RangeWorkspaces(func(other string, _ *repowork.Workspace) bool {
		if other != "" && other != relPath && filepath.Base(other) == base {
			collides = true
			return false
		}
		return true
	})
	return collides
}

// allocateBranches assigns every repo of a task its own branch name, uniformly —
// the Manager owns branch-name allocation for all repos, with no special case for
// the primary. mounts[:reserveOnly] are repos whose branch is created elsewhere
// (by md.Fork for a fork's source repos, or by the runner concurrently with
// launch for a fresh task's primary), so they only need a name reserved.
// mounts[reserveOnly:] are new to the host, so their branch is created here from
// their own workspace.
func (m *Manager) allocateBranches(ctx context.Context, t *task.Task, mounts []task.RepoMount, reserveOnly int) error {
	for i := range mounts {
		ws, ok := m.Workspace(mounts[i].Name)
		if !ok {
			return fmt.Errorf("repo %q is not registered", mounts[i].Name)
		}
		if i < reserveOnly {
			t.SetRepoBranch(i, ws.ReserveBranchName())
			continue
		}
		branch, err := ws.AllocateBranch(ctx)
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

func (m *Manager) defaultRuntimeName() runtime.Name {
	return m.Runtimes.Runtimes[0].Name()
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

// watchRuntimeEvents listens for runtime instance exit events and triggers
// cleanup for the corresponding task.
func (m *Manager) watchRuntimeEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := m.Runtimes.WatchEvents(ctx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
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
			for ev := range ch {
				m.handleRuntimeInstanceExit(ev.InstanceID)
			}
			if ctx.Err() != nil {
				return
			}
			m.log.WarnContext(ctx, "runtime events stream ended, reconnecting in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// handleRuntimeInstanceExit looks up a task by runtime instance name and archives it.
func (m *Manager) handleRuntimeInstanceExit(instanceID runtime.ID) {
	m.mu.Lock()
	var found *Entry
	for _, e := range m.tasks {
		if e.task.RuntimeInstanceID() != instanceID {
			continue
		}
		found = e
		break
	}
	m.mu.Unlock()
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
	m.log.Info("instance", "msg", "died, archiving as stopped", "instance", instanceID, "task", t.ID, "br", deathBranch, "prev_state", prevState)
	t.DetachSession()
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

// resolveWorkspace returns the repo workspace for a task's primary repo, or the
// always-present no-repo workspace. Both are guaranteed non-nil: New()
// registers "", and callers must register repo-specific workspaces.
func (m *Manager) resolveWorkspace(t *task.Task) *repowork.Workspace {
	key := ""
	if p := t.Primary(); p != nil {
		key = p.Name
	}
	r, _ := m.Workspace(key)
	return r
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
// during adoption.
//
// The durable log contains the full pre-restart history. The relay tail contains
// output produced while caic was down, plus some overlapping history. Pi replay
// can reconstruct the same logical messages with small metadata differences, so
// overlap matching uses semantic equivalence instead of strict equality.
func mergeLogAndRelayMessages(logMsgs, relayMsgs []agent.Message) []agent.Message {
	return newLogRelayMessageMerger(logMsgs).merge(relayMsgs)
}

// adoptOne investigates a single runtime instance and registers it as a task.
func (m *Manager) adoptOne(ctx context.Context, ri AdoptRepo, workspace *repowork.Workspace, c *runtime.Instance, branch, taskIDVal string, metadataResolved bool, branchIDs map[string][]string, allLogs []*task.LoadedTask) (*AdoptedTask, error) { //nolint:gocritic // massive function from existing code; refactor deferred
	ctx, adoptTask := trace.NewTask(ctx, "adopt-instance")
	defer adoptTask.End()
	trace.Logf(ctx, "instance", "%s repo=%s branch=%s", c.ID, ri.RelPath, branch)

	// Only adopt runtime instances that caic started. MetadataTaskID is set at
	// creation and is the authoritative proof of ownership.
	if !metadataResolved {
		var err error
		taskIDVal, err = m.runtimeTaskID(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("metadata check for %s: %w", c.ID, err)
		}
	}
	if taskIDVal == "" {
		m.log.InfoContext(ctx, "instance", "msg", "skipping non-caic", "repo", ri.RelPath, "instance", c.ID, "br", branch)
		return nil, nil //nolint:nilnil // non-caic runtime instances are intentionally skipped
	}
	taskID, err := ksid.Parse(taskIDVal)
	if err != nil {
		return nil, fmt.Errorf("parse caic metadata %q on %s: %w", taskIDVal, c.ID, err)
	}

	isExited := c.State == "exited"
	if isExited {
		m.log.InfoContext(ctx, "instance", "msg", "adopting exited instance as stopped", "instance", c.ID, "br", branch)
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
	if branch == "" && ri.RelPath == "" {
		if lp != nil {
			return nil, fmt.Errorf("task log %s has repo %q but runtime has no repo", taskID, lp.Name)
		}
	} else if lp == nil || lp.Name != ri.RelPath || lp.Branch != branch {
		return nil, fmt.Errorf("task log %s does not match runtime repo %q branch %q", taskID, ri.RelPath, branch)
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
	backend, ok := m.backends[lt.Harness]
	if !ok || backend == nil {
		return nil, fmt.Errorf("unknown harness %q for adopted task %s", lt.Harness, taskID)
	}
	lt.SetNativeParserResolver(m.resolveNativeParser)
	// Check relay liveness.
	var relayAlive bool
	var relayMsgs []agent.Message
	var relaySize int64
	var relayDiag string
	relayTarget := c.AgentTarget
	if !isExited {
		var relayErr error
		relayAlive, relayDiag, relayErr = m.relay.Status(ctx, relayTarget)
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", relayErr, "diag", relayDiag)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
		relayMsgs, relaySize, relayErr = m.relay.ReadTail(readCtx, relayTarget, backend.NewWire().ParseMessage, 10<<20) // 10 MiB tail
		readCancel()
		if relayErr != nil {
			m.log.WarnContext(ctx, "relay", "msg", "read output failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", relayErr)
			relayAlive = false
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
			m.log.WarnContext(ctx, "load session metadata failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
	}

	var adoptRepos []task.RepoMount
	if ri.RelPath != "" {
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
			containerPath = m.containerPathForRepo(ri.RelPath)
		}
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, BaseBranch: primaryBaseBranch, GitRoot: ri.AbsPath, Branch: branch, ContainerPath: containerPath}}
		// Build lookup of instance repos by branch for ContainerPath.
		runtimeRepoByBranch := make(map[string]string, len(c.Repos))
		for _, cr := range c.Repos {
			runtimeRepoByBranch[cr.Branch] = cr.ContainerPath
		}
		for _, lm := range lt.Repos[1:] {
			gitRoot := ""
			if er, ok := m.Workspace(lm.Name); ok {
				gitRoot = er.Dir
			}
			containerPath := runtimeRepoByBranch[lm.Branch]
			if containerPath == "" {
				containerPath = m.containerPathForRepo(lm.Name)
			}
			adoptRepos = append(adoptRepos, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, ContainerPath: containerPath})
		}
	}

	forgeIssue := lt.ForgeIssue
	rt := m.defaultRuntimeName()
	var forkedFromTaskID ksid.ID
	if lt.RuntimeName != "" {
		rt = lt.RuntimeName
	} else {
		lt.RuntimeName = rt
	}
	if lt.ForkedFromTaskID != "" {
		forkedFromTaskID, err = ksid.Parse(lt.ForkedFromTaskID)
		if err != nil {
			return nil, fmt.Errorf("adopt task %q: invalid forkedFromTaskID %q: %w", taskID.String(), lt.ForkedFromTaskID, err)
		}
	}
	if c.ID.RuntimeName() != "" {
		rt = c.ID.RuntimeName()
	}

	t := &task.Task{
		ID:                taskID,
		InitialPrompt:     agent.Prompt{Text: prompt},
		Repos:             adoptRepos,
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

	foundPRFromLog := false
	switch {
	case lt.ForgePR > 0:
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		foundPRFromLog = true
	case forgeIssue > 0 && ri.ForgeOwner != "":
		t.SetPR(ri.ForgeOwner, ri.ForgeRepo, 0)
	}

	// Restore messages from both the local log and the relay tail. The local log
	// has the full pre-restart history; the relay tail has any output produced
	// while the server was down. Merge them by overlap so the UI does not collapse
	// to the bounded relay tail after a server restart. Fail closed: a malformed
	// persistent history must not attach a live task with untrusted state.
	if err := lt.LoadMessagesWithResolver(m.resolveNativeParser); err != nil {
		return nil, fmt.Errorf("load messages for adopted task %s: %w", taskID, err)
	}
	t.SetLogValidationSnapshot(lt.ValidatedSnapshot())
	if len(relayMsgs) > 0 {
		msgs := relayMsgs
		if len(lt.Msgs) > 0 {
			msgs = mergeLogAndRelayMessages(lt.Msgs, relayMsgs)
		}
		t.RestoreMessages(msgs)
		applyLoadedSessionMetadata(t, lt)
		t.SetRelayOffset(relaySize)
		m.log.DebugContext(ctx, "relay", "msg", "restored from", "repo", ri.RelPath, "br", branch, "instance", c.ID, "alive", relayAlive, "msgs", len(msgs), "relayMsgs", len(relayMsgs))
	} else if len(lt.Msgs) > 0 {
		t.RestoreMessages(lt.Msgs)
		applyLoadedSessionMetadata(t, lt)
		m.log.WarnContext(ctx, "relay", "msg", "restored from log", "repo", ri.RelPath, "br", branch, "instance", c.ID, "msgs", len(lt.Msgs))
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
			foundPRFromLog = true
		}
	}

	if !isExited {
		t.SetTurnStartedAt(time.Now().UTC())
	}

	if isExited {
		if t.GetState() != task.StateCrashed {
			if t.LastExitError() != "" {
				t.RecordSessionCrash(ctx, errors.New("agent subprocess exited before adoption"))
			} else {
				t.SetState(task.StateStopped)
			}
		}
	} else if !relayAlive {
		relayLog := m.relay.ReadLog(ctx, relayTarget, 4096)
		if relayLog != "" {
			m.log.WarnContext(ctx, "relay", "msg", "log from dead relay", "instance", c.ID, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		trace.Logf(ctx, "adopt", "%s: relay-dead", c.ID)
		if t.LastExitError() != "" {
			t.RecordSessionCrash(ctx, errors.New("relay exited before adoption"))
			if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // adoption must outlive request
				m.log.ErrorContext(ctx, "stop failed after adopted relay crash", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", err)
			}
		} else if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			m.log.WarnContext(ctx, "relay", "msg", "dead, marking waiting",
				"repo", ri.RelPath, "br", branch, "instance", c.ID,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entry := NewEntry(t, lt)
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
		if err := writeTaskResultTrailer(t, result); err != nil {
			m.log.WarnContext(ctx, "write adopted result trailer failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", err)
		}
	}

	// Register entry, replacing stale log entries.
	m.mu.Lock()
	if ri.RelPath != "" || branch != "" {
		for _, oldID := range branchIDs[ri.RelPath+"\x00"+branch] {
			delete(m.tasks, oldID)
		}
	}
	m.tasks[t.ID.String()] = entry
	m.taskChanged()
	m.mu.Unlock()
	m.watchRateLimitEvents(t)

	m.log.InfoContext(ctx, "instance", "msg", "adopted",
		"repo", ri.RelPath, "instance", c.ID, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed.
	if needsTitleRegen(t, lt, m.resolveNativeParser) {
		go t.GenerateTitle(m.serverCtx) //nolint:contextcheck // fire-and-forget; must outlive adoption
	}

	// Auto-reconnect immediately so adopted live tasks can accept input as
	// soon as startup returns. EnsureSession may still replace an already-exited
	// attach in the background, but the attach itself must not race the first
	// user reply after restart.
	if t.GetState() != task.StateStopped && relayAlive {
		m.log.DebugContext(ctx, "instance", "msg", "auto-reconnect starting", "repo", ri.RelPath, "br", branch, "instance", c.ID)
		tlog := m.log.With("repo", ri.RelPath, "br", branch, "instance", t.RuntimeInstanceID())
		h, err := m.sessions(workspace).Reconnect(m.serverCtx, t, true) //nolint:contextcheck // adopted sessions must outlive startup/adoption.
		if err != nil {
			tlog.Warn("auto-reconnect failed", "err", err)
			m.NotifyTaskChange()
			return &AdoptedTask{Entry: entry, Task: t, RelPath: ri.RelPath, ForgeKind: ri.ForgeKind, ForgeOwner: ri.ForgeOwner, ForgeRepo: ri.ForgeRepo, Branch: branch, FoundPRFromLog: foundPRFromLog}, nil
		}
		go func() {
			h, err = m.sessions(workspace).EnsureSession(m.serverCtx, t, h, tlog)
			if err != nil {
				tlog.Warn("ensure session failed", "err", err)
				t.SetState(task.StateWaiting)
				m.NotifyTaskChange()
				return
			}
			tlog.Debug("auto-reconnect succeeded")
			t.SetVNCPort(m.Runtimes.VNCPort(m.serverCtx, t.RuntimeInstanceID()))
			refreshAdoptedDiffStat(m.serverCtx, workspace, t)
			m.NotifyTaskChange()
			m.watchSession(entry, workspace, h)
		}()
	} else if !relayAlive && t.GetState() != task.StateStopped && t.GetState() != task.StateCrashed && t.GetState() != task.StateFailed {
		m.log.ErrorContext(ctx, "relay dead, stopping instance",
			"repo", ri.RelPath, "br", branch, "instance", c.ID,
			"state", t.GetState())
		t.SetState(task.StateStopping)
		if err := m.Runtimes.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // adoption must outlive request
			m.log.ErrorContext(ctx, "stop failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", err)
		}
		t.SetState(task.StateStopped)
	}

	return &AdoptedTask{
		Entry:          entry,
		Task:           t,
		RelPath:        ri.RelPath,
		ForgeKind:      ri.ForgeKind,
		ForgeOwner:     ri.ForgeOwner,
		ForgeRepo:      ri.ForgeRepo,
		Branch:         branch,
		FoundPRFromLog: foundPRFromLog,
	}, nil
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

func writeTaskResultTrailer(t *task.Task, r *task.Result) error {
	msg := &agent.MetaResultMessage{
		MessageType:              "caic_result",
		State:                    r.State.String(),
		Title:                    t.Title(),
		CostUSD:                  r.CostUSD,
		Duration:                 r.Duration.Seconds(),
		NumTurns:                 r.NumTurns,
		InputTokens:              r.Usage.InputTokens,
		OutputTokens:             r.Usage.OutputTokens,
		CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     r.Usage.CacheReadInputTokens,
		DiffStat:                 r.DiffStat,
		AgentResult:              r.AgentResult,
	}
	if r.Err != nil {
		msg.Error = r.Err.Error()
	}
	return t.WriteToLog(msg)
}

func refreshAdoptedDiffStat(ctx context.Context, workspace *repowork.Workspace, t *task.Task) {
	switch t.GetState() {
	case task.StateWaiting, task.StateAsking, task.StateHasPlan:
	default:
		return
	}
	if ds := workspace.BranchDiffStat(ctx, t); len(ds) > 0 {
		t.SetLiveDiffStat(ds)
	}
}

// resolveNativeParser constructs one fresh native parser for a validated task
// log harness.
func (m *Manager) resolveNativeParser(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
	backend := m.backends[h]
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

// watchSession monitors a single active session. Clean session exits move the
// task to StateWaiting; SSH/session errors fail the task and stop the instance.
func (m *Manager) watchSession(entry *Entry, workspace *repowork.Workspace, h *task.SessionHandle) {
	go func() {
		t := entry.Task()
		traceCtx, tk := trace.NewTask(m.serverCtx, "session.watch:"+t.ID.String())
		defer tk.End()
		_ = traceCtx
		done := h.Session.Done()
		select {
		case <-done:
			// Session died. Check if this handle is still the task's current handle.
			current := t.SessionDone()
			if current != done {
				return
			}
			t.DetachSession()
			sessionErr := h.Session.Wait()
			h.CloseMsgCh()
			<-h.DispatchDone
			if h.LogW != nil {
				_ = h.LogW.Close()
			}
			watchPrimaryName := ""
			watchPrimaryBranch := ""
			if p := t.Primary(); p != nil {
				watchPrimaryName = p.Name
				watchPrimaryBranch = p.Branch
			}
			attrs := []any{"repo", watchPrimaryName, "br", watchPrimaryBranch, "instance", t.RuntimeInstanceID()}
			if sessionErr != nil {
				m.log.WarnContext(m.serverCtx, "session exited with error", append(attrs, "err", sessionErr)...)
				if t.RecordSessionCrash(m.serverCtx, sessionErr) {
					m.stopFailedSessionInstance(workspace, t, attrs)
					crashErr := sessionErr
					if exitErr := t.LastExitError(); exitErr != "" {
						crashErr = errors.New(exitErr)
					}
					costUSD, numTurns, duration, usage, _ := t.LiveStats()
					result := &task.Result{
						State:       task.StateCrashed,
						DiffStat:    t.LiveDiffStat(),
						CostUSD:     costUSD,
						Duration:    duration,
						NumTurns:    numTurns,
						Usage:       usage,
						AgentResult: t.LastAgentResult(),
						Err:         crashErr,
					}
					entry.Finish(result)
					if err := writeTaskResultTrailer(t, result); err != nil {
						m.log.WarnContext(m.serverCtx, "write crashed task trailer failed", append(attrs, "err", err)...)
					}
					if err := t.CommitEventReplay(); err != nil {
						m.log.WarnContext(m.serverCtx, "commit event replay failed", append(attrs, "err", err)...)
					}
				} else if t.RecordSessionFailure(m.serverCtx, sessionErr) {
					failureErr := sessionErr
					if exitErr := t.LastExitError(); exitErr != "" {
						failureErr = errors.New(exitErr)
					}
					costUSD, numTurns, duration, usage, _ := t.LiveStats()
					result := &task.Result{
						State:       task.StateFailed,
						DiffStat:    t.LiveDiffStat(),
						CostUSD:     costUSD,
						Duration:    duration,
						NumTurns:    numTurns,
						Usage:       usage,
						AgentResult: t.LastAgentResult(),
						Err:         failureErr,
					}
					entry.Finish(result)
					if err := writeTaskResultTrailer(t, result); err != nil {
						m.log.WarnContext(m.serverCtx, "write failed task trailer failed", append(attrs, "err", err)...)
					}
					if err := t.CommitEventReplay(); err != nil {
						m.log.WarnContext(m.serverCtx, "commit event replay failed", append(attrs, "err", err)...)
					}
				}
			} else {
				m.log.InfoContext(m.serverCtx, "session exited", attrs...)
				// Race with Task.addMessage: a clean relay exit reaches here
				// concurrently with the dispatch goroutine processing the
				// final ResultMessage (which also targets Waiting/Asking/
				// HasPlan; see the comment at addMessage's ResultMessage
				// handling in task.go). The CAS only fires from Running, so
				// it is a no-op once addMessage has already moved the state.
				t.SetStateIf(task.StateRunning, task.StateWaiting)
				if err := t.CommitEventReplay(); err != nil {
					m.log.WarnContext(m.serverCtx, "commit event replay failed", append(attrs, "err", err)...)
				}
			}
			m.NotifyTaskChange()
		case <-entry.Done():
		}
	}()
}

func (m *Manager) stopFailedSessionInstance(_ *repowork.Workspace, t *task.Task, attrs []any) {
	id := t.RuntimeInstanceID()
	if id == "" {
		return
	}
	if err := m.Runtimes.Stop(m.serverCtx, id); err != nil {
		m.log.ErrorContext(m.serverCtx, "stop failed after session error", append(attrs, "err", err)...)
	}
}

// cleanupTask runs workspace.Cleanup exactly once per task.
func (m *Manager) cleanupTask(entry *Entry, workspace *repowork.Workspace, reason task.State) {
	entry.Cleanup(func() {
		start := time.Now()
		t := entry.Task()
		result := m.runner(workspace).Cleanup(m.serverCtx, t, reason)
		elapsed := time.Since(start).Round(time.Millisecond)
		if result.Err != nil {
			m.log.ErrorContext(m.serverCtx, "cleanup failed", "task", t.ID, "reason", reason, "dur", elapsed, "err", result.Err)
		} else {
			m.log.InfoContext(m.serverCtx, "cleanup done", "task", t.ID, "reason", reason, "dur", elapsed,
				"cost", result.CostUSD, "turns", result.NumTurns, "final_state", result.State)
		}
		entry.Finish(&result)
		m.NotifyTaskChange()
	})
}

// logRelayMessageMerger owns the adoption-time overlap rules for disk-log
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
		// the durable log and from the relay tail during restart adoption.
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
