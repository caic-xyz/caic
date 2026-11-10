// Lifecycle commits task intents, joins execution producers, and delivers durable queued prompt appends and edits.

package taskmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	goruntime "runtime"
	"runtime/trace"
	"slices"
	"sync"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

// Lifecycle coordinates state transitions, session watching, and user
// operations for one entry. Manager owns the entry registry and shared state.
type Lifecycle struct {
	manager      *Manager
	entry        *Entry
	agentRuntime taskrun.AgentRuntime
	ctx          context.Context
	wg           sync.WaitGroup

	inputMu sync.Mutex // Serializes immediate input and waiting-turn queue delivery.

	operationMu sync.Mutex // Keeps Stop/Revive/Purge completion in the same entry incarnation.

	purgeMu         sync.Mutex
	purgeCancel     context.CancelFunc
	purgeGeneration uint64
	purgeScheduled  bool
}

// Cancel rejects new execution and interrupts local producers without sealing the journal.
func (r *Lifecycle) Cancel() {
	r.agentRuntime.Cancel(r.entry.Task())
}

// Close stops local producers and waits for lifecycle-owned recording and background work.
func (r *Lifecycle) Close() error {
	r.Cancel()
	r.cancelScheduledPurge()
	// Lifecycle producers can still finalize accepted work after transport
	// cancellation. Join them before physically closing their record owner.
	r.wg.Wait()
	return r.agentRuntime.Shutdown(context.WithoutCancel(r.manager.serverCtx), r.entry.Task())
}

// Purge removes a stopped task immediately. Active tasks are stopped first and
// retain their runtime resources for delay so they can be revived.
func (r *Lifecycle) Purge(ctx context.Context, delay time.Duration) error {
	t := r.entry.Task()
	if delay < 0 {
		return badRequestf("purge delay cannot be negative")
	}
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()
	if r.purgeScheduled {
		switch t.GetState() {
		case task.StateStopping, task.StateStopped, task.StatePurging:
			return nil
		default:
			return conflict("task is no longer stopped for scheduled purge")
		}
	}

	state := t.GetState()
	runtimeState := task.StateStopping
	switch state {
	case task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateRunning, task.StateStopping:
	case task.StateStopped:
		runtimeState = task.StatePurging
	case task.StateCrashed:
		runtimeState = task.StateStopped
	default:
		return conflict("task is not running, waiting, stopped, or crashed")
	}
	state, accepted, err := r.agentRuntime.RecordLifecycleIntent(ctx, t, task.StatePurging, runtimeState, state)
	if err != nil {
		return fmt.Errorf("persist purge intent: %w", err)
	}
	if !accepted {
		return conflict("task state changed while scheduling purge")
	}
	purgeStoppedImmediately := state == task.StateStopped
	r.purgeScheduled = true
	r.purgeGeneration++
	generation := r.purgeGeneration
	purgeCtx, purgeCancel := context.WithCancel(context.WithoutCancel(ctx))
	r.purgeCancel = purgeCancel
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	r.manager.log.InfoContext(ctx, "purge scheduled", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", state, "delay", delay)
	r.wg.Go(func() {
		defer purgeCancel()
		defer func() {
			r.purgeMu.Lock()
			if r.purgeGeneration == generation {
				r.purgeCancel = nil
				r.purgeScheduled = false
			}
			r.purgeMu.Unlock()
		}()
		if !purgeStoppedImmediately {
			if state != task.StateStopping && state != task.StateCrashed {
				// Yield so the task-list snapshot loop can observe the stopping
				// state before StopTask advances it. This is best-effort: the
				// state transition journal is what guarantees the transition is
				// delivered.
				goruntime.Gosched()
				r.agentRuntime.StopTask(purgeCtx, t)
				r.manager.NotifyTaskUpdate(r.entry.Task().ID)
			}
			if !r.waitForStopped(purgeCtx) {
				return
			}

			if !r.waitForPurgeDelay(purgeCtx, delay) {
				return
			}
		}
		r.finishPurge(context.WithoutCancel(purgeCtx), t, purgeStoppedImmediately)
	})
	return nil
}

// Stop transitions the task to stopping and stops its runtime without purging it.
// Failed and crashed tasks can retain a running instance after partial startup.
func (r *Lifecycle) Stop(ctx context.Context) error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}
	defer r.operationMu.Unlock()
	t := r.entry.Task()
	if (t.GetState() == task.StateFailed || t.GetState() == task.StateCrashed) && t.RuntimeInstanceID() == "" {
		return conflict("task has no runtime instance to stop")
	}
	// Accept durable user intent before publishing a runtime transition or
	// stopping the relay, so interrupted shutdown remains intentional.
	state, accepted, err := r.agentRuntime.RecordLifecycleIntent(ctx, t, task.StateStopping, task.StateStopping,
		task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateRunning, task.StateFailed, task.StateCrashed)
	if err != nil {
		return fmt.Errorf("persist stop intent: %w", err)
	}
	if !accepted {
		return conflict("task is not running or waiting")
	}
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	r.manager.log.InfoContext(ctx, "stop requested", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", state)
	r.wg.Go(func() {
		r.operationMu.Lock()
		defer r.operationMu.Unlock()
		if res := r.agentRuntime.StopTask(r.ctx, t); res != nil {
			r.entry.Finish(res)
		}
		r.manager.log.InfoContext(r.ctx, "stop completed", "task", t.ID, "instance", t.RuntimeInstanceID(), "final_state", t.GetState())
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	})
	return nil
}

// Revive restarts a stopped or crashed task.
func (r *Lifecycle) Revive() error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}

	t := r.entry.Task()
	if state := t.GetState(); state != task.StateStopped && state != task.StateCrashed {
		r.operationMu.Unlock()
		return conflict("task is not stopped or crashed")
	}
	r.reviveLocked()
	return nil
}

// Restart starts a fresh agent session with prompt.
func (r *Lifecycle) Restart(ctx context.Context, prompt agent.Prompt) error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}
	defer r.operationMu.Unlock()
	t := r.entry.Task()
	if state := t.GetState(); state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan {
		return conflict("task is not waiting or asking")
	}
	if prompt.Text == "" {
		target := t.RuntimeConnectionTarget()
		var err error
		if target.SSHHost != "" {
			prompt.Text, err = agent.ReadPlan(r.ctx, target.SSHHost, t.GetPlanFile()) //nolint:contextcheck // plan read must finish during shutdown
		} else {
			err = errors.New("agent connection target missing SSH host")
		}
		if err != nil {
			return internalErr(err, "read plan from instance")
		}
	}
	h, err := r.agentRuntime.RestartSession(r.ctx, t, prompt) //nolint:contextcheck // session setup must finish during shutdown
	if err != nil {
		return internalErr(err, "restart session")
	}
	r.watchSession(h) //nolint:contextcheck // watcher uses the Manager lifetime.
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	return nil
}

// ClearContext starts a fresh idle agent session.
func (r *Lifecycle) ClearContext() error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}
	defer r.operationMu.Unlock()
	t := r.entry.Task()
	if state := t.GetState(); state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan {
		return conflict("task is not waiting or asking")
	}
	if r.agentRuntime.Checkout == nil {
		return internalErr(errors.New("task checkout is unavailable"), "clear context")
	}
	h, err := r.agentRuntime.ClearContextSession(r.ctx, t)
	if err != nil {
		return internalErr(err, "clear context")
	}
	r.watchSession(h)
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	return nil
}

// Compact asks the active agent session to compact its context.
func (r *Lifecycle) Compact(ctx context.Context, instructions string) error {
	if err := r.entry.Task().SendCompact(ctx, instructions); err != nil {
		if errors.Is(err, agent.ErrCompactInstructionsUnsupported) {
			return &Error{Kind: KindBadRequest, Msg: "compact context", Err: err}
		}
		return conflictErr(err, "compact context")
	}
	return nil
}

// SendInput forwards prompt to the agent session, reconnecting when needed.
func (r *Lifecycle) SendInput(ctx context.Context, prompt agent.Prompt) error {
	r.inputMu.Lock()
	defer r.inputMu.Unlock()
	return r.sendInputLocked(ctx, prompt)
}

// Start starts the runtime and agent session for a newly registered task.
func (r *Lifecycle) Start(ctx context.Context, resolvedGitHubToken string) error {
	t := r.entry.Task()
	h, err := r.agentRuntime.Start(ctx, t, resolvedGitHubToken)
	if err != nil {
		result := &task.Result{State: task.StateFailed, Err: internalErr(err, "start task")}
		if startupErr, ok := errors.AsType[*taskrun.StartupError](err); ok {
			details := startupErr.Details()
			result.StartupFailure = &details
		}
		r.entry.Finish(result)
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		return err
	}
	if t.Sudo {
		t.SetSudoPassword(r.manager.SudoPassword(ctx, t))
	}
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	r.watchSession(h) //nolint:contextcheck // watcher uses the Manager lifetime.
	return nil
}

// Fork initializes and publishes a new task with readable history, then forks
// this task's retained runtime instance asynchronously.
//
// p must be non-nil.
func (r *Lifecycle) Fork(ctx context.Context, p *ForkParams) (ksid.ID, error) {
	source := r.entry.Task()
	state := source.GetState()
	switch state {
	case task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateStopped, task.StateCrashed:
	default:
		return 0, conflict("task must be active, stopped, or crashed to fork")
	}
	if source.RuntimeInstanceID() == "" {
		return 0, conflict("task has no instance")
	}
	sourceRepos := source.ReposSnapshot()
	if len(sourceRepos) == 0 {
		return 0, badRequestf("cannot fork a no-repo task")
	}

	forkHarness := source.Harness
	forkModel := source.RequestedModel
	forkEffort := source.RequestedEffort
	sourceBackend, sourceHarnessAvailable := r.manager.Backends[forkHarness]
	if p.Harness == "" && !sourceHarnessAvailable {
		return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownHarness, Msg: "unknown harness: " + string(source.Harness)}
	}
	if p.Harness != "" {
		forkHarness = p.Harness
		backend, ok := r.manager.Backends[forkHarness]
		if !ok {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownHarness, Msg: "unknown harness: " + string(p.Harness)}
		}
		if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnsupportedModel, Msg: "unsupported model for " + string(p.Harness) + ": " + p.Model}
		}
		if p.Model == "" {
			if forkModel != "" && !slices.Contains(backend.ModelInventory().IDs(), forkModel) {
				return 0, &Error{Kind: KindBadRequest, Code: CodeUnsupportedModel, Msg: "unsupported model for " + string(p.Harness) + ": " + forkModel}
			}
		} else {
			forkModel = p.Model
			forkEffort = p.Effort
		}
	} else if p.Model != "" {
		if !slices.Contains(sourceBackend.ModelInventory().IDs(), p.Model) {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnsupportedModel, Msg: "unsupported model for " + string(source.Harness) + ": " + p.Model}
		}
		forkModel = p.Model
		forkEffort = p.Effort
	}

	sourceRepoNames := make(map[string]struct{}, len(sourceRepos))
	for _, repoMount := range sourceRepos {
		sourceRepoNames[repoMount.Name] = struct{}{}
	}
	var extraMounts []repo.Mount
	for _, rs := range p.ExtraRepos {
		if _, overlap := sourceRepoNames[rs.Name]; overlap {
			return 0, badRequestf("extraRepos contains repo already in source task: %s", rs.Name)
		}
		checkout, ok := r.manager.Checkouts.Checkout(rs.Name)
		if !ok {
			return 0, &Error{Kind: KindBadRequest, Code: CodeUnknownRepository, Msg: "unknown extra repo: " + rs.Name}
		}
		extraMounts = append(extraMounts, repo.Mount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: checkout.Dir, ContainerPath: r.manager.containerPathForRepo(rs.Name)})
	}

	mounts := make([]repo.Mount, len(sourceRepos), len(sourceRepos)+len(extraMounts))
	copy(mounts, sourceRepos)
	mounts = append(mounts, extraMounts...)
	t, err := r.manager.newTask(p.Prompt, forkHarness, forkModel, forkEffort, source.BaseImage, source.ContainerPlatform, "")
	if err != nil {
		return 0, badRequestf("%v", err)
	}
	t.Repos = mounts
	t.RuntimeName = source.RuntimeName
	t.MaxCPUs = source.MaxCPUs
	t.CacheMounts = slices.Clone(source.CacheMounts)
	t.Mounts = slices.Clone(source.Mounts)
	t.GitHubToken = p.GitHubToken
	t.Tailscale = p.Tailscale
	t.USB = p.USB
	t.Display = p.Display
	t.Sudo = p.Sudo
	t.OwnerID = p.OwnerID
	t.ForkedFromTaskID = source.ID
	t.ParentTaskID = p.parentTaskID
	if p.parentTaskID != 0 {
		// TODO(delegation): Enable CAIC MCP for delegated children after recursive
		// delegation has bounded depth and resource policies.
		t.CaicMCP = false
	}
	t.Provider = r.manager.provider
	forkEntry := r.manager.NewEntry(t, nil)
	if err := r.manager.publishEntry(ctx, forkEntry, len(sourceRepos), false); err != nil {
		return 0, err
	}
	forkEntry.Lifecycle.generateTitle()

	forkEntry.Lifecycle.wg.Go(func() { //nolint:contextcheck // fork must finish during shutdown
		ctx, tk := trace.NewTask(forkEntry.Lifecycle.ctx, "task.fork:"+source.ID.String()+"->"+t.ID.String())
		defer tk.End()
		metadata := maps.Clone(r.manager.runtimeMetadata)
		if metadata == nil {
			metadata = runtime.Metadata{}
		}
		maps.Copy(metadata, taskrun.MakeMetadata(t))
		forkOpts := &runtime.ForkOptions{RuntimeName: source.RuntimeName, Display: p.Display, Tailscale: p.Tailscale, USB: p.USB, Sudo: p.Sudo, Metadata: metadata, Harness: forkHarness, Mounts: slices.Clone(source.Mounts), MaxCPUs: source.MaxCPUs}
		if p.ResolvedGitHubToken != "" {
			forkOpts.ExtraEnv = []string{"GITHUB_TOKEN=" + p.ResolvedGitHubToken}
		}
		h, err := forkEntry.Lifecycle.agentRuntime.ForkTask(ctx, source, t, forkOpts, p.ResolvedGitHubToken)
		if err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "fork task")})
			r.manager.NotifyTaskUpdate(t.ID)
			return
		}
		if t.Sudo {
			t.SetSudoPassword(r.manager.SudoPassword(ctx, t))
		}
		r.manager.NotifyTaskUpdate(t.ID)
		forkEntry.Lifecycle.watchSession(h)
	})
	return t.ID, nil
}

// ForkDelegated creates a child from this task's server-owned runtime snapshot.
// The source task is recorded as the child's parent; callers cannot choose it.
func (r *Lifecycle) ForkDelegated(ctx context.Context, p *ForkParams) (ksid.ID, error) {
	return r.ForkDelegatedFor(ctx, r.entry.Task().ID, p)
}

// ForkDelegatedFor creates a child from this task's runtime snapshot for a
// server-authorized delegating task.
func (r *Lifecycle) ForkDelegatedFor(ctx context.Context, parentTaskID ksid.ID, p *ForkParams) (ksid.ID, error) {
	p.parentTaskID = parentTaskID
	return r.Fork(ctx, p)
}

// Sync pushes task commits to each repository's task or default branch and
// publishes refreshed diff statistics and repository state after a successful push.
func (r *Lifecycle) Sync(ctx context.Context, target SyncTarget, force bool) (*SyncResult, error) {
	t := r.entry.Task()
	switch t.GetState() {
	case task.StatePending:
		return nil, conflict("task has no instance yet")
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateCrashed, task.StateFailed, task.StatePurged:
		return nil, conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StatePulling, task.StatePushing:
		// Syncable states; continue below.
	}
	if target == SyncTargetDefault && force {
		return nil, badRequestf("force is not supported for default-branch sync")
	}
	checkout := r.agentRuntime.Checkout
	if checkout == nil {
		return nil, conflict("task has no checkout")
	}
	gitTarget, destinations, err := checkout.ResolvePushDestinations(ctx, r.manager.log, t.GitTarget(), target == SyncTargetDefault)
	if err != nil {
		return nil, internalErr(err, "resolve push destination")
	}
	opts := repo.PushOptions{}
	if target != SyncTargetDefault {
		opts = repo.PushOptions{CommitPending: true, BypassSafety: force, Force: true}
	}
	ds, issues, err := checkout.Push(ctx, r.manager.log, r.manager.Runtimes, gitTarget, destinations, opts)
	if err != nil {
		return nil, internalErr(err, "sync to "+string(target))
	}
	status := "synced"
	if len(issues) > 0 && !opts.BypassSafety {
		status = "blocked"
	} else if len(ds) == 0 {
		status = "empty"
	}
	if status != "blocked" {
		snapshot, err := checkout.DiffStatAndRepoStates(ctx, r.manager.log, r.manager.Runtimes, gitTarget)
		changed, recordErr := r.agentRuntime.RecordRepositorySummary(ctx, t, &snapshot)
		if changed {
			r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		}
		if err := errors.Join(err, recordErr); err != nil {
			return nil, internalErr(err, "push completed but refreshing task repository summary failed")
		}
	}
	return &SyncResult{Status: status, Branch: destinations[0].Branch, DiffStat: ds, SafetyIssues: issues}, nil
}

// QueueInput appends to a pending prompt, or sends immediately when waiting.
// Otherwise it persists a new pending prompt.
// It reports whether the prompt was queued rather than sent.
func (r *Lifecycle) QueueInput(ctx context.Context, prompt agent.Prompt) (bool, error) {
	// Busy tasks can enqueue independently of a blocked transport write. Only
	// the immediate waiting path needs the same serialization as SendInput.
	sendNow := r.entry.Task().GetState() == task.StateWaiting
	if sendNow {
		r.inputMu.Lock()
		defer r.inputMu.Unlock()
	}
	t := r.entry.Task()
	if len(prompt.Images) > 0 {
		if b := r.manager.Backends[t.Harness]; b != nil && !b.SupportsImages() {
			return false, badRequestf("%s does not support images", t.Harness)
		}
	}
	m := r.manager
	switch t.GetState() {
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan:
	default:
		return false, conflict("task is not ready for queued input")
	}
	m.inputMu.Lock()
	pending := r.entry.PendingInput()
	if pending != nil && (pending.Delivering || pending.Delivered) {
		m.inputMu.Unlock()
		return false, conflict("pending prompt is being delivered or was already sent")
	}
	if pending == nil && sendNow && t.GetState() == task.StateWaiting {
		m.inputMu.Unlock()
		return false, r.sendInputLocked(ctx, prompt)
	}
	defer m.inputMu.Unlock()
	if pending != nil {
		text := pending.Prompt.Text
		if text != "" && prompt.Text != "" {
			text += "\n"
		}
		prompt.Text = text + prompt.Text
		prompt.Images = append(slices.Clone(pending.Prompt.Images), prompt.Images...)
	} else {
		prompt.Images = slices.Clone(prompt.Images)
	}
	// New identity rejects edits that started before this content was appended.
	q := &PendingInput{ID: ksid.NewID(), Prompt: prompt}
	if err := m.updatePendingInputsLocked(map[*Entry]*PendingInput{r.entry: q}); err != nil {
		return false, internalErr(err, "save pending prompt")
	}
	m.NotifyTaskUpdate(r.entry.Task().ID)
	return true, nil
}

// CancelQueuedInput removes a prompt only if it still belongs to this task's queue.
func (r *Lifecycle) CancelQueuedInput(id ksid.ID) error {
	m := r.manager
	m.inputMu.Lock()
	defer m.inputMu.Unlock()
	q := r.entry.PendingInput()
	if q == nil || q.ID != id {
		return conflict("pending prompt was already sent or canceled")
	}
	if q.Delivering {
		return conflict("pending prompt is being delivered")
	}
	if err := m.updatePendingInputsLocked(map[*Entry]*PendingInput{r.entry: nil}); err != nil {
		return internalErr(err, "cancel pending prompt")
	}
	m.NotifyTaskUpdate(r.entry.Task().ID)
	return nil
}

func (r *Lifecycle) sendInputLocked(ctx context.Context, prompt agent.Prompt) error {
	t := r.entry.Task()
	if len(prompt.Images) > 0 {
		if b := r.manager.Backends[t.Harness]; b != nil && !b.SupportsImages() {
			return badRequestf("%s does not support images", string(t.Harness))
		}
	}
	if err := t.SendInput(ctx, prompt); err != nil {
		if !errors.Is(err, taskrun.ErrNoActiveSession) {
			return conflictErr(err, "send input")
		}
		var failedReconnect error
		if reconnectErr := r.reconnectForInput(); reconnectErr == nil { //nolint:contextcheck // The replacement session belongs to the server lifetime.
			if retryErr := t.SendInput(ctx, prompt); retryErr == nil {
				return nil
			} else if !errors.Is(retryErr, taskrun.ErrNoActiveSession) {
				return conflictErr(retryErr, "send input")
			} else {
				err = retryErr
			}
		} else {
			failedReconnect = reconnectErr
			r.manager.log.WarnContext(ctx, "reconnect before send input failed", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", t.GetState(), "err", reconnectErr)
		}
		r.manager.log.WarnContext(ctx, "no active session", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", t.GetState())
		return &NoSessionError{Err: err, ReconnectErr: failedReconnect}
	}
	return nil
}

func (r *Lifecycle) cancelScheduledPurge() {
	r.purgeMu.Lock()
	if r.purgeCancel != nil {
		r.purgeCancel()
		r.purgeCancel = nil
		r.purgeGeneration++
		r.purgeScheduled = false
	}
	r.purgeMu.Unlock()
}

func (r *Lifecycle) waitForPurgeDelay(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		changed := r.manager.Changed()
		if r.entry.Task().GetState() != task.StateStopped {
			return false
		}
		select {
		case <-timer.C:
			return true
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// waitForStopped waits until the task leaves the stopping state and reports
// whether it settled as stopped. It wakes on task-change announcements, so the
// stop that sets the state announces it; a purge must not depend on unrelated
// activity to observe the transition.
func (r *Lifecycle) waitForStopped(ctx context.Context) bool {
	for r.entry.Task().GetState() == task.StateStopping {
		changed := r.manager.Changed()
		if r.entry.Task().GetState() != task.StateStopping {
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
	return r.entry.Task().GetState() == task.StateStopped
}

// scheduleImportedRecovery waits until buffered inventory events are applied
// before stopping and reviving the selected retained instance.
func (r *Lifecycle) scheduleImportedRecovery(expectedState task.State) {
	r.wg.Go(func() {
		if err := r.manager.WaitForRuntimeImport(r.manager.serverCtx); err != nil {
			return
		}
		if err := r.recoverImportedSession(expectedState); err != nil {
			r.manager.log.Warn("startup recovery failed", "task", r.entry.Task().ID, "err", err)
		}
	})
}

// recoverImportedSession holds the lifecycle reservation while core records the
// recovery intent, stops the retained instance, and restores one execution.
func (r *Lifecycle) recoverImportedSession(expectedState task.State) error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}
	defer r.operationMu.Unlock()
	t := r.entry.Task()
	h, err := r.agentRuntime.RecoverRetainedSession(r.ctx, t, expectedState)
	if err != nil {
		r.entry.Finish(&task.Result{State: t.GetState(), Err: err})
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		return err
	}
	r.cancelScheduledPurge()
	r.entry.Reset()
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	r.watchSession(h)
	return nil
}

// reviveLocked transfers the held lifecycle reservation to the setup worker.
// Core accepts the durable revival intent before publishing provisioning.
func (r *Lifecycle) reviveLocked() {
	t := r.entry.Task()
	r.wg.Go(func() {
		defer r.operationMu.Unlock()
		ctx, span := trace.NewTask(r.ctx, "task.revive:"+t.ID.String())
		defer span.End()
		h, err := r.agentRuntime.ReviveTask(ctx, t)
		if err != nil {
			r.manager.log.WarnContext(ctx, "revive failed", "task", t.ID, "err", err)
			r.entry.Finish(&task.Result{State: t.GetState(), Err: internalErr(err, "revive task")})
			r.manager.NotifyTaskUpdate(r.entry.Task().ID)
			return
		}
		r.cancelScheduledPurge()
		r.entry.Reset()
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		r.watchSession(h)
	})
}

func (r *Lifecycle) reconnectForInput() error {
	if !r.operationMu.TryLock() {
		return conflict("task lifecycle operation is in progress")
	}
	defer r.operationMu.Unlock()
	t := r.entry.Task()
	if t.HasSession() {
		return nil
	}
	switch t.GetState() {
	case task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateRunning:
	default:
		return conflict("task is not ready for input")
	}
	// The replacement session survives the request that triggered attachment.
	previousResult := r.entry.Result()
	h, err := r.agentRuntime.Reconnect(r.manager.serverCtx, t)
	if err != nil {
		if t.HasSession() {
			return nil
		}
		return err
	}
	tlog := r.manager.log.With("task", t.ID, "instance", t.RuntimeInstanceID())
	h, err = r.agentRuntime.EnsureSession(r.manager.serverCtx, tlog, t, h)
	if err != nil {
		return err
	}
	r.entry.clearResult(previousResult)
	r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	r.watchSession(h)
	return nil
}

// reconnectImportedSession restores a session for a live runtime imported at startup.
func (r *Lifecycle) reconnectImportedSession() {
	t := r.entry.Task()
	primary := t.Primary()
	repoName := ""
	branch := ""
	if primary != nil {
		repoName = primary.Name
		branch = primary.Branch
	}
	tlog := r.manager.log.With("repo", repoName, "br", branch, "instance", t.RuntimeInstanceID())
	// The owner replays imported history without live effects, then records
	// the attached tail with repository probes and result-time summaries.
	previousResult := r.entry.Result()
	h, err := r.agentRuntime.Reconnect(r.ctx, t)
	if err != nil {
		tlog.Warn("auto-reconnect failed", "err", err)
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		return
	}
	r.wg.Go(func() {
		h, err = r.agentRuntime.EnsureSession(r.ctx, tlog, t, h)
		if err != nil {
			tlog.Warn("ensure session failed", "err", err)
			// Failed readiness cannot resolve a pending question or plan.
			t.SetStateIf(task.StateRunning, task.StateWaiting)
			r.manager.NotifyTaskUpdate(r.entry.Task().ID)
			return
		}
		r.entry.clearResult(previousResult)
		tlog.Debug("auto-reconnect succeeded")
		t.SetVNCPort(r.manager.Runtimes.VNCPort(r.ctx, t.RuntimeInstanceID()))
		if checkout := r.agentRuntime.Checkout; checkout != nil && (t.GetState() == task.StateWaiting || t.GetState() == task.StateAsking || t.GetState() == task.StateHasPlan) {
			snapshot, err := checkout.DiffStat(r.ctx, r.manager.log, r.manager.Runtimes, t.GitTarget())
			if err != nil {
				tlog.Warn("restore branch diff stat failed", "err", err)
			} else if _, err := r.agentRuntime.RecordRepositorySummary(r.ctx, t, &snapshot); err != nil {
				tlog.Warn("record restored branch diff stat failed", "err", err)
			}
		}
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		r.watchSession(h)
	})
}

// watchSession monitors the active session. A clean exit leaves the task
// waiting; an error records a terminal result and stops the runtime.
//
// It uses manager.serverCtx rather than r.ctx so it exits on server shutdown.
// Lifecycle work uses r.ctx to finish cleanly during shutdown.
func (r *Lifecycle) watchSession(h *taskrun.SessionHandle) {
	r.wg.Go(func() {
		t := r.entry.Task()
		ctx, tk := trace.NewTask(r.manager.serverCtx, "session.watch:"+t.ID.String())
		defer tk.End()
		done := h.Session.Done()
		select {
		case <-done:
			if ctx.Err() != nil {
				return
			}
			if t.SessionDone() != done {
				return
			}
			sessionErr := h.Session.Wait()
			claimed, finishErr := r.agentRuntime.FinishSession(ctx, t, h, sessionErr)
			if !claimed {
				return
			}
			primaryName := ""
			primaryBranch := ""
			if primary := t.Primary(); primary != nil {
				primaryName = primary.Name
				primaryBranch = primary.Branch
			}
			attrs := []any{"repo", primaryName, "br", primaryBranch, "instance", t.RuntimeInstanceID()}
			if sessionErr != nil || finishErr != nil {
				err := errors.Join(sessionErr, finishErr)
				r.manager.log.WarnContext(ctx, "session exited with error", append(attrs, "err", err)...)
				state := t.GetState()
				if state == task.StateCrashed || state == task.StateFailed {
					if state == task.StateCrashed {
						r.stopFailedSessionInstance(ctx, t, attrs)
					}
					if exitErr := t.LastExitError(); exitErr != "" {
						err = errors.Join(errors.New(exitErr), finishErr)
					}
					snapshot := t.Snapshot()
					r.entry.Finish(&task.Result{State: state, DiffStat: snapshot.DiffStat, CostUSD: snapshot.CostUSD, CostKnown: snapshot.CostKnown, CostComplete: snapshot.CostComplete, Duration: snapshot.Duration, NumTurns: snapshot.NumTurns, Usage: snapshot.Usage, AgentResult: t.LastAgentResult(), Err: err})
				}
			} else {
				r.manager.log.InfoContext(ctx, "session exited", attrs...)
			}

			r.manager.NotifyTaskUpdate(r.entry.Task().ID)
		case <-r.entry.Done():
		case <-ctx.Done():
		}
	})
}

func (r *Lifecycle) stopFailedSessionInstance(ctx context.Context, t *taskrun.Task, attrs []any) {
	id := t.RuntimeInstanceID()
	if id == "" {
		return
	}
	if err := r.manager.Runtimes.Stop(ctx, id); err != nil {
		r.manager.log.ErrorContext(ctx, "stop failed after session error", append(attrs, "err", err)...)
	}
}

// finishPurge transitions a settled task to purging and removes its runtime while
// operationMu is held. A Stop that still owns the incarnation publishes its
// terminal result and trailer first; otherwise the purge would finish first and
// the superseded stop would describe the task afterwards. Nothing else can start
// once the task is purging, so this hold adds no contention.
func (r *Lifecycle) finishPurge(ctx context.Context, t *taskrun.Task, stoppedImmediately bool) {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if !stoppedImmediately {
		if !t.SetStateIf(task.StateStopped, task.StatePurging) {
			r.manager.log.InfoContext(ctx, "scheduled purge cancelled", "task", t.ID, "state", t.GetState())
			return
		}
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	}
	r.cleanup(ctx, task.StatePurged)
	r.manager.log.InfoContext(ctx, "purge completed", "task", t.ID, "final_state", t.GetState())
}

func (r *Lifecycle) cleanup(ctx context.Context, reason task.State) {
	r.entry.Cleanup(func() {
		start := time.Now()
		t := r.entry.Task()
		if reason == task.StatePurged {
			t.DiscardRollup()
		}
		result := r.agentRuntime.Cleanup(ctx, t, reason)
		elapsed := time.Since(start).Round(time.Millisecond)
		if result.Err != nil {
			r.manager.log.ErrorContext(ctx, "cleanup failed", "task", t.ID, "reason", reason, "dur", elapsed, "err", result.Err)
		} else {
			r.manager.log.InfoContext(ctx, "cleanup done", "task", t.ID, "reason", reason, "dur", elapsed, "cost", result.CostUSD, "turns", result.NumTurns, "final_state", result.State)
		}
		r.entry.Finish(&result)
		r.manager.NotifyTaskUpdate(r.entry.Task().ID)
	})
}

func (r *Lifecycle) generateTitle() {
	t := r.entry.Task()
	r.wg.Go(func() { t.GenerateTitle(r.ctx, r.manager.log.With("task", t.ID), r.agentRuntime.NotifyTaskChange) })
}

var _ io.Closer = (*Lifecycle)(nil)
