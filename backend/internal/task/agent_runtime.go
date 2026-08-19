// AgentRuntime owns runtime and coding-agent sessions for a task.

package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// MakeMetadata builds runtime metadata for a task instance.
//
// MetadataLegacyTaskID is kept alongside MetadataTaskID for compatibility with
// existing instances and event filters. Once old instances have cycled out, the
// legacy metadata key can be removed.
func MakeMetadata(t *Task) runtime.Metadata {
	metadata := runtime.Metadata{
		runtime.MetadataTaskID:       t.ID.String(),
		runtime.MetadataLegacyTaskID: t.ID.String(),
		runtime.MetadataHarness:      string(t.Harness),
	}
	if t.GitHubTokenEnabled() {
		metadata[runtime.MetadataGitHubToken] = "true"
	}
	return metadata
}

// mutatingTools lists tool names whose execution may change files in the
// instance, warranting a diff stat refresh after their result arrives.
var mutatingTools = map[string]struct{}{
	"Bash":         {},
	"Edit":         {},
	"Write":        {},
	"NotebookEdit": {},
}

// AgentRuntime owns task runtime setup, agent sessions, log persistence, message
// dispatch, cleanup, restart, reconnect, revive, and fork operations.
type AgentRuntime struct {
	// Immutable.
	Backends         map[harness.Name]agent.Backend
	LogStore         *taskslog.Store
	LogPath          *taskslog.Path
	Runtimes         *runtime.Router
	Log              *slog.Logger
	NotifyTaskChange func()

	Checkout            *repo.Checkout // nil for no-repository tasks
	RuntimeMetadata     runtime.Metadata
	RuntimeStartTimeout time.Duration // Timeout for instance start (image pull). Must be non-zero.
}

// Reconnect reattaches to a running relay, or starts a new agent session
// resuming the previous conversation if no relay is available. Returns the
// SessionHandle so the caller can start a session watcher.
//
// Strategy:
//  1. Check if the relay daemon is alive (Unix socket exists in instance).
//  2. If alive, attach to the relay. This is the preferred path because it
//     reconnects to the still-running agent process with zero message loss.
//  3. If attaching fails (relay died between check and attach), fall back to
//     starting a new agent session with --resume to continue the conversation.
//  4. If both fail, revert to StateWaiting so the user can retry or purge.
//
// State transitions:
//   - Relay attach: keeps StateWaiting/StateAsking if agent already finished its
//     turn; transitions to StateRunning only if the agent was mid-output.
//   - --resume fallback: always transitions to StateRunning since a new agent
//     process is started.
//   - All-fail: reverts to StateWaiting.
func (r *AgentRuntime) Reconnect(ctx context.Context, t *Task, skipSideEffects bool) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.reconnect:"+t.ID.String())
	defer task.End()

	if t.HasSession() {
		return nil, errors.New("session already active")
	}
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, errors.New("no instance to reconnect to")
	}
	sessionID := t.GetSessionID()
	if harness.RequiresResumeSessionID(t.Harness) && sessionID == "" {
		return nil, fmt.Errorf("%s session ID missing; cannot reconnect", t.Harness)
	}
	// Remember the state inferred from restored messages so we don't
	// blindly override it to StateRunning for an idle relay.
	prevState := t.GetState()

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, skipSideEffects)

	// Reconnect resumes an existing session, so append only after Reopen
	// validates the existing file's authoritative header. A missing or corrupt
	// log must not be replaced because the running relay's format is unknown.
	log, err := r.reopenLog(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, err
	}

	// Attach to the live relay. If the relay is dead, the session is lost.
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	// Only transition to StateRunning if the restored messages indicate
	// the agent was still producing output (no trailing ResultMessage).
	// If the agent had already completed its turn, keep the inferred
	// StateWaiting/StateAsking so the UI shows the correct status.
	if prevState != taskslog.StateWaiting && prevState != taskslog.StateAsking {
		t.SetState(taskslog.StateRunning)
	}
	target := t.RuntimeConnectionTarget()
	session, err := r.Backends[t.Harness].AttachRelay(ctx, &agent.Options{
		Logger:             r.Log,
		Target:             target,
		RelayOffset:        t.RelayOffsetValue(),
		ResumeSessionID:    sessionID,
		Effort:             t.Effort,
		PendingUserActions: t.PendingUserActions(),
		MsgCh:              msgCh,
		Log:                log,
	})
	if err != nil {
		_ = log.Close()
		close(msgCh)
		<-dispatchDone
		t.SetState(taskslog.StateWaiting)
		r.Log.Error("attach relay failed", "br", primaryBranch, "instance", instanceID, "err", err)
		return nil, fmt.Errorf("reconnect: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)
	return h, nil
}

// EnsureSession waits briefly for h to confirm it's alive. If the session
// exits within 10 seconds (agent had already finished), it detaches and
// starts a fresh idle relay so the task can accept new prompts.
func (r *AgentRuntime) EnsureSession(ctx context.Context, tlog *slog.Logger, t *Task, h *SessionHandle) (*SessionHandle, error) {
	select {
	case <-h.Done():
		// Session exited immediately (agent was already done).
		t.DetachSession()
		err := h.Drain()
		_ = h.Log.Close()
		tlog.Info("attached session exited, starting idle relay", "err", err)
		if s := t.GetState(); s == taskslog.StateStopping || s == taskslog.StateStopped || s == taskslog.StatePurged {
			return nil, fmt.Errorf("task is %s", s)
		}
		t.SetState(taskslog.StateWaiting)
		return r.StartSession(ctx, t, agent.Prompt{})
	case <-time.After(10 * time.Second):
		// Session is alive — all good.
		return h, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StartSession starts a fresh relay+agent session on an existing instance.
// If prompt is non-empty, it is sent as the initial input and the task
// transitions to StateRunning. If prompt is empty, the agent starts idle
// and the task stays in its current state (typically StateWaiting).
func (r *AgentRuntime) StartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	if t.RuntimeInstanceID() == "" {
		return nil, errors.New("no instance")
	}
	log, err := r.openLog(t)
	if err != nil {
		return nil, err
	}
	h, err := r.startSessionWithLog(ctx, t, prompt, log)
	if err != nil {
		return nil, errors.Join(err, log.Close())
	}
	return h, nil
}

// RestartSession closes the current agent session and starts a fresh one in
// the same instance with a new prompt. Returns the new SessionHandle so the
// caller can start a session watcher.
func (r *AgentRuntime) RestartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	return r.replaceSession(ctx, t, prompt, replaceSessionRestart)
}

// ClearContextSession closes the current agent session and starts a fresh one
// in the same instance without a prompt. The task transitions to StateWaiting
// so the user can send a new message when ready.
func (r *AgentRuntime) ClearContextSession(ctx context.Context, t *Task) (*SessionHandle, error) {
	return r.replaceSession(ctx, t, agent.Prompt{}, replaceSessionClearContext)
}

// Start performs branch/instance setup, starts the agent session, and sends
// the initial prompt. Returns the SessionHandle so the caller can start a
// session watcher.
//
// Sequence:
//  1. Create a new git branch from origin/<BaseBranch> (or the local branch if not on origin).
//  2. Start a runtime instance on that branch.
//  3. Deploy the relay script and launch the agent (claude) via the
//     relay daemon. The relay owns the agent's stdin/stdout and persists
//     across transport disconnects.
//  4. Send the initial prompt to the agent.
//
// The session is left open for follow-up messages via SendInput.
func (r *AgentRuntime) Start(ctx context.Context, t *Task, resolvedGitHubToken string) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.start:"+t.ID.String())
	defer task.End()

	if r.Checkout != nil {
		t.SetState(taskslog.StateBranching)
	}
	// The Manager has already assigned every repo's branch name (the branch itself
	// is created below, concurrently with launch), so the log can open with the
	// durable, branch-derived filename and persist output from its first line.
	log, err := r.openLog(t)
	if err != nil {
		t.recordStartupFailure(ctx, err)
		return nil, err
	}

	tStart := time.Now()
	// 1. Create branch (serialized) + start instance (concurrent).
	r.Log.Info("setup task")
	region := trace.StartRegion(ctx, "setup")
	metadata := maps.Clone(r.RuntimeMetadata)
	if metadata == nil {
		metadata = runtime.Metadata{}
	}
	maps.Copy(metadata, MakeMetadata(t))
	sr, err := r.setup(ctx, t, metadata, resolvedGitHubToken, log)
	region.End()
	if err != nil {
		return nil, r.finishStartupFailure(ctx, t, log, err)
	}
	t.SetRuntimeConnectionInfo(sr.InstanceID, sr.AgentTarget, sr.TailscaleFQDN, sr.TailscaleAuthURL, r.Runtimes.VNCPort(ctx, sr.InstanceID))
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Log.Info("checkout", "msg", "ready", "br", primaryBranch, "instance", sr.InstanceID, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(taskslog.StateStarting)
	var msgCh chan agent.ParsedMessage
	var dispatchDone <-chan struct{}
	{
		region := trace.StartRegion(ctx, "dispatch-init")
		msgCh, dispatchDone = r.startMessageDispatch(ctx, t, false)
		region.End()
	}

	tSession := time.Now()
	tlog := r.Log.With("br", primaryBranch, "instance", sr.InstanceID)
	tlog.Info("starting session", "hns", t.Harness)
	region = trace.StartRegion(ctx, "agent-session")
	target := sr.AgentTarget
	session, err := r.Backends[t.Harness].Start(ctx, &agent.Options{
		Logger:        r.Log,
		Target:        target,
		Dir:           r.runtimeDir(t),
		Model:         t.Model,
		Effort:        t.Effort,
		InitialPrompt: t.InitialPrompt,
		MsgCh:         msgCh,
		Log:           log,
	})
	region.End()
	if err != nil {
		close(msgCh)
		<-dispatchDone
		tlog.Error("session start failed", "err", err)
		return nil, r.finishStartupFailure(ctx, t, log, err)
	}

	// Store handle so SendInput can reach it.
	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(t.InitialPrompt), false)
	// Use SetStateIf so that a fast agent subprocess that already
	// produced a result (and was processed by the dispatch goroutine
	// via addMessage) isn't overwritten back to Running.
	t.SetStateIf(taskslog.StateStarting, taskslog.StateRunning)
	tlog.Info("agent running", "session_dur", time.Since(tSession), "total_startup_dur", time.Since(tStart))
	return h, nil
}

// Cleanup is the single shutdown path for a task (Flow 1 in the relay
// shutdown protocol — see package agent). It sends the null-byte sentinel
// to trigger graceful agent exit, then purges the instance.
//
// This is only called for intentional purge (user action or instance
// death), never during backend restart. On restart, the relay daemon stays
// alive and the server reconnects during task import.
//
// Steps:
//  1. Detach the session handle from the task.
//  2. If a session exists: Stop (sends \x00, waits up to 20s), then Close.
//  3. Set task state to reason (StatePurged or StateFailed).
//  4. Purge the instance (stop + remove + cleanup git remotes/runtime config).
//  5. If graceful wait timed out, drain session now (runtime connection severed).
//  6. Close msgCh and log, write log trailer.
//  7. Build and return Result.
func (r *AgentRuntime) Cleanup(ctx context.Context, t *Task, reason taskslog.State) taskslog.Result {
	ctx, task := trace.NewTask(ctx, "task.cleanup:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", name)
	tlog.InfoContext(ctx, "cleanup starting", "reason", reason, "state", t.GetState(), "has_session", t.HasSession())

	// Graceful shutdown: send stop sentinel so the relay sends SIGINT.
	// Stats come from live accumulators updated by startMessageDispatch.
	gStart := time.Now()
	h, gsErr := t.GracefulStopSession(ctx, 20*time.Second)
	if h != nil {
		if gsErr != nil {
			tlog.WarnContext(ctx, "graceful stop timed out", "err", gsErr, "dur", time.Since(gStart).Round(time.Millisecond))
			r.logRelayDiag(ctx, tlog, t.RuntimeConnectionTarget())
		} else {
			tlog.DebugContext(ctx, "cleanup: graceful stop succeeded", "dur", time.Since(gStart).Round(time.Millisecond))
		}
	}

	// Positively confirm the branch is empty against the live instance before
	// deleting it below. branchConfirmedEmpty is set only when this cleanup
	// fetched the instance's branch diff and observed it empty. A lost or absent
	// signal (dead instance, fetch failure, or no instance at all) must never
	// authorize deletion: unsynced work lives only in the container's branch, and
	// the host branch's own commit count says nothing about it.
	branchConfirmedEmpty := false
	if reason == taskslog.StatePurged && !t.DiffCreated() && name != "" && r.Checkout != nil {
		ds, err := r.branchDiffStat(ctx, t)
		switch {
		case err != nil:
			tlog.WarnContext(ctx, "verify empty task branch failed", "err", err)
		case len(ds) > 0:
			t.SetLiveDiffStat(ds)
		default:
			branchConfirmedEmpty = true
		}
	}

	t.SetState(reason)

	runtimeRemovedOrAbsent := name == ""
	if name != "" {
		tlog.InfoContext(ctx, "cleanup: purging instance")
		pStart := time.Now()
		timeout := time.Minute
		if r.Checkout != nil {
			timeout = r.Checkout.GitTimeout
		}
		purgeCtx, purgeCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		err := r.Runtimes.Purge(purgeCtx, name)
		purgeCancel()
		if err != nil {
			tlog.WarnContext(ctx, "purge instance failed", "err", err, "dur", time.Since(pStart).Round(time.Millisecond))
		} else {
			runtimeRemovedOrAbsent = true
			tlog.DebugContext(ctx, "cleanup: instance purged", "dur", time.Since(pStart).Round(time.Millisecond))
		}
	} else {
		tlog.DebugContext(ctx, "cleanup: no instance to purge", "name", name)
	}

	// Drain the session: if graceful stop timed out, the instance purge
	// above severed the runtime connection so this unblocks.
	if h != nil {
		dStart := time.Now()
		if err := h.Drain(); err != nil {
			tlog.DebugContext(ctx, "cleanup: session drain returned error", "err", err)
		}
		tlog.DebugContext(ctx, "cleanup: session drained", "dur", time.Since(dStart).Round(time.Millisecond))
	}

	res := taskslog.Result{
		State:       reason,
		AgentResult: t.LastAgentResult(),
	}
	if liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats(); liveCost > 0 {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.Duration = liveDur
		res.Usage = liveUsage
	}
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	if reason == taskslog.StatePurged && runtimeRemovedOrAbsent && branchConfirmedEmpty {
		if r.Checkout != nil {
			r.Checkout.DeleteUnmodifiedTaskBranches(ctx, r.Log, t)
		}
	}
	var log agent.LogSink
	if h != nil {
		log = h.Log
	} else {
		// Task was stopped before purge: the session handle (and its Log) was
		// released by StopTask. Reopen the log for appending so we can write
		// the caic_result trailer; without it the task would load as "failed"
		// on the next server restart instead of "purged".
		tlog.DebugContext(ctx, "cleanup: no session handle, reopening log for trailer")
		var reopenErr error
		log, reopenErr = r.reopenLog(t)
		if reopenErr != nil {
			tlog.WarnContext(ctx, "reopen log for trailer failed", "err", reopenErr)
		}
	}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	if trailerErr != nil {
		tlog.WarnContext(ctx, "write log trailer failed", "err", trailerErr)
	}
	if log != nil {
		if trailerErr != nil {
			if err := log.Close(); err != nil {
				tlog.WarnContext(ctx, "close log failed", "err", err)
			}
		} else if err := r.compressLog(log, reason); err != nil {
			tlog.WarnContext(ctx, "compress task log failed", "err", err)
		} else {
			tlog.DebugContext(ctx, "cleanup: log trailer written and closed")
		}
	}
	tlog.InfoContext(ctx, "cleanup done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns, "reason", reason)
	return res
}

// StopTask gracefully shuts down the agent session and stops the instance
// without removing it. The instance can be revived later. Unlike Cleanup,
// this preserves git remotes and runtime config.
func (r *AgentRuntime) StopTask(ctx context.Context, t *Task) {
	ctx, task := trace.NewTask(ctx, "task.stop:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", name)
	tlog.InfoContext(ctx, "stop starting", "state", t.GetState())
	if _, changed := t.SetStateUnless(taskslog.StateStopping, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StateStopped); !changed {
		tlog.InfoContext(ctx, "stop skipped", "state", t.GetState())
		return
	}

	// Graceful shutdown: send stop sentinel so the relay sends SIGINT.
	gStart := time.Now()
	h, gsErr := t.GracefulStopSession(ctx, 20*time.Second)
	if h != nil {
		if gsErr != nil {
			tlog.WarnContext(ctx, "graceful stop timed out", "err", gsErr, "dur", time.Since(gStart).Round(time.Millisecond))
			r.logRelayDiag(ctx, tlog, t.RuntimeConnectionTarget())
		} else {
			tlog.DebugContext(ctx, "stop: graceful stop succeeded", "dur", time.Since(gStart).Round(time.Millisecond))
		}
	}

	tlog.InfoContext(ctx, "stop: stopping instance")
	if name != "" {
		cStart := time.Now()
		if err := r.Runtimes.Stop(ctx, name); err != nil {
			tlog.WarnContext(ctx, "stop: instance Stop failed", "err", err, "dur", time.Since(cStart).Round(time.Millisecond))
		} else {
			tlog.DebugContext(ctx, "stop: instance Stop succeeded", "dur", time.Since(cStart).Round(time.Millisecond))
		}
	} else {
		tlog.DebugContext(ctx, "stop: no instance to stop", "name", name)
	}

	// Drain session after instance is stopped, then wait for the dispatch
	// goroutine to finish processing all buffered messages so that t.msgs
	// is complete before the state transitions to StateStopped.
	if h != nil {
		dStart := time.Now()
		if err := h.Drain(); err != nil {
			tlog.DebugContext(ctx, "stop: session drain returned error", "err", err)
		}
		tlog.DebugContext(ctx, "stop: session drained", "dur", time.Since(dStart).Round(time.Millisecond))
	}

	if _, changed := t.SetStateUnless(taskslog.StateStopped, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateCrashed, taskslog.StateFailed); !changed {
		if h != nil && h.Log != nil {
			_ = h.Log.Close()
		}
		tlog.InfoContext(ctx, "stop abandoned", "state", t.GetState())
		return
	}

	// Write log trailer so the task reloads as "stopped" (not "failed")
	// after a server restart, preserving live stats for the UI.
	res := taskslog.Result{State: taskslog.StateStopped, AgentResult: t.LastAgentResult()}
	if liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats(); liveCost > 0 {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.Duration = liveDur
		res.Usage = liveUsage
	}
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	var log agent.LogSink
	if h != nil {
		log = h.Log
	}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	if trailerErr != nil {
		tlog.WarnContext(ctx, "write log trailer failed", "err", trailerErr)
	}
	var closeErr error
	if log != nil {
		closeErr = log.Close()
		if closeErr != nil {
			tlog.WarnContext(ctx, "close log failed", "err", closeErr)
		}
	}
	tlog.InfoContext(ctx, "stop done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns)
}

// ReviveTask restarts a stopped or crashed instance and resumes the agent session.
// The instance's filesystem is preserved from the previous run.
func (r *AgentRuntime) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.revive:"+t.ID.String())
	defer task.End()

	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, errors.New("no instance to revive")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", instanceID)

	// 1. Revive the instance.
	if state, changed := t.SetStateIfAny(taskslog.StateProvisioning, taskslog.StateStopped, taskslog.StateCrashed, taskslog.StateProvisioning); !changed {
		return nil, fmt.Errorf("cannot revive in state %s", state)
	}
	tlog.Info("reviving instance")
	tlog.Debug("checkout", "msg", "calling instance.Revive")
	if err := r.Runtimes.Revive(ctx, instanceID); err != nil {
		tlog.Error("checkout", "msg", "Revive failed", "err", err)
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("revive instance: %w", err), nil)
	}
	tlog.Debug("checkout", "msg", "Revive succeeded", "instance", instanceID)
	t.SetVNCPort(r.Runtimes.VNCPort(ctx, instanceID))

	// 2. Start a new relay with --resume to continue the previous session.
	// skipSideEffects=true: --resume replays all historical messages and
	// each would trigger fetch+diff+title if side effects were enabled.
	// Instead we do a single BranchDiffStat at the end.
	t.SetState(taskslog.StateStarting)
	tlog.Info("resuming session after revive", "sess", t.GetSessionID())

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, true)
	log, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("open log: %w", err), nil)
	}

	t.SetState(taskslog.StateRunning)
	target := t.RuntimeConnectionTarget()
	session, err := r.Backends[t.Harness].Start(ctx, &agent.Options{
		Logger:          r.Log,
		Target:          target,
		Dir:             r.runtimeDir(t),
		Model:           t.Model,
		Effort:          t.Effort,
		ResumeSessionID: t.GetSessionID(),
		MsgCh:           msgCh,
		Log:             log,
	})
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("resume session after revive: %w", err), log)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)

	// 3. If --resume exits immediately (previous session was complete),
	// start a fresh idle relay so the task can accept new prompts.
	h, err = r.EnsureSession(ctx, tlog, t, h)
	if err != nil {
		return nil, r.finishReviveFailure(ctx, t, err, nil)
	}

	// 4. Compute host-side diff stat once.
	if r.Checkout != nil {
		if ds := r.Checkout.BranchDiffStat(ctx, r.Log, r.Runtimes, t); len(ds) > 0 {
			t.SetLiveDiffStat(ds)
		}
	}
	tlog.Info("agent ready after revive", "state", t.GetState())
	return h, nil
}

// ForkTask snapshots the source task's instance and starts an idle agent
// session in the forked instance. The new task must already have its ID,
// Harness, Model, and other immutable fields set. The method fills in
// Runtime, Repos[*].Branch, and starts the session.
func (r *AgentRuntime) ForkTask(ctx context.Context, source, fork *Task, forkOpts *runtime.ForkOptions, resolvedGitHubToken string) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.fork:"+source.ID.String()+"->"+fork.ID.String())
	defer task.End()

	sourceInstanceID := source.RuntimeInstanceID()
	if sourceInstanceID == "" {
		return nil, errors.New("source task has no instance")
	}

	var sourcePrimaryBranch string
	if p := source.Primary(); p != nil {
		sourcePrimaryBranch = p.Branch
	}
	tlog := r.Log.With("src_br", sourcePrimaryBranch, "src_instance", sourceInstanceID)

	// Every fork branch — primary and extras alike — was assigned by the Manager
	// before ForkTask, so the log can open with a correct metadata header up
	// front and provisioning output is durable from the first line.
	fork.SetState(taskslog.StateProvisioning)
	forkBranch := ""
	if p := fork.Primary(); p != nil {
		forkBranch = p.Branch
	}
	// Build the full fork repo set: every fork repo with its reserved destination
	// primary branch. Repos already in the source instance carry that instance's
	// current branch (so the runtime can validate against the snapshot); new repos
	// carry the host branch to push — their base branch, or the repo's upstream
	// default when empty.
	srcBranch := make(map[string]string) // GitRoot -> source instance branch
	for _, r := range source.RuntimeRepos() {
		srcBranch[r.GitRoot] = r.Branch
	}
	forkMounts := fork.ReposSnapshot()
	specs := make([]runtime.ForkRepo, len(forkMounts))
	for i, r := range forkMounts {
		spec := runtime.ForkRepo{GitRoot: r.GitRoot, ContainerPath: r.ContainerPath, DestPrimary: r.Branch}
		if b, ok := srcBranch[r.GitRoot]; ok {
			spec.SourceBranches = []string{b}
		} else if r.BaseBranch != "" {
			spec.SourceBranches = []string{r.BaseBranch}
		}
		specs[i] = spec
	}
	forkOpts.Repos = specs
	metadata := maps.Clone(r.RuntimeMetadata)
	if metadata == nil {
		metadata = runtime.Metadata{}
	}
	maps.Copy(metadata, forkOpts.Metadata)
	maps.Copy(metadata, MakeMetadata(fork))
	forkOpts.Metadata = metadata

	log, err := r.openLog(fork)
	if err != nil {
		fork.recordStartupFailure(ctx, err)
		return nil, err
	}

	// 2. Fork the runtime instance. The runtime creates exactly the branch we
	// reserved (it owns uniqueness otherwise); provisioning output streams
	// straight to the already-open log.
	tlog.Info("forking instance", "br", forkBranch)
	tlog.Debug("checkout", "msg", "calling instance.Fork", "source", sourceInstanceID, "harness", forkOpts.Harness, "tailscale", forkOpts.Tailscale, "usb", forkOpts.USB, "display", forkOpts.Display, "sudo", forkOpts.Sudo, "gitHubToken", fork.GitHubTokenEnabled())
	provisioningLog := &provisioningWriter{ctx: ctx, t: fork, log: log}
	forkOpts.LogWriter = provisioningLog
	forkName, forkConn, forkRepos, err := r.Runtimes.Fork(ctx, sourceInstanceID, forkOpts)
	if flushErr := provisioningLog.Flush(); flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		tlog.Error("checkout", "msg", "instance.Fork failed", "source", sourceInstanceID, "err", err)
		return nil, r.finishStartupFailure(ctx, fork, log, fmt.Errorf("fork instance: %w", err))
	}
	tlog.Debug("checkout", "msg", "instance.Fork succeeded", "source", sourceInstanceID, "fork", forkName)
	fork.SetRuntimeConnectionInfo(forkName, forkConn.AgentTarget, "", "", r.Runtimes.VNCPort(ctx, forkName))
	for i := range fork.ReposSnapshot() {
		if i < len(forkRepos) {
			fork.SetRepoBranch(i, forkRepos[i].Branch)
		}
	}
	tlog.Info("fork instance ready", "instance", forkName)

	// 2. Clean relay state from the source instance's snapshot so the
	// forked task starts with an empty output.jsonl.
	forkSSHHost := forkConn.AgentTarget.SSHHost
	if forkSSHHost == "" {
		forkSSHHost = string(forkName)
	}
	if err := agent.CleanRelayState(ctx, forkSSHHost); err != nil {
		tlog.Warn("clean relay state failed (non-fatal)", "err", err)
	}

	// 3. Start a fresh agent session with the fork prompt.
	// No --resume: the fork gets its own session ID and clean message history.
	fork.SetState(taskslog.StateStarting)
	h, err := r.startSessionWithLog(ctx, fork, fork.InitialPrompt, log)
	if err != nil {
		startupErr := fmt.Errorf("start session on fork: %w", err)
		return nil, r.finishStartupFailure(ctx, fork, log, startupErr)
	}
	tlog.Info("fork session running", "instance", forkName)
	return h, nil
}

func (r *AgentRuntime) openLog(t *Task) (agent.LogSink, error) {
	log, path, err := r.LogStore.Open(t.LogFilename(), t.LogHeader())
	if err != nil {
		return nil, err
	}
	r.LogPath.Set(path)
	return log, nil
}

func (r *AgentRuntime) reopenLog(t *Task) (agent.LogSink, error) {
	// A tracked path preserves the filename actually on disk (e.g. a fork's
	// branch may be reassigned after its log was opened); only recompute it
	// from the task when no log has been opened yet in this process.
	name := t.LogFilename()
	if path := r.LogPath.Get(); path != "" {
		name = filepath.Base(path)
	}
	log, path, err := r.LogStore.Reopen(name, t.LogHeader())
	if err != nil {
		return nil, err
	}
	r.LogPath.Set(path)
	return log, nil
}

func (r *AgentRuntime) compressLog(log agent.LogSink, state taskslog.State) error {
	path, err := r.LogStore.Compress(r.LogPath.Get(), log, state)
	if err != nil {
		return err
	}
	r.LogPath.Set(path)
	return nil
}

func (r *AgentRuntime) branchDiffStat(ctx context.Context, t *Task) (agent.DiffStat, error) {
	if r.Checkout == nil {
		return nil, nil
	}
	id := t.RuntimeInstanceID()
	if id == "" {
		return nil, errors.New("task has no runtime instance")
	}
	repos := t.RuntimeRepos()
	if len(repos) > 0 && repos[0].GitRoot == "" {
		repos[0].GitRoot = r.Checkout.Dir
	}
	r.Log.InfoContext(ctx, "branch diff stat for purge verification", "repo", r.Checkout.RelPath, "repos", len(repos))
	return r.Checkout.DiffStat(ctx, r.Log, r.Runtimes, id, repos)
}

// setup reserves a branch name, starts the instance (Phase A) and creates the
// git branch concurrently, then completes instance startup (Phase B).
// Phase A (runtime launch) and git fetch+branch-create overlap, cutting the
// branch-allocation time off the critical path.
func (r *AgentRuntime) setup(ctx context.Context, t *Task, metadata runtime.Metadata, resolvedGitHubToken string, log agent.LogSink) (setupResult, error) {
	t.SetState(taskslog.StateProvisioning)
	detached := context.WithoutCancel(ctx)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Log.Info("starting instance", "br", primaryBranch, "img", t.BaseImage, "platform", t.ContainerPlatform, "hns", t.Harness, "ts", t.Tailscale, "usb", t.USB, "dpy", t.Display, "sudo", t.Sudo, "gitHubToken", t.GitHubTokenEnabled())
	tContainer := time.Now()
	startCtx, startCancel := context.WithTimeout(detached, r.RuntimeStartTimeout)
	defer startCancel()

	runtimeName := t.RuntimeName
	if runtimeName == "" {
		runtimeName = r.Runtimes.Runtimes[0].Name()
	}
	provisioningLog := &provisioningWriter{ctx: ctx, t: t, log: log}
	opts := &runtime.StartOptions{
		RuntimeName:       runtimeName,
		Metadata:          metadata,
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		Harness:           t.Harness,
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		Caches:            t.CacheMounts,
		Mounts:            t.Mounts,
		MaxCPUs:           t.MaxCPUs,
		GitHubToken:       resolvedGitHubToken,
		LogWriter:         provisioningLog,
	}

	// Phase A: runtime launch + connection config. Branch creation runs concurrently so
	// git fetch overlaps with instance connection startup.
	var repos []runtime.Repo
	if r.Checkout != nil {
		repos = t.RuntimeRepos()
	}
	var instanceID runtime.ID
	r.Log.Debug("checkout", "msg", "provisioning phase A: launching instance and creating branch", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo, "repos_count", len(repos))
	eg, egCtx := errgroup.WithContext(startCtx)
	eg.Go(func() error {
		defer trace.StartRegion(egCtx, "instance-launch").End()
		r.Log.Debug("checkout", "msg", "calling instance.Launch", "branch", primaryBranch)
		id, err := r.Runtimes.Launch(egCtx, repos, opts)
		if err != nil {
			r.Log.Error("checkout", "msg", "instance.Launch failed", "branch", primaryBranch, "err", err)
			return err
		}
		r.Log.Debug("checkout", "msg", "instance.Launch succeeded", "instance", id)
		instanceID = id
		return nil
	})
	if r.Checkout != nil {
		eg.Go(func() error {
			defer trace.StartRegion(egCtx, "branch-create").End()
			r.Log.Debug("checkout", "msg", "fetching and creating branch", "branch", primaryBranch)
			err := r.Checkout.FetchAndCreateBranch(egCtx, r.Log, t, primaryBranch)
			if err != nil {
				r.Log.Error("checkout", "msg", "fetchAndCreateBranch failed", "branch", primaryBranch, "err", err)
			} else {
				r.Log.Debug("checkout", "msg", "fetchAndCreateBranch succeeded", "branch", primaryBranch)
			}
			return err
		})
	}
	r.Log.Debug("checkout", "msg", "waiting for phase A errgroup")
	if err := eg.Wait(); err != nil {
		return setupResult{}, errors.Join(err, provisioningLog.Flush())
	}
	if err := provisioningLog.Flush(); err != nil {
		return setupResult{}, err
	}
	r.Log.Debug("checkout", "msg", "phase A complete", "instance", instanceID)

	// Phase B: wait for runtime connection + push (branch now exists locally).
	r.Log.Debug("checkout", "msg", "provisioning phase B: connecting to instance", "instance", instanceID)
	conn, err := r.Runtimes.Connect(startCtx, instanceID, opts)
	if err != nil {
		r.Log.Error("checkout", "msg", "instance.Connect failed", "instance", instanceID, "err", err)
		return setupResult{}, errors.Join(fmt.Errorf("start instance: %w", err), provisioningLog.Flush())
	}
	if err := provisioningLog.Flush(); err != nil {
		return setupResult{}, err
	}
	r.Log.Info("checkout", "msg", "started", "br", primaryBranch, "dur", time.Since(tContainer), "instance", instanceID, "fqdn", conn.TailscaleFQDN)
	return setupResult{
		InstanceID:       instanceID,
		AgentTarget:      conn.AgentTarget,
		TailscaleFQDN:    conn.TailscaleFQDN,
		TailscaleAuthURL: conn.TailscaleAuthURL,
	}, nil
}

// finishReviveFailure records a failed revive result in the task log. A revive
// may fail before opening a session log, after opening one, or while replacing
// an immediately-exited resumed session; in every case the final trailer is
// appended to a validated log and the non-revivable log is compressed before
// lifecycle cache publication.
func (r *AgentRuntime) finishReviveFailure(ctx context.Context, t *Task, reviveErr error, log agent.LogSink) error {
	t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
	if h := t.CloseAndDetachSession(context.WithoutCancel(ctx)); h != nil {
		h.CloseMsgCh()
		<-h.DispatchDone
		if log == nil {
			log = h.Log
		}
	}
	var reopenErr error
	if log == nil {
		log, reopenErr = r.reopenLog(t)
	}
	if log == nil {
		return errors.Join(reviveErr, reopenErr)
	}
	res := taskslog.Result{State: taskslog.StateFailed, Err: reviveErr}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	if trailerErr != nil {
		return errors.Join(reviveErr, trailerErr, log.Close())
	}
	return errors.Join(reviveErr, r.compressLog(log, taskslog.StateFailed))
}

// finishStartupFailure records a startup error in the task log so the failure
// survives a server restart.
func (r *AgentRuntime) finishStartupFailure(ctx context.Context, t *Task, log agent.LogSink, startupErr error) error {
	failure := &agent.LogMessage{MessageType: "caic_log", Line: "Task startup failed: " + startupErr.Error()}
	writeErr := log.AppendMessage(failure)
	t.SetState(taskslog.StateFailed)
	t.addMessage(ctx, failure, false)

	res := taskslog.Result{State: taskslog.StateFailed, Err: startupErr}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	if writeErr != nil || trailerErr != nil {
		return errors.Join(startupErr, writeErr, trailerErr, log.Close())
	}
	return errors.Join(startupErr, r.compressLog(log, taskslog.StateFailed))
}

// logRelayDiag reads the relay daemon's relay.log from the instance and logs
// its tail. Called when GracefulStop times out to capture relay-side diagnostics.
func (r *AgentRuntime) logRelayDiag(ctx context.Context, tlog *slog.Logger, target runtime.ConnectionTarget) {
	if target.SSHHost == "" {
		tlog.Warn("relay target unavailable")
		return
	}
	tail := agent.ReadRelayLog(ctx, target.SSHHost, 4096)
	if tail == "" {
		tlog.Warn("relay.log empty or unreadable")
		return
	}
	tlog.Warn("relay.log tail on shutdown timeout", "log", tail)
}

func (r *AgentRuntime) startSessionWithLog(ctx context.Context, t *Task, prompt agent.Prompt, log agent.LogSink) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.start-session:"+t.ID.String())
	defer task.End()

	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, errors.New("no instance")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", instanceID)

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)
	tlog.Info("starting session", "hns", t.Harness)
	target := t.RuntimeConnectionTarget()
	session, err := r.Backends[t.Harness].Start(ctx, &agent.Options{
		Logger:        r.Log,
		Target:        target,
		Dir:           r.runtimeDir(t),
		Model:         t.Model,
		Effort:        t.Effort,
		InitialPrompt: prompt,
		MsgCh:         msgCh,
		Log:           log,
	})
	if err != nil {
		close(msgCh)
		<-dispatchDone
		tlog.Error("session start failed", "err", err)
		return nil, err
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)
	if prompt.Text != "" || len(prompt.Images) > 0 {
		t.addMessage(ctx, syntheticUserInput(prompt), false)
		t.SetState(taskslog.StateRunning)
	}
	return h, nil
}

func (r *AgentRuntime) replaceSession(ctx context.Context, t *Task, prompt agent.Prompt, mode replaceSessionMode) (*SessionHandle, error) {
	traceName := "task.clear-context:"
	if mode == replaceSessionRestart {
		traceName = "task.restart:"
	}
	ctx, task := trace.NewTask(ctx, traceName+t.ID.String())
	defer task.End()

	state := t.GetState()
	if state != taskslog.StateWaiting && state != taskslog.StateAsking && state != taskslog.StateHasPlan && state != taskslog.StateStarting {
		return nil, fmt.Errorf("cannot %s in state %s", mode, state)
	}

	// Close current session and persist a context_cleared marker. The marker
	// must be written before closing the old log so RestoreMessages can reset
	// plan state on server restart.
	oldH := t.CloseAndDetachSession(ctx)
	if oldH != nil {
		oldH.CloseMsgCh()
		<-oldH.DispatchDone
		if oldH.Log != nil {
			err := r.LogStore.WriteContextCleared(oldH.Log)
			err = errors.Join(err, oldH.Log.Close())
			if err != nil {
				t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
				return nil, fmt.Errorf("write context cleared: %w", err)
			}
		}
	}

	// Clear in-memory messages.
	t.ClearMessages(ctx)

	// Open new log segment.
	log, err := r.openLog(t)
	if err != nil {
		t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		return nil, fmt.Errorf("open log: %w", err)
	}

	// Start new session.
	t.SetState(taskslog.StateStarting)
	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)

	var branch string
	if p := t.Primary(); p != nil {
		branch = p.Branch
	}
	instanceID := t.RuntimeInstanceID()
	tlog := r.Log.With("br", branch, "instance", instanceID)
	tlog.Info(mode.logMessage(), "hns", t.Harness)
	target := t.RuntimeConnectionTarget()
	opts := &agent.Options{
		Logger: r.Log,
		Target: target,
		Dir:    r.runtimeDir(t),
		Model:  t.Model,
		Effort: t.Effort,
		MsgCh:  msgCh,
		Log:    log,
	}
	if mode == replaceSessionRestart {
		opts.InitialPrompt = prompt
	}
	backend := r.Backends[t.Harness]
	if backend == nil {
		_ = log.Close()
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		return nil, fmt.Errorf("unknown harness %q", t.Harness)
	}
	session, err := backend.Start(ctx, opts)
	if err != nil {
		_ = log.Close()
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		return nil, fmt.Errorf("start session: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)
	if mode == replaceSessionRestart {
		t.addMessage(ctx, syntheticUserInput(prompt), false)
		t.SetState(taskslog.StateRunning)
		tlog.Info("session restarted")
	} else {
		t.SetState(taskslog.StateWaiting)
		tlog.Info("context cleared")
	}
	return h, nil
}

// startMessageDispatch starts a goroutine that reads from msgCh, dispatches to
// t.addMessage, and reports task state transitions.
func (r *AgentRuntime) startMessageDispatch(ctx context.Context, t *Task, skipSideEffects bool) (msgCh chan agent.ParsedMessage, dispatchDone <-chan struct{}) {
	// Capture all repos outside the goroutine to avoid races.
	allRepos := t.RuntimeRepos()
	instanceID := t.RuntimeInstanceID()
	msgCh = make(chan agent.ParsedMessage, 256)
	done := make(chan struct{})
	dispatchDone = done
	go func() {
		defer close(done)
		// Track tool_use IDs from ToolUseMessage that may mutate files.
		pendingMutating := make(map[string]struct{})
		for parsed := range msgCh {
			m := parsed.Message
			emitToolDiff := false
			switch msg := m.(type) {
			case *agent.ToolUseMessage:
				if _, ok := mutatingTools[msg.Name]; ok {
					pendingMutating[msg.ToolUseID] = struct{}{}
				}
			case *agent.ToolResultMessage:
				if _, ok := pendingMutating[msg.ToolUseID]; ok {
					delete(pendingMutating, msg.ToolUseID)
					emitToolDiff = !skipSideEffects && r.Runtimes != nil && r.Checkout != nil
				}
			case *agent.ResultMessage:
				if !skipSideEffects && r.Runtimes != nil && r.Checkout != nil {
					ds, _ := r.Checkout.DiffStat(ctx, r.Log, r.Runtimes, instanceID, allRepos)
					msg.DiffStat = ds
				}
			}
			stateChanged, generateTitle := t.addParsedMessage(parsed, skipSideEffects)
			if stateChanged {
				r.NotifyTaskChange()
			}
			if generateTitle {
				go t.GenerateTitle(ctx, r.Log)
			}
			if emitToolDiff {
				r.emitDiffStatBranch(ctx, t, instanceID, allRepos)
			}
		}
	}()
	return msgCh, dispatchDone
}

// emitDiffStatBranch emits a DiffStatMessage from the current in-container
// diff. This keeps live UI diff stats fresh during a running turn.
func (r *AgentRuntime) emitDiffStatBranch(ctx context.Context, t *Task, id runtime.ID, repos []runtime.Repo) {
	if r.Checkout == nil {
		return
	}
	ds, _ := r.Checkout.DiffStat(ctx, r.Log, r.Runtimes, id, repos)
	if len(ds) == 0 {
		return
	}
	t.addMessage(ctx, &agent.DiffStatMessage{
		MessageType: "caic_diff_stat",
		DiffStat:    ds,
	}, false)
}

// runtimeDir returns the working directory path inside a runtime instance.
// Uses the task's primary repo ContainerPath when available; otherwise falls back
// to computing it from the checkout's Dir basename (legacy). Returns /home/user
// for no-repo checkouts.
//
// TODO(2026-07-01): remove the filepath.Base fallback once all pre-ContainerPath
// runtime instances have cycled out.
func (r *AgentRuntime) runtimeDir(t *Task) string {
	if p := t.Primary(); p != nil && p.ContainerPath != "" {
		return md.ResolveContainerPath(p.ContainerPath)
	}
	if r.Checkout == nil {
		return "/home/user"
	}
	return "/home/user/src/" + filepath.Base(r.Checkout.Dir)
}

type replaceSessionMode int

const (
	replaceSessionRestart replaceSessionMode = iota
	replaceSessionClearContext
)

func (m replaceSessionMode) String() string {
	switch m {
	case replaceSessionRestart:
		return "restart"
	case replaceSessionClearContext:
		return "clear context"
	default:
		return "replace session"
	}
}

func (m replaceSessionMode) logMessage() string {
	if m == replaceSessionRestart {
		return "restarting session"
	}
	return "clearing context"
}

// setupResult holds the outputs of setup: the instance name and optional Tailscale FQDN.
// The primary branch is written into the task repo metadata during setup.
type setupResult struct {
	InstanceID       runtime.ID
	AgentTarget      runtime.ConnectionTarget
	TailscaleFQDN    string
	TailscaleAuthURL string
}

// provisioningWriter is an io.Writer that converts line-by-line output from the
// instance backend into LogMessage events stored on the task for SSE streaming.
type provisioningWriter struct {
	ctx context.Context
	t   *Task
	log agent.LogSink

	mu  sync.Mutex
	buf []byte
}

func (w *provisioningWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			if err := w.emitLineLocked(line); err != nil {
				return len(p), err
			}
		}
	}
	return len(p), nil
}

func (w *provisioningWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := strings.TrimSpace(string(w.buf))
	if line == "" {
		w.buf = nil
		return nil
	}
	w.buf = nil
	return w.emitLineLocked(line)
}

func (w *provisioningWriter) emitLineLocked(line string) error {
	m := &agent.LogMessage{MessageType: "caic_log", Line: line}
	if w.log != nil {
		if err := w.log.AppendMessage(m); err != nil {
			return err
		}
	}
	w.t.addMessage(w.ctx, m, false)
	return nil
}
