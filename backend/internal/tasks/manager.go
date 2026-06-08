// Package tasks orchestrates task lifecycle management: creation, execution,
// session watching, stats polling, and instance adoption.
//
// It sits between the HTTP adapter (internal/server) and the domain layer
// (internal/task). The one-letter difference from the singular "task" package
// is deliberate: task.Task / task.Runner are pure domain types, while
// tasks.Manager is the orchestration layer.
//
// Two contexts coexist here. Methods accept a request-scoped ctx that is
// honored for synchronous work (state checks, logging). Background goroutines
// that must outlive any single request (session watchers, stats poller, fire-
// and-forget title generation) use serverCtx instead — it tracks the lifetime
// of the Manager itself.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// errTaskNotFound is returned when a task ID doesn't exist.
var errTaskNotFound = &Error{Kind: KindNotFound, Msg: "task not found"}

type relayReader interface {
	Status(ctx context.Context, container string) (bool, string, error)
	ReadTail(ctx context.Context, container string, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) ([]agent.Message, int64, error)
	ReadLog(ctx context.Context, container string, maxBytes int) string
}

type agentRelayReader struct{}

func (agentRelayReader) Status(ctx context.Context, container string) (alive bool, diag string, err error) {
	return agent.RelayStatus(ctx, container)
}

func (agentRelayReader) ReadTail(ctx context.Context, container string, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) (msgs []agent.Message, size int64, err error) {
	return agent.ReadRelayTail(ctx, container, parseFn, maxBytes)
}

func (agentRelayReader) ReadLog(ctx context.Context, container string, maxBytes int) string {
	return agent.ReadRelayLog(ctx, container, maxBytes)
}

// Config holds the dependencies Manager needs at construction.
type Config struct {
	// ServerCtx tracks the Manager's own lifetime. Used as the parent for
	// background goroutines that must survive individual requests.
	ServerCtx context.Context
	LogDir    string
	CacheDir  string
	// Backend is the per-task instance lifecycle seam (launch/stop/purge/fork).
	// Production passes *mdruntime.Backend (md over Docker/Podman); a future VM backend or a
	// test fake can be substituted via this interface.
	Backend    runtime.Backend
	Monitor    runtime.Monitor
	Inventory  runtime.Inventory
	Privilege  runtime.PrivilegeInfo
	Backends   map[agent.Harness]agent.Backend
	HarnessEnv map[string][]string
	Prefs      *preferences.Store
	Provider   genai.Provider // nil-safe
}

// Manager owns the task and runner registries, instance adoption, session
// watching, and stats polling.
type Manager struct {
	// Immutable after construction.
	serverCtx  context.Context // lifetime of the Manager; for goroutines that outlive requests
	logDir     string
	cacheDir   string
	backend    runtime.Backend
	monitor    runtime.Monitor
	inventory  runtime.Inventory
	privilege  runtime.PrivilegeInfo
	harnessEnv map[string][]string
	prefs      *preferences.Store
	provider   genai.Provider
	relay      relayReader

	// Guarded by mu.
	mu      sync.Mutex
	tasks   map[string]*Entry
	runners map[string]*task.Runner
	changed chan struct{} // closed on mutation, replaced under mu
}

// New creates a Manager. A no-repo runner is always registered.
// Call RegisterRunner for each repo, then Start.
func New(cfg Config) *Manager { //nolint:gocritic // Config is a value bag passed once at construction
	m := &Manager{
		serverCtx:  cfg.ServerCtx,
		logDir:     cfg.LogDir,
		cacheDir:   cfg.CacheDir,
		backend:    cfg.Backend,
		monitor:    cfg.Monitor,
		inventory:  cfg.Inventory,
		privilege:  cfg.Privilege,
		harnessEnv: cfg.HarnessEnv,
		prefs:      cfg.Prefs,
		provider:   cfg.Provider,
		relay:      agentRelayReader{},
		tasks:      make(map[string]*Entry),
		runners:    make(map[string]*task.Runner),
		changed:    make(chan struct{}),
	}
	noRepoRunner := &task.Runner{
		LogDir:     cfg.LogDir,
		CacheDir:   cfg.CacheDir,
		HarnessEnv: cfg.HarnessEnv,
		Runtime:    cfg.Backend,
		Backends:   cfg.Backends,
	}
	_ = noRepoRunner.Init(cfg.ServerCtx)
	m.runners[""] = noRepoRunner
	return m
}

// Start launches background goroutines: instance event watching and stats polling.
// Must be called once after New, after runners have been registered.
func (m *Manager) Start() {
	go m.watchRuntimeEvents(m.serverCtx)
	go m.pollStats(m.serverCtx)
}

// RegisterRunner registers a task runner keyed by relPath.
// "" registers the no-repo runner.
func (m *Manager) RegisterRunner(relPath string, r *task.Runner) {
	m.mu.Lock()
	m.runners[relPath] = r
	m.mu.Unlock()
}

// Runner returns the runner for relPath, or nil.
func (m *Manager) Runner(relPath string) (*task.Runner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[relPath]
	return r, ok
}

// RangeRunners iterates over every registered runner. It snapshots the registry
// under m.mu and invokes fn unlocked, so fn may safely call back into the
// Manager. The runner set is a point-in-time snapshot. Stops iteration if fn
// returns false.
func (m *Manager) RangeRunners(fn func(relPath string, r *task.Runner) bool) {
	m.mu.Lock()
	type kv struct {
		relPath string
		r       *task.Runner
	}
	snap := make([]kv, 0, len(m.runners))
	for relPath, r := range m.runners {
		snap = append(snap, kv{relPath, r})
	}
	m.mu.Unlock()
	for _, it := range snap {
		if !fn(it.relPath, it.r) {
			return
		}
	}
}

// UnregisterRunner removes the runner registered for relPath.
func (m *Manager) UnregisterRunner(relPath string) {
	m.mu.Lock()
	delete(m.runners, relPath)
	m.mu.Unlock()
}

// Insert registers a pre-built entry. Production task creation goes through
// Create/Fork; Insert is retained for tests (in internal/tasks and
// internal/server) that seed the registry without a real runner.
func (m *Manager) Insert(id string, entry *Entry) {
	m.insertEntry(id, entry)
}

// Range iterates over every registered entry. It snapshots the registry under
// m.mu and invokes fn unlocked, so fn may safely call back into the Manager
// (e.g. Runner). The entry set is a point-in-time snapshot; Entry pointers are
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
	// Resolve primary runner.
	var primaryRunner *task.Runner
	if len(p.Repos) > 0 {
		r, ok := m.Runner(p.Repos[0].Name)
		if !ok {
			return "", badRequestf("unknown repo: %s", p.Repos[0].Name)
		}
		primaryRunner = r
	} else {
		// New() always registers the no-repo "" runner, so this never fails.
		primaryRunner, _ = m.Runner("")
	}

	// Validate and resolve extra repo runners.
	extraRunners := make([]*task.Runner, 0, max(0, len(p.Repos)-1))
	for _, rs := range p.Repos[min(1, len(p.Repos)):] {
		er, ok := m.Runner(rs.Name)
		if !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
		extraRunners = append(extraRunners, er)
	}

	backend, ok := primaryRunner.Backends[p.Harness]
	if !ok {
		return "", badRequestf("unknown harness: %s", string(p.Harness))
	}

	if p.Model != "" && !slices.Contains(backend.Models(), p.Model) {
		return "", badRequestf("unsupported model for %s: %s", string(p.Harness), p.Model)
	}

	if len(p.Prompt.Images) > 0 && !backend.SupportsImages() {
		return "", badRequestf("%s does not support images", string(p.Harness))
	}

	// Build RepoMount slice. MountedPath follows the fixed "~/src/<name>"
	// convention used by the instance provisioner. Use the basename unless
	// another registered repo shares it.
	mounts := make([]task.RepoMount, len(p.Repos))
	for i, rs := range p.Repos {
		r, _ := m.Runner(rs.Name)
		mounts[i] = task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir, MountedPath: m.mountPathForRepo(rs.Name)}
	}

	t := &task.Task{
		ID:                ksid.NewID(),
		InitialPrompt:     p.Prompt,
		Repos:             mounts,
		Harness:           p.Harness,
		Model:             p.Model,
		Effort:            p.Effort,
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
	entry := NewEntry(t)

	m.insertEntry(t.ID.String(), entry)

	// Run in background using server context.
	go func() {
		for i, er := range extraRunners {
			branch, err := er.AllocateBranch(m.serverCtx)
			if err != nil {
				entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "allocate branch for extra repo")})
				m.NotifyTaskChange()
				return
			}
			t.SetRepoBranch(i+1, branch)
		}

		ghToken := p.ResolvedGitHubToken

		h, err := primaryRunner.Start(m.serverCtx, t, ghToken)
		if err != nil {
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "start task")})
			m.NotifyTaskChange()
			return
		}
		if t.Sudo {
			t.SetSudoPassword(m.SudoPassword(m.serverCtx, t))
		}
		m.NotifyTaskChange()
		m.watchSession(entry, primaryRunner, h)
	}()
	return t.ID.String(), nil
}

// Purge transitions a task to purging.
func (m *Manager) Purge(ctx context.Context, entry *Entry) error {
	state, changed := entry.task.SetStateIfAny(task.StatePurging,
		task.StateWaiting, task.StateAsking, task.StateHasPlan,
		task.StateRunning, task.StateStopping, task.StateStopped)
	if !changed {
		return conflict("task is not running or waiting")
	}
	m.NotifyTaskChange()
	runner := m.resolveRunner(entry.task)
	slog.InfoContext(ctx, "purge requested", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "state", state)
	go func() {
		m.cleanupTask(entry, runner, task.StatePurged)
		slog.InfoContext(m.serverCtx, "purge completed", "task", entry.task.ID, "final_state", entry.task.GetState())
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
	runner := m.resolveRunner(entry.task)
	slog.InfoContext(ctx, "stop requested", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "state", state)
	go func() {
		runner.StopTask(m.serverCtx, entry.task)
		slog.InfoContext(m.serverCtx, "stop completed", "task", entry.task.ID, "instance", entry.task.RuntimeInstanceID(), "final_state", entry.task.GetState())
		m.NotifyTaskChange()
	}()
	return nil
}

// Revive restarts a stopped task.
func (m *Manager) Revive(ctx context.Context, entry *Entry) error {
	if _, changed := entry.task.SetStateIfAny(task.StateProvisioning, task.StateStopped); !changed {
		return conflict("task is not stopped")
	}
	runner := m.resolveRunner(entry.task)
	entry.Reset()
	m.NotifyTaskChange()
	go func() { //nolint:contextcheck // background goroutine roots its own trace task on serverCtx
		ctx, tk := trace.NewTask(m.serverCtx, "task.revive:"+entry.task.ID.String())
		defer tk.End()
		h, err := runner.ReviveTask(ctx, entry.task)
		if err != nil {
			slog.WarnContext(ctx, "revive failed", "task", entry.task.ID, "err", err)
			entry.task.SetState(task.StateFailed)
			entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "revive task")})
			m.NotifyTaskChange()
			return
		}
		m.NotifyTaskChange()
		m.watchSession(entry, runner, h)
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
		plan, err := agent.ReadPlan(m.serverCtx, string(t.RuntimeInstanceID()), t.GetPlanFile()) //nolint:contextcheck // intentionally using server context
		if err != nil {
			t.SetStateIf(task.StateStarting, prevState)
			return &Error{Kind: KindBadRequest, Msg: "no prompt provided and failed to read plan from instance", Err: err}
		}
		prompt.Text = plan
	}
	runner := m.resolveRunner(t)
	h, err := runner.RestartSession(m.serverCtx, t, prompt) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return internalErr(err, "restart session")
	}
	m.watchSession(entry, runner, h)
	m.NotifyTaskChange()
	return nil
}

// ClearContext clears the agent session context.
func (m *Manager) ClearContext(ctx context.Context, entry *Entry) error {
	t := entry.task
	if _, changed := t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan); !changed {
		return conflict("task is not waiting or asking")
	}
	runner := m.resolveRunner(t)
	h, err := runner.ClearContextSession(m.serverCtx, t) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return internalErr(err, "clear context")
	}
	m.watchSession(entry, runner, h)
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
		runner := m.resolveRunner(entry.task)
		if b := runner.Backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return badRequestf("%s does not support images", string(entry.task.Harness))
		}
	}
	if err := entry.task.SendInput(ctx, prompt); err != nil {
		if !errors.Is(err, task.ErrNoActiveSession) {
			return conflictErr(err, "send input")
		}
		t := entry.task
		taskState := t.GetState()
		slog.WarnContext(ctx, "no active session",
			"task", t.ID,
			"instance", t.RuntimeInstanceID(),
			"state", taskState,
		)
		// Wrap so the handler can detect the no-session condition via
		// errors.Is while preserving the underlying diagnostic message
		// verbatim (NoSessionError.Error() == err.Error()).
		return &NoSessionError{Err: err}
	}
	return nil
}

// Fork creates a new task from a running task's instance.
func (m *Manager) Fork(ctx context.Context, sourceEntry *Entry, p ForkParams) (string, error) { //nolint:gocritic // ForkParams is a request-shaped value bag
	source := sourceEntry.task
	state := source.GetState()
	switch state {
	case task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan:
	default:
		return "", conflict("task must be running or waiting to fork")
	}
	if source.RuntimeInstanceID() == "" {
		return "", conflict("task has no instance")
	}
	sourceRepos := source.ReposSnapshot()
	if len(sourceRepos) == 0 {
		return "", badRequestf("cannot fork a no-repo task")
	}

	runner := m.resolveRunner(source)

	// Resolve harness and model.
	forkHarness := source.Harness
	forkModel := source.Model
	forkEffort := source.Effort
	if p.Harness != "" {
		forkHarness = p.Harness
		backend, ok := runner.Backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(p.Harness))
		}
		if p.Model != "" && !slices.Contains(backend.Models(), p.Model) {
			return "", badRequestf("unsupported model for %s: %s", string(p.Harness), p.Model)
		}
		forkModel = p.Model
		forkEffort = p.Effort
	} else if p.Model != "" {
		backend, ok := runner.Backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(source.Harness))
		}
		if !slices.Contains(backend.Models(), p.Model) {
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
	var extraRepos []runtime.Repo
	for _, rs := range p.ExtraRepos {
		if _, overlap := sourceRepoNames[rs.Name]; overlap {
			return "", badRequestf("extraRepos contains repo already in source task: %s", rs.Name)
		}
		er, ok := m.Runner(rs.Name)
		if !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
		rm := task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: er.Dir, MountedPath: m.mountPathForRepo(rs.Name)}
		extraMounts = append(extraMounts, rm)
		extraRepos = append(extraRepos, rm.ToRuntimeRepo())
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
		Provider:          m.provider,
	}
	t.SetTitle(p.Prompt.Text)
	go t.GenerateTitle(m.serverCtx) //nolint:contextcheck // fire-and-forget; must outlive request
	forkEntry := NewEntry(t)

	m.insertEntry(t.ID.String(), forkEntry)

	go func() { //nolint:contextcheck // background goroutine roots its own trace task on serverCtx
		ctx, tk := trace.NewTask(m.serverCtx, "task.fork:"+source.ID.String()+"->"+t.ID.String())
		defer tk.End()
		ghToken := p.ResolvedGitHubToken

		var extraEnv []string
		if ghToken != "" {
			extraEnv = append(extraEnv, "GITHUB_TOKEN="+ghToken)
		}

		forkOpts := &runtime.ForkOptions{
			ExtraRepos: extraRepos,
			Display:    p.Display,
			Tailscale:  p.Tailscale,
			USB:        p.USB,
			Sudo:       p.Sudo,
			Metadata:   task.MakeMetadata(t),
			Harness:    forkHarness,
			ExtraEnv:   extraEnv,
			Mounts:     slices.Clone(source.Mounts),
			MaxCPUs:    source.MaxCPUs,
		}
		h, err := runner.ForkTask(ctx, source, t, forkOpts, ghToken)
		if err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "fork task")})
			m.NotifyTaskChange()
			return
		}
		if t.Sudo {
			t.SetSudoPassword(m.SudoPassword(m.serverCtx, t))
		}
		m.NotifyTaskChange()
		m.watchSession(forkEntry, runner, h)
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
	if runner, ok := m.Runner(p.Name); ok {
		return runner.BaseBranch
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
	pw, err := m.privilege.SudoPassword(ctx, instanceID)
	if err != nil {
		slog.WarnContext(ctx, "sudo password lookup failed", "instance", instanceID, "err", err)
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
func (m *Manager) AdoptInstances(ctx context.Context, repos []AdoptRepo, instances []runtime.Instance, allLogs []*task.LoadedTask) ([]AdoptedTask, error) {
	if instances == nil {
		return nil, nil
	}

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
	var adopted []AdoptedTask
	claimed := make(map[runtime.InstanceID]bool, len(instances))

	for i := range repos {
		ri := &repos[i]
		runner, _ := m.Runner(ri.RelPath)
		if runner == nil {
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
			wg.Go(func() {
				at, err := m.adoptOne(ctx, *ri, runner, c, branch, branchIDs, allLogs)
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
	if noRepoRunner, ok := m.Runner(""); ok {
		for i := range instances {
			c := &instances[i]
			if claimed[c.ID] || !strings.HasPrefix(string(c.ID), "md-agent-") {
				continue
			}
			wg.Go(func() {
				at, err := m.adoptOne(ctx, AdoptRepo{}, noRepoRunner, c, "", branchIDs, allLogs)
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
	if !mountedPathMatchesRepo(r.MountPath, ri) {
		return "", false
	}
	return r.Branch, true
}

func mountedPathMatchesRepo(mountedPath string, ri *AdoptRepo) bool {
	relPath := filepath.ToSlash(ri.RelPath)
	baseName := filepath.Base(ri.AbsPath)
	return slices.Contains([]string{
		"/home/user/src/" + relPath,
		"~/src/" + relPath,
		relPath,
		"/home/user/src/" + baseName,
		"~/src/" + baseName,
		baseName,
	}, mountedPath)
}

// needTitleRegen reports whether the adopted task needs an LLM title regeneration.
func needsTitleRegen(t *task.Task, lt *task.LoadedTask) bool {
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
	if err := lt.LoadMessages(); err == nil {
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
		if st == task.StateWaiting || st == task.StateStopped || st == task.StateFailed || st == task.StatePurged {
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
		case task.StateWaiting, task.StateStopped, task.StateFailed, task.StatePurged:
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
		slog.Info("no purged tasks to load", "candidates", len(all))
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lt := range purged {
		taskID := ksid.NewID()
		if len(lt.TaskID) >= 9 {
			if parsed, parseErr := ksid.Parse(lt.TaskID); parseErr == nil && parsed != 0 {
				taskID = parsed
			}
		}
		t := &task.Task{
			ID:            taskID,
			InitialPrompt: agent.Prompt{Text: lt.Prompt},
			Model:         lt.Model,
			Effort:        lt.Effort,
			Repos:         lt.Repos,
			Harness:       lt.Harness,
			StartedAt:     lt.StartedAt,
			Tailscale:     lt.Tailscale,
			USB:           lt.USB,
			Display:       lt.Display,
			Sudo:          lt.Sudo,
			GitHubToken:   lt.GitHubToken,
		}
		t.SetStateAt(lt.State, lt.LastStateUpdateAt)
		if lt.AgentVersion != "" {
			t.SetAgentVersion(lt.AgentVersion)
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
		m.setParser(lt)
		m.tasks[t.ID.String()] = newPurgedEntry(t, lt.Result, lt)
	}
	m.taskChanged()
	slog.Info("loaded purged tasks from logs", "n", len(purged))
	return nil
}

// Cleanup is the exported variant of cleanupTask, idempotent per incarnation.
func (m *Manager) Cleanup(entry *Entry, runner *task.Runner, reason task.State) {
	m.cleanupTask(entry, runner, reason)
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
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateFailed, task.StatePurged:
		return nil, conflict("task is in a terminal state")
	}

	runner := m.resolveRunner(t)
	syncPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		syncPrimaryBranch = p.Branch
	}

	if target == SyncTargetDefault {
		if force {
			return nil, badRequestf("force is not supported for default-branch sync")
		}
		baseBranch := runner.BaseBranch
		message := t.Title()
		if message == "" {
			message = t.InitialPrompt.Text
		}
		ds, issues, err := runner.SyncToDefault(ctx, t, message)
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
	ds, issues, err := runner.SyncToOrigin(ctx, t, force)
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

func (m *Manager) mountPathForRepo(relPath string) string {
	base := filepath.Base(relPath)
	if !m.repoBasenameCollides(relPath) {
		return "~/src/" + base
	}
	return "~/src/" + relPath
}

func (m *Manager) repoBasenameCollides(relPath string) bool {
	base := filepath.Base(relPath)
	collides := false
	m.RangeRunners(func(other string, _ *task.Runner) bool {
		if other != "" && other != relPath && filepath.Base(other) == base {
			collides = true
			return false
		}
		return true
	})
	return collides
}

// pollStats polls runtime resource stats every 5 seconds for all active tasks.
func (m *Manager) pollStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pushStats(ctx)
		}
	}
}

func (m *Manager) pushStats(ctx context.Context) {
	defer trace.StartRegion(ctx, "poll-stats").End()
	m.mu.Lock()
	type entry struct {
		task *task.Task
		name runtime.InstanceID
	}
	var active []entry
	for _, e := range m.tasks {
		t := e.task
		name := t.RuntimeInstanceID()
		if name == "" {
			continue
		}
		st := t.GetState()
		if st == task.StatePurged || st == task.StateFailed || st == task.StateStopped || st == task.StateStopping {
			continue
		}
		active = append(active, entry{task: t, name: name})
	}
	m.mu.Unlock()
	if len(active) == 0 {
		return
	}
	ids := make([]runtime.InstanceID, len(active))
	for i, e := range active {
		ids[i] = e.name
	}
	statsMap, err := m.monitor.StatsAll(ctx, ids)
	if err != nil {
		slog.Debug("stats poll failed", "err", err)
		return
	}
	now := time.Now()
	for _, e := range active {
		cs, ok := statsMap[e.name]
		if !ok {
			continue
		}
		e.task.PushStats(&runtime.Stats{
			Ts:         now,
			CPUPerc:    cs.CPUPerc,
			MemUsed:    cs.MemUsed,
			MemLimit:   cs.MemLimit,
			MemPerc:    cs.MemPerc,
			NetRx:      cs.NetRx,
			NetTx:      cs.NetTx,
			BlockRead:  cs.BlockRead,
			BlockWrite: cs.BlockWrite,
			DiskUsed:   cs.DiskUsed,
		})
	}
}

// watchRuntimeEvents listens for runtime instance exit events and triggers
// cleanup for the corresponding task.
func (m *Manager) watchRuntimeEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := m.monitor.WatchEvents(ctx, runtime.EventFilter{MetadataKey: runtime.MetadataLegacyTaskID})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("runtime events failed, retrying in 5s", "err", err)
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
			slog.Warn("runtime events stream ended, reconnecting in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// handleRuntimeInstanceExit looks up a task by runtime instance name and archives it.
func (m *Manager) handleRuntimeInstanceExit(instanceID runtime.InstanceID) {
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
		task.StatePurged, task.StateFailed, task.StateStopped,
		task.StateStopping, task.StatePurging)
	if !changed {
		return
	}
	deathBranch := ""
	if p := t.Primary(); p != nil {
		deathBranch = p.Branch
	}
	slog.Info("instance", "msg", "died, archiving as stopped", "instance", instanceID, "task", t.ID, "br", deathBranch, "prev_state", prevState)
	t.DetachSession()
	m.NotifyTaskChange()
}

// insertEntry adds an entry under m.mu and signals the change.
func (m *Manager) insertEntry(id string, entry *Entry) {
	m.mu.Lock()
	m.tasks[id] = entry
	m.taskChanged()
	m.mu.Unlock()
}

// taskChanged closes the current changed channel and replaces it.
// Must be called while holding m.mu.
func (m *Manager) taskChanged() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// resolveRunner returns the runner for a task's primary repo, or the
// always-present no-repo runner. Both are guaranteed non-nil: New()
// registers "", and callers must register repo-specific runners.
func (m *Manager) resolveRunner(t *task.Task) *task.Runner {
	key := ""
	if p := t.Primary(); p != nil {
		key = p.Name
	}
	r, _ := m.Runner(key)
	return r
}

func applyLoadedSessionMetadata(t *task.Task, lt *task.LoadedTask) {
	if lt == nil || t.GetSessionID() != "" {
		return
	}
	t.SetSessionMetadata(lt.SessionID, lt.Model, lt.AgentVersion)
}

// adoptOne investigates a single runtime instance and registers it as a task.
func (m *Manager) adoptOne(ctx context.Context, ri AdoptRepo, runner *task.Runner, c *runtime.Instance, branch string, branchIDs map[string][]string, allLogs []*task.LoadedTask) (*AdoptedTask, error) { //nolint:gocritic // massive function from existing code; refactor deferred
	ctx, adoptTask := trace.NewTask(ctx, "adopt-instance")
	defer adoptTask.End()
	trace.Logf(ctx, "instance", "%s repo=%s branch=%s", c.ID, ri.RelPath, branch)

	// Only adopt runtime instances that caic started. MetadataTaskID is set at
	// creation and is the authoritative proof of ownership.
	taskIDVal, err := m.inventory.Metadata(ctx, c.ID, runtime.MetadataTaskID)
	if taskIDVal == "" && err == nil {
		taskIDVal, err = m.inventory.Metadata(ctx, c.ID, runtime.MetadataLegacyTaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("metadata check for %s: %w", c.ID, err)
	}
	if taskIDVal == "" {
		slog.Info("instance", "msg", "skipping non-caic", "repo", ri.RelPath, "instance", c.ID, "br", branch)
		return nil, nil //nolint:nilnil // non-caic runtime instances are intentionally skipped
	}
	taskID, err := ksid.Parse(taskIDVal)
	if err != nil {
		return nil, fmt.Errorf("parse caic metadata %q on %s: %w", taskIDVal, c.ID, err)
	}

	isExited := c.State == "exited"
	if isExited {
		slog.Info("instance", "msg", "adopting exited instance as stopped", "instance", c.ID, "br", branch)
	}

	// Find the log file for this task.
	var lt *task.LoadedTask
	for _, log := range slices.Backward(allLogs) {
		if branch == "" && ri.RelPath == "" {
			if log.TaskID == taskID.String() {
				lt = log
				break
			}
		} else {
			lp := log.Primary()
			if lp != nil && lp.Branch == branch && lp.Name == ri.RelPath {
				lt = log
				break
			}
		}
	}

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Read harness from runtime metadata (authoritative), fall back to log.
	harnessLabel, _ := m.inventory.Metadata(ctx, c.ID, runtime.MetadataHarness)
	if harnessLabel == "" {
		harnessLabel, _ = m.inventory.Metadata(ctx, c.ID, runtime.MetadataLegacyHarness)
	}
	harnessName := agent.Harness(harnessLabel)
	if harnessName == "" && lt != nil {
		harnessName = lt.Harness
	}
	if harnessName == "" {
		harnessName = agent.Claude
	}
	if lt != nil {
		lt.Harness = harnessName
		m.setParser(lt)
	}

	// Check relay liveness.
	var relayAlive bool
	var relayMsgs []agent.Message
	var relaySize int64
	var relayDiag string
	if !isExited {
		var relayErr error
		relayAlive, relayDiag, relayErr = m.relay.Status(ctx, string(c.ID))
		if relayErr != nil {
			slog.Warn("relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", relayErr, "diag", relayDiag)
		}
		b, ok := runner.Backends[harnessName]
		if !ok {
			slog.Warn("relay", "msg", "no backend for harness", "harness", harnessName, "instance", c.ID)
			relayAlive = false
		} else {
			readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
			relayMsgs, relaySize, relayErr = m.relay.ReadTail(readCtx, string(c.ID), b.NewWire().ParseMessage, 10<<20) // 10 MiB tail
			readCancel()
			if relayErr != nil {
				slog.Warn("relay", "msg", "read output failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", relayErr)
				relayAlive = false
			}
		}
	}

	if lt != nil && lt.Prompt != "" {
		prompt = lt.Prompt
		startedAt = lt.StartedAt
		stateUpdatedAt = lt.LastStateUpdateAt
	}
	if stateUpdatedAt.IsZero() {
		stateUpdatedAt = time.Now().UTC()
	}
	var model string
	var effort string
	if lt != nil {
		model = lt.Model
		effort = lt.Effort
		if agent.RequiresResumeSessionID(harnessName) && lt.SessionID == "" {
			if err := lt.LoadSessionMetadata(); err != nil {
				slog.Warn("load session metadata failed", "repo", ri.RelPath, "br", branch, "err", err)
			}
		}
	}

	var adoptRepos []task.RepoMount
	if ri.RelPath != "" {
		primaryBaseBranch := ""
		if lt != nil && lt.Primary() != nil {
			primaryBaseBranch = lt.Primary().BaseBranch
		}
		// Derive MountedPath from the runtime instance repo metadata.
		var mountedPath string
		if len(c.Repos) > 0 && c.Repos[0].Branch == branch {
			mountedPath = c.Repos[0].MountPath
		}
		if mountedPath == "" {
			mountedPath = m.mountPathForRepo(ri.RelPath)
		}
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, BaseBranch: primaryBaseBranch, GitRoot: ri.AbsPath, Branch: branch, MountedPath: mountedPath}}
		if lt != nil {
			// Build lookup of instance repos by branch for MountedPath.
			containerRepoByBranch := make(map[string]string, len(c.Repos))
			for _, cr := range c.Repos {
				containerRepoByBranch[cr.Branch] = cr.MountPath
			}
			for _, lm := range lt.Repos[1:] {
				gitRoot := ""
				if er, ok := m.Runner(lm.Name); ok {
					gitRoot = er.Dir
				}
				mp := containerRepoByBranch[lm.Branch]
				if mp == "" {
					mp = m.mountPathForRepo(lm.Name)
				}
				adoptRepos = append(adoptRepos, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot, MountedPath: mp})
			}
		}
	}

	var forgeIssue int
	if lt != nil {
		forgeIssue = lt.ForgeIssue
	}

	t := &task.Task{
		ID:            taskID,
		InitialPrompt: agent.Prompt{Text: prompt},
		Repos:         adoptRepos,
		Harness:       harnessName,
		Model:         model,
		Effort:        effort,
		StartedAt:     startedAt,
		Tailscale:     c.Tailscale,
		TailscaleFQDN: c.TailscaleFQDN,
		USB:           c.USB,
		Display:       c.Display,
		Sudo:          c.Sudo,
		VNCPort:       c.VNCPort,
		Provider:      m.provider,
		ForgeIssue:    forgeIssue,
	}
	t.SetRuntimeInstanceInfo(c.ID, c.TailscaleFQDN, "", c.VNCPort)
	// Restore GitHub token flag from log trailer (primary) or runtime metadata (fallback).
	gtLabel, _ := m.inventory.Metadata(ctx, c.ID, runtime.MetadataGitHubToken)
	if (lt != nil && lt.GitHubToken) || gtLabel == "true" {
		t.SetGitHubTokenEnabled(true)
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	if c.Sudo {
		if pw, err := m.privilege.SudoPassword(ctx, c.ID); err == nil {
			t.SetSudoPassword(pw)
		}
	}
	if lt != nil && lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
	}
	if lt != nil && lt.LogPath() != "" {
		t.SetLogPath(lt.LogPath())
	}

	foundPRFromLog := false
	switch {
	case lt != nil && lt.ForgePR > 0:
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		foundPRFromLog = true
	case forgeIssue > 0 && ri.ForgeOwner != "":
		t.SetPR(ri.ForgeOwner, ri.ForgeRepo, 0)
	}

	// Restore messages from relay or logs.
	if len(relayMsgs) > 0 {
		t.RestoreMessages(relayMsgs)
		applyLoadedSessionMetadata(t, lt)
		t.SetRelayOffset(relaySize)
		slog.Debug("relay", "msg", "restored from", "repo", ri.RelPath, "br", branch, "instance", c.ID, "alive", relayAlive, "msgs", len(relayMsgs))
	} else if lt != nil {
		m.setParser(lt)
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
		if len(lt.Msgs) > 0 {
			t.RestoreMessages(lt.Msgs)
			applyLoadedSessionMetadata(t, lt)
			slog.Warn("relay", "msg", "restored from log", "repo", ri.RelPath, "br", branch, "instance", c.ID, "msgs", len(lt.Msgs))
		}
	}
	applyLoadedSessionMetadata(t, lt)
	t.SetStateAt(t.GetState(), stateUpdatedAt)

	// Full log parse for PR recovery.
	if lt != nil && t.GetPR() == 0 {
		if lt.ForgePR == 0 {
			_ = lt.LoadMessages()
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
			foundPRFromLog = true
		}
	}

	if !isExited {
		t.SetTurnStartedAt(time.Now().UTC())
	}

	if isExited {
		t.SetState(task.StateStopped)
	} else if !relayAlive {
		relayLog := m.relay.ReadLog(ctx, string(c.ID), 4096)
		if relayLog != "" {
			slog.Warn("relay", "msg", "log from dead relay", "instance", c.ID, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		trace.Logf(ctx, "adopt", "%s: relay-dead", c.ID)
		if t.LastExitError() != "" {
			t.RecordSessionFailure(ctx, errors.New("relay exited before adoption"))
		} else if t.GetState() == task.StateRunning {
			t.SetStateAt(task.StateWaiting, stateUpdatedAt)
			slog.Warn("relay", "msg", "dead, marking waiting",
				"repo", ri.RelPath, "br", branch, "instance", c.ID,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entry := NewEntry(t)
	if t.GetState() == task.StateFailed {
		failureErr := errors.New("agent session failed")
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
		if err := writeAdoptedFailureTrailer(t, result); err != nil {
			slog.Warn("write adopted failure trailer failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", err)
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

	slog.Info("instance", "msg", "adopted",
		"repo", ri.RelPath, "instance", c.ID, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed.
	if needsTitleRegen(t, lt) {
		go t.GenerateTitle(m.serverCtx) //nolint:contextcheck // fire-and-forget; must outlive adoption
	}

	// Auto-reconnect in background.
	if t.GetState() != task.StateStopped && relayAlive {
		slog.Debug("instance", "msg", "auto-reconnect starting", "repo", ri.RelPath, "br", branch, "instance", c.ID)
		go func() {
			tlog := slog.With("repo", ri.RelPath, "br", branch, "instance", t.RuntimeInstanceID())
			h, err := runner.Reconnect(m.serverCtx, t, true)
			if err != nil {
				tlog.Warn("auto-reconnect failed", "err", err)
				m.NotifyTaskChange()
				return
			}
			h, err = runner.EnsureSession(m.serverCtx, t, h, tlog)
			if err != nil {
				tlog.Warn("ensure session failed", "err", err)
				t.SetState(task.StateWaiting)
				m.NotifyTaskChange()
				return
			}
			tlog.Debug("auto-reconnect succeeded")
			t.SetVNCPort(runner.Runtime.VNCPort(m.serverCtx, t.RuntimeInstanceID()))
			refreshAdoptedDiffStat(m.serverCtx, runner, t)
			m.NotifyTaskChange()
			m.watchSession(entry, runner, h)
		}()
	} else if !relayAlive && t.GetState() != task.StateStopped && t.GetState() != task.StateFailed {
		slog.Error("relay dead, stopping instance",
			"repo", ri.RelPath, "br", branch, "instance", c.ID,
			"state", t.GetState())
		t.SetState(task.StateStopping)
		if err := runner.Runtime.Stop(m.serverCtx, c.ID); err != nil { //nolint:contextcheck // adoption must outlive request
			slog.Error("stop failed", "repo", ri.RelPath, "br", branch, "instance", c.ID, "err", err)
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

func writeAdoptedFailureTrailer(t *task.Task, r *task.Result) error {
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

func refreshAdoptedDiffStat(ctx context.Context, runner *task.Runner, t *task.Task) {
	switch t.GetState() {
	case task.StateWaiting, task.StateAsking, task.StateHasPlan:
	default:
		return
	}
	if ds := runner.BranchDiffStat(ctx, t); len(ds) > 0 {
		t.SetLiveDiffStat(ds)
	}
}

// setParser sets the parse function on a LoadedTask from the first runner
// that has a backend for the task's harness.
func (m *Manager) setParser(lt *task.LoadedTask) {
	// Called from LoadPurgedTasks which already holds m.mu.
	for _, r := range m.runners {
		if b := r.Backends[lt.Harness]; b != nil {
			lt.SetParser(b.NewWire().ParseMessage)
			return
		}
	}
}

// loadTaskMessagesOnDemand triggers lazy message loading for purged tasks.
func (m *Manager) loadTaskMessagesOnDemand(entry *Entry) {
	entry.LoadMessagesOnce(func() {
		lt := entry.LoadedTask()
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("lazy load messages failed", "task", entry.Task().ID, "err", err)
			return
		}
		entry.Task().RestoreMessages(lt.Msgs)
	})
}

// watchSession monitors a single active session. Clean session exits move the
// task to StateWaiting; SSH/session errors fail the task and stop the instance.
func (m *Manager) watchSession(entry *Entry, runner *task.Runner, h *task.SessionHandle) {
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
				slog.Warn("session exited with error", append(attrs, "err", sessionErr)...)
				if t.RecordSessionFailure(m.serverCtx, sessionErr) {
					m.stopFailedSessionInstance(runner, t, attrs)
					failureErr := sessionErr
					if exitErr := t.LastExitError(); exitErr != "" {
						failureErr = errors.New(exitErr)
					}
					costUSD, numTurns, duration, usage, _ := t.LiveStats()
					entry.Finish(&task.Result{
						State:       task.StateFailed,
						DiffStat:    t.LiveDiffStat(),
						CostUSD:     costUSD,
						Duration:    duration,
						NumTurns:    numTurns,
						Usage:       usage,
						AgentResult: t.LastAgentResult(),
						Err:         failureErr,
					})
				}
			} else {
				slog.Info("session exited", attrs...)
				t.SetStateIf(task.StateRunning, task.StateWaiting)
			}
			m.NotifyTaskChange()
		case <-entry.Done():
		}
	}()
}

func (m *Manager) stopFailedSessionInstance(runner *task.Runner, t *task.Task, attrs []any) {
	if runner == nil || runner.Runtime == nil {
		return
	}
	id := t.RuntimeInstanceID()
	if id == "" {
		return
	}
	if err := runner.Runtime.Stop(m.serverCtx, id); err != nil {
		slog.ErrorContext(m.serverCtx, "stop failed after session error", append(attrs, "err", err)...)
	}
}

// cleanupTask runs runner.Cleanup exactly once per task.
func (m *Manager) cleanupTask(entry *Entry, runner *task.Runner, reason task.State) {
	entry.Cleanup(func() {
		start := time.Now()
		t := entry.Task()
		result := runner.Cleanup(m.serverCtx, t, reason)
		elapsed := time.Since(start).Round(time.Millisecond)
		if result.Err != nil {
			slog.ErrorContext(m.serverCtx, "cleanup failed", "task", t.ID, "reason", reason, "dur", elapsed, "err", result.Err)
		} else {
			slog.InfoContext(m.serverCtx, "cleanup done", "task", t.ID, "reason", reason, "dur", elapsed,
				"cost", result.CostUSD, "turns", result.NumTurns, "final_state", result.State)
		}
		entry.Finish(&result)
		m.NotifyTaskChange()
	})
}
