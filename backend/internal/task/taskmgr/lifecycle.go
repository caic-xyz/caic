// Lifecycle executes operations for one registered task.

package taskmgr

import (
	"context"
	"errors"
	"io"
	"maps"
	"runtime/trace"
	"slices"
	"sync"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// Lifecycle coordinates state transitions, session watching, and user
// operations for one entry. Manager owns the entry registry and shared state.
type Lifecycle struct {
	manager      *Manager
	entry        *Entry
	agentRuntime task.AgentRuntime
	ctx          context.Context
	wg           sync.WaitGroup
}

var _ io.Closer = (*Lifecycle)(nil)

// Close waits for lifecycle-owned background work to finish.
func (r *Lifecycle) Close() error {
	r.wg.Wait()
	return nil
}

// Purge transitions the task to purging and removes its runtime resources.
func (r *Lifecycle) Purge(ctx context.Context) error {
	t := r.entry.Task()
	state, changed := t.SetStateIfAny(task.StatePurging,
		task.StateWaiting, task.StateAsking, task.StateHasPlan,
		task.StateRunning, task.StateStopping, task.StateStopped, task.StateCrashed)
	if !changed {
		return conflict("task is not running, waiting, stopped, or crashed")
	}
	r.manager.NotifyTaskChange()
	r.manager.log.InfoContext(ctx, "purge requested", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", state)
	r.wg.Go(func() {
		r.cleanup(r.ctx, task.StatePurged)
		r.manager.log.InfoContext(r.ctx, "purge completed", "task", t.ID, "final_state", t.GetState())
	})
	return nil
}

// Stop transitions the task to stopping and stops its runtime without purging it.
func (r *Lifecycle) Stop(ctx context.Context) error {
	t := r.entry.Task()
	state, changed := t.SetStateIfAny(task.StateStopping,
		task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StateRunning)
	if !changed {
		return conflict("task is not running or waiting")
	}
	r.manager.NotifyTaskChange()
	r.manager.log.InfoContext(ctx, "stop requested", "task", t.ID, "instance", t.RuntimeInstanceID(), "state", state)
	r.wg.Go(func() {
		r.agentRuntime.StopTask(r.ctx, t)
		r.manager.log.InfoContext(r.ctx, "stop completed", "task", t.ID, "instance", t.RuntimeInstanceID(), "final_state", t.GetState())
		r.manager.NotifyTaskChange()
	})
	return nil
}

// Revive restarts a stopped or crashed task.
func (r *Lifecycle) Revive() error {
	t := r.entry.Task()
	if _, changed := t.SetStateIfAny(task.StateProvisioning, task.StateStopped, task.StateCrashed); !changed {
		return conflict("task is not stopped or crashed")
	}
	r.entry.Reset()
	r.manager.NotifyTaskChange()
	r.wg.Go(func() {
		ctx, tk := trace.NewTask(r.ctx, "task.revive:"+t.ID.String())
		defer tk.End()
		h, err := r.agentRuntime.ReviveTask(ctx, t)
		if err != nil {
			r.manager.log.WarnContext(ctx, "revive failed", "task", t.ID, "err", err)
			t.SetState(task.StateFailed)
			r.entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "revive task")})
			r.manager.NotifyTaskChange()
			return
		}
		r.manager.NotifyTaskChange()
		r.watchSession(h)
	})
	return nil
}

// Restart starts a fresh agent session with prompt.
func (r *Lifecycle) Restart(ctx context.Context, prompt agent.Prompt) error {
	t := r.entry.Task()
	prevState, changed := t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan)
	if !changed {
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
			t.SetStateIf(task.StateStarting, prevState)
			return &Error{Kind: KindBadRequest, Msg: "no prompt provided and failed to read plan from instance", Err: err}
		}
	}
	h, err := r.agentRuntime.RestartSession(r.ctx, t, prompt) //nolint:contextcheck // session setup must finish during shutdown
	if err != nil {
		return internalErr(err, "restart session")
	}
	r.watchSession(h) //nolint:contextcheck // watcher uses the Manager lifetime.
	r.manager.NotifyTaskChange()
	return nil
}

// ClearContext starts a fresh idle agent session.
func (r *Lifecycle) ClearContext() error {
	t := r.entry.Task()
	if _, changed := t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan); !changed {
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
	r.manager.NotifyTaskChange()
	return nil
}

// Compact asks the active agent session to compact its context.
func (r *Lifecycle) Compact(ctx context.Context, instructions string) error {
	if err := r.entry.Task().SendCompact(ctx, instructions); err != nil {
		return conflictErr(err, "no active session to compact")
	}
	return nil
}

// SendInput forwards prompt to the agent session, reconnecting when needed.
func (r *Lifecycle) SendInput(ctx context.Context, prompt agent.Prompt) error {
	t := r.entry.Task()
	if len(prompt.Images) > 0 {
		if b := r.manager.Backends[t.Harness]; b != nil && !b.SupportsImages() {
			return badRequestf("%s does not support images", string(t.Harness))
		}
	}
	if err := t.SendInput(ctx, prompt); err != nil {
		if !errors.Is(err, task.ErrNoActiveSession) {
			return conflictErr(err, "send input")
		}
		var failedReconnect error
		if reconnectErr := r.reconnectForInput(); reconnectErr == nil { //nolint:contextcheck // watcher uses the Manager lifetime.
			if retryErr := t.SendInput(ctx, prompt); retryErr == nil {
				return nil
			} else if !errors.Is(retryErr, task.ErrNoActiveSession) {
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

// Start starts the runtime and agent session for a newly registered task.
func (r *Lifecycle) Start(ctx context.Context, resolvedGitHubToken string) error {
	t := r.entry.Task()
	h, err := r.agentRuntime.Start(ctx, t, resolvedGitHubToken)
	if err != nil {
		r.entry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "start task")})
		r.manager.publishTerminalReplay(ctx, r.entry)
		r.manager.NotifyTaskChange()
		return err
	}
	if t.Sudo {
		t.SetSudoPassword(r.manager.SudoPassword(ctx, t))
	}
	r.manager.NotifyTaskChange()
	r.watchSession(h) //nolint:contextcheck // watcher uses the Manager lifetime.
	return nil
}

// Fork creates a new task from this task's retained runtime instance.
func (r *Lifecycle) Fork(ctx context.Context, p ForkParams) (string, error) { //nolint:gocritic // ForkParams is a request-shaped value bag
	source := r.entry.Task()
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

	forkHarness := source.Harness
	forkModel := source.Model
	forkEffort := source.Effort
	if p.Harness != "" {
		forkHarness = p.Harness
		backend, ok := r.manager.Backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(p.Harness))
		}
		if p.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
			return "", badRequestf("unsupported model for %s: %s", string(p.Harness), p.Model)
		}
		forkModel = p.Model
		forkEffort = p.Effort
	} else if p.Model != "" {
		backend, ok := r.manager.Backends[forkHarness]
		if !ok {
			return "", badRequestf("unknown harness: %s", string(source.Harness))
		}
		if !slices.Contains(backend.ModelInventory().IDs(), p.Model) {
			return "", badRequestf("unsupported model for %s: %s", string(source.Harness), p.Model)
		}
		forkModel = p.Model
		forkEffort = p.Effort
	}

	sourceRepoNames := make(map[string]struct{}, len(sourceRepos))
	for _, repoMount := range sourceRepos {
		sourceRepoNames[repoMount.Name] = struct{}{}
	}
	var extraMounts []task.RepoMount
	for _, rs := range p.ExtraRepos {
		if _, overlap := sourceRepoNames[rs.Name]; overlap {
			return "", badRequestf("extraRepos contains repo already in source task: %s", rs.Name)
		}
		checkout, ok := r.manager.Checkouts.Checkout(rs.Name)
		if !ok {
			return "", badRequestf("unknown extra repo: %s", rs.Name)
		}
		extraMounts = append(extraMounts, task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: checkout.Dir, ContainerPath: r.manager.containerPathForRepo(rs.Name)})
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
		Provider:          r.manager.provider,
	}
	t.SetTitle(p.Prompt.Text)
	forkEntry := r.manager.NewEntry(t, nil)
	r.manager.insertEntry(t.ID.String(), forkEntry)
	forkEntry.Lifecycle.generateTitle()

	forkEntry.Lifecycle.wg.Go(func() { //nolint:contextcheck // fork must finish during shutdown
		ctx, tk := trace.NewTask(forkEntry.Lifecycle.ctx, "task.fork:"+source.ID.String()+"->"+t.ID.String())
		defer tk.End()
		if err := r.manager.allocateBranches(ctx, t, mounts, len(sourceRepos)); err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "allocate fork branch")})
			r.manager.NotifyTaskChange()
			return
		}
		metadata := maps.Clone(r.manager.runtimeMetadata)
		if metadata == nil {
			metadata = runtime.Metadata{}
		}
		maps.Copy(metadata, task.MakeMetadata(t))
		forkOpts := &runtime.ForkOptions{RuntimeName: source.RuntimeName, Display: p.Display, Tailscale: p.Tailscale, USB: p.USB, Sudo: p.Sudo, Metadata: metadata, Harness: forkHarness, Mounts: slices.Clone(source.Mounts), MaxCPUs: source.MaxCPUs}
		if p.ResolvedGitHubToken != "" {
			forkOpts.ExtraEnv = []string{"GITHUB_TOKEN=" + p.ResolvedGitHubToken}
		}
		h, err := r.agentRuntime.ForkTask(ctx, source, t, forkOpts, p.ResolvedGitHubToken)
		if err != nil {
			forkEntry.Finish(&task.Result{State: task.StateFailed, Err: internalErr(err, "fork task")})
			r.manager.publishTerminalReplay(ctx, forkEntry)
			r.manager.NotifyTaskChange()
			return
		}
		if t.Sudo {
			t.SetSudoPassword(r.manager.SudoPassword(ctx, t))
		}
		r.manager.NotifyTaskChange()
		forkEntry.Lifecycle.watchSession(h)
	})
	return t.ID.String(), nil
}

// Sync pushes the task branch to its configured origin or default branch.
func (r *Lifecycle) Sync(ctx context.Context, target SyncTarget, force bool) (*SyncResult, error) {
	t := r.entry.Task()
	switch t.GetState() { //nolint:exhaustive // only terminal/blocked states are relevant
	case task.StatePending:
		return nil, conflict("task has no instance yet")
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateCrashed, task.StateFailed, task.StatePurged:
		return nil, conflict("task is in a terminal state")
	}
	checkout := r.agentRuntime.Checkout
	if checkout == nil {
		return nil, badRequestf("task has no checkout")
	}
	branch := ""
	if primary := t.Primary(); primary != nil {
		branch = primary.Branch
	}
	if target == SyncTargetDefault {
		if force {
			return nil, badRequestf("force is not supported for default-branch sync")
		}
		message := t.Title()
		if message == "" {
			message = t.InitialPrompt.Text
		}
		ds, issues, err := checkout.SyncToDefault(ctx, r.manager.log, r.manager.Runtimes, t, message)
		if err != nil {
			return nil, internalErr(err, "sync to default")
		}
		status := "synced"
		if len(ds) == 0 {
			status = "empty"
		} else if len(issues) > 0 {
			status = "blocked"
		}
		return &SyncResult{Status: status, Branch: checkout.BaseBranch, DiffStat: ds, SafetyIssues: issues}, nil
	}
	ds, issues, err := checkout.SyncToOrigin(ctx, r.manager.log, r.manager.Runtimes, t, force)
	if err != nil {
		return nil, internalErr(err, "sync to origin")
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !force {
		status = "blocked"
	}
	return &SyncResult{Status: status, Branch: branch, DiffStat: ds, SafetyIssues: issues}, nil
}

func (r *Lifecycle) reconnectForInput() error {
	t := r.entry.Task()
	if t.HasSession() {
		return nil
	}
	h, err := r.agentRuntime.Reconnect(r.ctx, t, false)
	if err != nil {
		if t.HasSession() {
			return nil
		}
		return err
	}
	tlog := r.manager.log.With("task", t.ID, "instance", t.RuntimeInstanceID())
	h, err = r.agentRuntime.EnsureSession(r.ctx, tlog, t, h)
	if err != nil {
		return err
	}
	r.watchSession(h)
	return nil
}

// watchSession monitors the active session. A clean exit leaves the task
// waiting; an error records a terminal result and stops the runtime.
//
// It uses manager.serverCtx rather than r.ctx so it exits on server shutdown.
// Lifecycle work uses r.ctx to finish cleanly during shutdown.
func (r *Lifecycle) watchSession(h *task.SessionHandle) {
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
			t.DetachSession()
			sessionErr := h.Session.Wait()
			h.CloseMsgCh()
			<-h.DispatchDone
			if h.Log != nil {
				_ = h.Log.Close()
			}
			primaryName := ""
			primaryBranch := ""
			if primary := t.Primary(); primary != nil {
				primaryName = primary.Name
				primaryBranch = primary.Branch
			}
			attrs := []any{"repo", primaryName, "br", primaryBranch, "instance", t.RuntimeInstanceID()}
			if sessionErr != nil {
				r.manager.log.WarnContext(ctx, "session exited with error", append(attrs, "err", sessionErr)...)
				if t.RecordSessionCrash(ctx, sessionErr) {
					r.stopFailedSessionInstance(ctx, t, attrs)
					crashErr := sessionErr
					if exitErr := t.LastExitError(); exitErr != "" {
						crashErr = errors.New(exitErr)
					}
					costUSD, numTurns, duration, usage, _ := t.LiveStats()
					result := &task.Result{State: task.StateCrashed, DiffStat: t.LiveDiffStat(), CostUSD: costUSD, Duration: duration, NumTurns: numTurns, Usage: usage, AgentResult: t.LastAgentResult(), Err: crashErr}
					r.entry.Finish(result)
					if err := r.manager.Logs.WriteTaskResultTrailer(t, result); err != nil {
						r.manager.log.WarnContext(ctx, "write crashed task trailer failed", append(attrs, "err", err)...)
					}
					r.manager.publishTerminalReplay(ctx, r.entry)
				} else if t.RecordSessionFailure(ctx, sessionErr) {
					failureErr := sessionErr
					if exitErr := t.LastExitError(); exitErr != "" {
						failureErr = errors.New(exitErr)
					}
					costUSD, numTurns, duration, usage, _ := t.LiveStats()
					result := &task.Result{State: task.StateFailed, DiffStat: t.LiveDiffStat(), CostUSD: costUSD, Duration: duration, NumTurns: numTurns, Usage: usage, AgentResult: t.LastAgentResult(), Err: failureErr}
					r.entry.Finish(result)
					if err := r.manager.Logs.WriteTaskResultTrailer(t, result); err != nil {
						r.manager.log.WarnContext(ctx, "write failed task trailer failed", append(attrs, "err", err)...)
					}
					r.manager.publishTerminalReplay(ctx, r.entry)
				}
			} else {
				r.manager.log.InfoContext(ctx, "session exited", attrs...)
				t.SetStateIf(task.StateRunning, task.StateWaiting)
			}
			r.manager.NotifyTaskChange()
		case <-r.entry.Done():
		case <-ctx.Done():
		}
	})
}

func (r *Lifecycle) stopFailedSessionInstance(ctx context.Context, t *task.Task, attrs []any) {
	id := t.RuntimeInstanceID()
	if id == "" {
		return
	}
	if err := r.manager.Runtimes.Stop(ctx, id); err != nil {
		r.manager.log.ErrorContext(ctx, "stop failed after session error", append(attrs, "err", err)...)
	}
}

func (r *Lifecycle) cleanup(ctx context.Context, reason task.State) {
	r.entry.Cleanup(func() {
		start := time.Now()
		t := r.entry.Task()
		result := r.agentRuntime.Cleanup(ctx, t, reason)
		elapsed := time.Since(start).Round(time.Millisecond)
		if result.Err != nil {
			r.manager.log.ErrorContext(ctx, "cleanup failed", "task", t.ID, "reason", reason, "dur", elapsed, "err", result.Err)
		} else {
			r.manager.log.InfoContext(ctx, "cleanup done", "task", t.ID, "reason", reason, "dur", elapsed, "cost", result.CostUSD, "turns", result.NumTurns, "final_state", result.State)
		}
		r.entry.Finish(&result)
		r.manager.NotifyTaskChange()
	})
}

func (r *Lifecycle) generateTitle() {
	r.wg.Go(func() { r.entry.Task().GenerateTitle(r.ctx) })
}
