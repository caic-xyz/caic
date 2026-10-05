// AgentRuntime owns task sessions, ordered Git summaries, and consolidated turn measurements.

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

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/gomode/mcp"
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
	Backends         agent.Backends
	LogStore         *taskslog.Store
	LogPath          *taskslog.Path
	Runtimes         *runtime.Router
	Log              *slog.Logger
	NotifyTaskChange func()

	Checkout            *repo.Checkout // nil for no-repository tasks
	RuntimeMetadata     runtime.Metadata
	RuntimeStartTimeout time.Duration // Timeout for instance start (image pull). Must be non-zero.
	MCPRegistry         mcp.Registry  // Task-scoped CAIC MCP registry; nil when disabled.
}

// Reconnect attaches to the existing relay using validated persistent history.
// A dead relay returns an error; startup recovery separately revives eligible
// retained instances rather than replacing an unverified live relay.
func (r *AgentRuntime) Reconnect(ctx context.Context, t *Task) (*SessionHandle, error) {
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
	opts := &agent.Options{
		Logger:             r.Log,
		Target:             t.RuntimeConnectionTarget(),
		RelayOffset:        t.RelayOffsetValue(),
		WarmHistory:        true,
		ResumeSessionID:    sessionID,
		Effort:             t.RequestedEffort,
		PendingUserActions: t.PendingUserActions(),
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		return nil, fmt.Errorf("reconnect: %w", err)
	}

	// Reconnect resumes an existing session, so append only after Reopen
	// validates the existing file's authoritative header. A missing or corrupt
	// log must not be replaced because the running relay's format is unknown.
	log, err := r.reopenLog(t)
	if err != nil {
		return nil, err
	}

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false, log)

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
	opts.MsgCh = msgCh
	opts.Log = log
	session, err := r.Backends[t.Harness].AttachRelay(ctx, opts)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		_ = log.Close()
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
// starts a fresh idle relay only when the task has no saved session.
// Existing conversations fail instead of silently losing history.
func (r *AgentRuntime) EnsureSession(ctx context.Context, tlog *slog.Logger, t *Task, h *SessionHandle) (*SessionHandle, error) {
	select {
	case <-h.Done():
		// Session exited immediately (agent was already done).
		t.DetachSession()
		err := h.Drain()
		_ = h.Log.Close()
		if t.GetSessionID() != "" {
			return nil, errors.Join(errors.New("agent session exited before becoming ready"), err)
		}
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
//  1. Adopt the selected unused local branch, or create a new branch from the selected remote/default branch.
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

	// The Manager has already assigned every repo's branch name. The branch is
	// created during setup, so the log can open with the durable, branch-derived
	// filename and persist output from its first line.
	log, err := r.openLog(t)
	if err != nil {
		if errors.Is(err, agent.ErrReadOnlyLog) {
			return nil, err
		}
		startupErr := &StartupError{Harness: t.Harness, Phase: "task log setup", Err: err}
		t.recordStartupFailure(ctx, startupErr)
		return nil, startupErr
	}

	if r.Checkout != nil {
		t.SetState(taskslog.StateBranching)
	}
	tStart := time.Now()
	// 1. Create the branch, then start the instance.
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
		return nil, r.finishStartupFailure(ctx, t, log, &StartupError{Harness: t.Harness, Phase: "runtime setup", Err: err})
	}
	t.SetRuntimeConnectionInfo(sr.InstanceID, sr.AgentTarget, sr.TailscaleFQDN, sr.TailscaleAuthURL, r.Runtimes.VNCPort(ctx, sr.InstanceID))
	r.recordCommitBaseline(ctx, t, log, sr.InstanceID)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Log.Info("checkout", "msg", "ready", "br", primaryBranch, "instance", sr.InstanceID, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(taskslog.StateStarting)
	var msgCh chan agent.TimedMessage
	var dispatchDone <-chan struct{}
	{
		region := trace.StartRegion(ctx, "dispatch-init")
		msgCh, dispatchDone = r.startMessageDispatch(ctx, t, false, log)
		region.End()
	}

	tSession := time.Now()
	tlog := r.Log.With("br", primaryBranch, "instance", sr.InstanceID)
	tlog.Info("starting session", "hns", t.Harness)
	region = trace.StartRegion(ctx, "agent-session")
	target := sr.AgentTarget
	opts := &agent.Options{
		Logger:        r.Log,
		Target:        target,
		Dir:           r.runtimeDir(t),
		Model:         t.RequestedModel,
		Effort:        t.RequestedEffort,
		InitialPrompt: t.InitialPrompt,
		MsgCh:         msgCh,
		Log:           log,
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, r.finishStartupFailure(ctx, t, log, &StartupError{Harness: t.Harness, Phase: "task-scoped MCP setup", Err: err})
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
	region.End()
	if err != nil {
		close(msgCh)
		<-dispatchDone
		tlog.Error("session start failed", "err", err)
		return nil, r.finishStartupFailure(ctx, t, log, &StartupError{Harness: t.Harness, Phase: "agent startup", Err: err})
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
		snapshot, err := r.Checkout.DiffStat(ctx, r.Log, r.Runtimes, t.GitTarget())
		switch {
		case err != nil:
			tlog.WarnContext(ctx, "verify empty task branch failed", "err", err)
		case len(snapshot.DiffStat) > 0:
			t.SetLiveRepositorySummary(&snapshot)
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
		} else if err := r.compressLog(log, t, &res); err != nil {
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
// this preserves git remotes and runtime config. It returns the persisted
// result, or nil when a concurrent teardown supersedes the stop.
func (r *AgentRuntime) StopTask(ctx context.Context, t *Task) *taskslog.Result {
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
		return nil
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
	r.recordStoppedDiskUsage(ctx, t, name, tlog)

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
		return nil
	}

	// Write log trailer so the task reloads as "stopped" (not "failed")
	// after a server restart, preserving live stats for the UI.
	res := taskslog.Result{State: taskslog.StateStopped, AgentResult: t.LastAgentResult()}
	if diskUsed, ok := t.DiskUsage(); ok {
		res.DiskUsedBytes = &diskUsed
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
	var log agent.LogSink
	if h != nil {
		log = h.Log
	} else if r.LogPath.Get() != "" {
		// A failed revive has no attached session. Reopen its preserved log
		// so stopping the instance also persists the retryable task state.
		var err error
		log, err = r.reopenLog(t)
		if err != nil {
			tlog.WarnContext(ctx, "reopen stopped task log failed", "err", err)
		}
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
	res.Err = errors.Join(trailerErr, closeErr)
	return &res
}

// ReviveTask restarts a stopped or crashed instance and resumes the agent session.
// The instance's filesystem is preserved from the previous run.
func (r *AgentRuntime) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.revive:"+t.ID.String())
	defer task.End()

	if (t.InitialPrompt.Text != "" || len(t.InitialPrompt.Images) > 0) && !t.HasAcceptedInputEvidence() {
		return nil, ErrInitialPromptNotAccepted
	}
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, errors.New("no instance to revive")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", instanceID)

	// Reject read-only history before changing task state or the instance.
	intentLog, err := r.reopenLog(t)
	if err != nil {
		return nil, err
	}
	if state, changed := t.SetStateIfAny(taskslog.StateProvisioning, taskslog.StateStopped, taskslog.StateCrashed, taskslog.StateProvisioning); !changed {
		return nil, errors.Join(fmt.Errorf("cannot revive in state %s", state), intentLog.Close())
	}
	// Accepting revival clears a prior stop/purge intent before any runtime
	// side effects. Interrupted revival remains recoverable on restart.
	if err := errors.Join(r.LogStore.WriteResultTrailer(intentLog, t.Title(), &taskslog.Result{State: taskslog.StateProvisioning}), intentLog.Close()); err != nil {
		return nil, err
	}

	// 1. Revive the instance.
	tlog.Info("reviving instance")
	tlog.Debug("checkout", "msg", "calling instance.Revive")
	if err := r.Runtimes.Revive(ctx, instanceID); err != nil {
		tlog.Error("checkout", "msg", "Revive failed", "err", err)
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("revive instance: %w", err), nil)
	}
	tlog.Debug("checkout", "msg", "Revive succeeded", "instance", instanceID)
	t.SetVNCPort(r.Runtimes.VNCPort(ctx, instanceID))

	// 2. Start a new relay with --resume to continue the previous session.
	// Resuming restores harness context inside a fresh relay. Its output is
	// live activity, so keep tool and turn measurements enabled while
	// preserving the existing title in the resumed session.
	t.SetState(taskslog.StateStarting)
	tlog.Info("resuming session after revive", "sess", t.GetSessionID())

	// Restore archived history rather than creating a replacement segment.
	log, err := r.reopenLog(t)
	if err != nil {
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("open log: %w", err), nil)
	}

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, true, log)

	target := t.RuntimeConnectionTarget()
	opts := &agent.Options{
		Logger:          r.Log,
		Target:          target,
		Dir:             r.runtimeDir(t),
		Model:           t.RequestedModel,
		Effort:          t.RequestedEffort,
		ResumeSessionID: t.GetSessionID(),
		MsgCh:           msgCh,
		Log:             log,
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, r.finishReviveFailure(ctx, t, err, log)
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("resume session after revive: %w", err), log)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	// Resume has no prompt. Publish idle before exposing the handle, so
	// input during the readiness wait owns the next Running transition.
	t.SetStateIf(taskslog.StateStarting, taskslog.StateWaiting)
	t.AttachSession(h)

	// 3. An immediately exiting resumed agent fails without discarding context.
	// Tasks without a saved session may start a fresh idle relay.
	h, err = r.EnsureSession(ctx, tlog, t, h)
	if err != nil {
		return nil, r.finishReviveFailure(ctx, t, err, nil)
	}

	// 4. Restore diff stat and per-repo states before returning. Subsequent
	// mutating tool results refresh them through normal message dispatch.
	if r.Checkout != nil {
		snapshot, _ := r.Checkout.DiffStatAndRepoStates(ctx, r.Log, r.Runtimes, t.GitTarget())
		if snapshot.Read.NewerThan(repo.GitRead{}) {
			t.SetLiveRepositorySummary(&snapshot)
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
	forkName, forkConn, err := r.Runtimes.Fork(ctx, sourceInstanceID, forkOpts)
	if flushErr := provisioningLog.Flush(); flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		tlog.Error("checkout", "msg", "instance.Fork failed", "source", sourceInstanceID, "err", err)
		return nil, r.finishStartupFailure(ctx, fork, log, fmt.Errorf("fork instance: %w", err))
	}
	tlog.Debug("checkout", "msg", "instance.Fork succeeded", "source", sourceInstanceID, "fork", forkName)
	fork.SetRuntimeConnectionInfo(forkName, forkConn.AgentTarget, "", "", r.Runtimes.VNCPort(ctx, forkName))
	// Branch names need no sync from the fork result: the runtime is contracted
	// to create DestPrimary verbatim, so the reserved names set before the log
	// opened remain authoritative, matching the launch path.
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

// recordStoppedDiskUsage captures a stopped instance's writable-layer size
// before its result is persisted. The measurement is optional: stop remains
// successful when a runtime cannot inspect the stopped instance.
func (r *AgentRuntime) recordStoppedDiskUsage(ctx context.Context, t *Task, id runtime.ID, log *slog.Logger) {
	if id == "" || r.Runtimes == nil {
		return
	}
	usage, err := r.Runtimes.DiskUsage(ctx, []runtime.ID{id})
	if err != nil {
		log.WarnContext(ctx, "measure stopped disk usage failed", "err", err)
		return
	}
	diskUsed, ok := usage[id]
	if !ok {
		log.WarnContext(ctx, "stopped disk usage unavailable")
		return
	}
	t.UpdateDiskUsage(diskUsed)
}

func (r *AgentRuntime) openLog(t *Task) (agent.LogSink, error) {
	// Retained history owns its filename and format, including archives.
	// Never replace it with a newly created plain segment.
	if r.LogPath.Get() != "" {
		return r.reopenLog(t)
	}
	log, path, err := r.LogStore.Open(t.LogFilename(), t.LogHeader())
	if err != nil {
		return nil, err
	}
	r.LogPath.Set(path)
	return log, nil
}

func (r *AgentRuntime) reopenLog(t *Task) (agent.LogSink, error) {
	// A tracked path preserves the filename actually on disk: compression
	// renames it to .zst, and a path adopted via SetLoadedTask reflects what an
	// earlier process wrote (possibly under an older naming scheme). Recomputing
	// t.LogFilename() is only safe for logs this process opened and has not
	// compressed; branch names cannot be the cause of drift because they are
	// reserved before any task log opens.
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

func (r *AgentRuntime) compressLog(log agent.LogSink, t *Task, res *taskslog.Result) error {
	path, err := r.LogStore.CompressTerminal(r.LogPath.Get(), log, t.terminalLogSummary(log.LogVersion(), res))
	if err != nil {
		return err
	}
	r.LogPath.Set(path)
	return nil
}

// setup creates the reserved task branch before launching the runtime, then
// connects to the instance. The ordering ensures runtimes never receive a
// mapped branch that does not exist yet.
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
	var repos []runtime.Repo
	if r.Checkout != nil {
		repos = t.RuntimeRepos()
		r.Log.Debug("checkout", "msg", "fetching and creating branch", "branch", primaryBranch)
		region := trace.StartRegion(startCtx, "branch-create")
		err := r.Checkout.FetchAndCreateBranch(startCtx, r.Log, t, primaryBranch)
		region.End()
		if err != nil {
			r.Log.Error("checkout", "msg", "fetchAndCreateBranch failed", "branch", primaryBranch, "err", err)
			return setupResult{}, errors.Join(err, provisioningLog.Flush())
		}
		r.Log.Debug("checkout", "msg", "fetchAndCreateBranch succeeded", "branch", primaryBranch)
	}

	r.Log.Debug("checkout", "msg", "calling instance.Launch", "branch", primaryBranch, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo, "repos_count", len(repos))
	region := trace.StartRegion(startCtx, "instance-launch")
	instanceID, err := r.Runtimes.Launch(startCtx, repos, opts)
	region.End()
	if err != nil {
		r.Log.Error("checkout", "msg", "instance.Launch failed", "branch", primaryBranch, "err", err)
		return setupResult{}, errors.Join(err, provisioningLog.Flush())
	}
	r.Log.Debug("checkout", "msg", "instance.Launch succeeded", "instance", instanceID)
	// The task owns the instance as soon as launch succeeds so every later
	// setup failure can purge it, including failures before Connect returns.
	t.SetRuntimeConnectionInfo(instanceID, runtime.ConnectionTarget{}, "", "", 0)
	if err := provisioningLog.Flush(); err != nil {
		return setupResult{}, err
	}

	r.Log.Debug("checkout", "msg", "connecting to instance", "instance", instanceID)
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

func (r *AgentRuntime) configureTaskMCP(t *Task, opts *agent.Options) error {
	if !t.CaicMCP {
		return nil
	}
	if r.MCPRegistry == nil {
		return errors.New("task-scoped MCP is unavailable")
	}
	opts.MCP = r.MCPRegistry
	return nil
}

// finishReviveFailure stops a partially revived instance and records a
// retryable crash. The preserved instance and uncompressed log permit another
// revive after the cause is repaired.
func (r *AgentRuntime) finishReviveFailure(ctx context.Context, t *Task, reviveErr error, log agent.LogSink) error {
	// Keep revive in progress until cleanup and log closure finish, so another
	// revive cannot race with stopping this instance or writing its trailer.
	defer t.SetStateUnless(taskslog.StateCrashed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
	if h := t.CloseAndDetachSession(context.WithoutCancel(ctx)); h != nil {
		h.CloseMsgCh()
		<-h.DispatchDone
		if log == nil {
			log = h.Log
		}
	}
	// Revive can start the instance before returning an error. Stop even when
	// the caller was cancelled so the task does not strand a running instance.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	stopErr := r.Runtimes.Stop(stopCtx, t.RuntimeInstanceID())
	cancel()
	if stopErr != nil {
		reviveErr = errors.Join(reviveErr, fmt.Errorf("stop partially revived instance: %w", stopErr))
	}
	var reopenErr error
	if log == nil {
		log, reopenErr = r.reopenLog(t)
	}
	if log == nil {
		return errors.Join(reviveErr, reopenErr)
	}
	res := taskslog.Result{State: taskslog.StateCrashed, Err: reviveErr}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	return errors.Join(reviveErr, trailerErr, log.Close())
}

// finishStartupFailure removes a started instance and records the startup
// error in the task log so the failure survives a server restart.
func (r *AgentRuntime) finishStartupFailure(ctx context.Context, t *Task, log agent.LogSink, startupErr error) error {
	failure := &agent.LogMessage{MessageType: "caic_log", Line: "Task startup failed: " + startupErr.Error()}
	writeErr := log.AppendMessage(failure)
	t.SetState(taskslog.StateFailed)
	t.addMessage(ctx, failure, false)
	purgeErr := r.purgeFailedStartupInstance(ctx, t)

	res := taskslog.Result{State: taskslog.StateFailed, Err: startupErr}
	if failure, ok := errors.AsType[*StartupError](startupErr); ok {
		details := failure.Details()
		res.StartupFailure = &details
	}
	trailerErr := r.LogStore.WriteResultTrailer(log, t.Title(), &res)
	if writeErr != nil || trailerErr != nil {
		return errors.Join(startupErr, purgeErr, writeErr, trailerErr, log.Close())
	}
	return errors.Join(startupErr, purgeErr, r.compressLog(log, t, &res))
}

// purgeFailedStartupInstance removes an instance created for a task that
// failed before its first agent session could start.
func (r *AgentRuntime) purgeFailedStartupInstance(ctx context.Context, t *Task) error {
	id := t.RuntimeInstanceID()
	if id == "" {
		return nil
	}
	timeout := time.Minute
	if r.Checkout != nil {
		timeout = r.Checkout.GitTimeout
	}
	tlog := r.Log.With("instance", id)
	tlog.InfoContext(ctx, "startup failed; purging instance")
	purgeCtx, purgeCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	err := r.Runtimes.Purge(purgeCtx, id)
	purgeCancel()
	if err != nil {
		tlog.WarnContext(ctx, "purge failed startup instance", "err", err)
	} else {
		t.SetRuntimeConnectionInfo("", runtime.ConnectionTarget{}, "", "", 0)
	}
	return err
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

	r.recordCommitBaseline(ctx, t, log, instanceID)
	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false, log)
	tlog.Info("starting session", "hns", t.Harness)
	target := t.RuntimeConnectionTarget()
	opts := &agent.Options{
		Logger:        r.Log,
		Target:        target,
		Dir:           r.runtimeDir(t),
		Model:         t.RequestedModel,
		Effort:        t.RequestedEffort,
		InitialPrompt: prompt,
		MsgCh:         msgCh,
		Log:           log,
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, err
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
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

	// Validate the retained log before closing the session or clearing history.
	log, err := r.openLog(t)
	if err != nil {
		if !errors.Is(err, agent.ErrReadOnlyLog) {
			t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		}
		return nil, fmt.Errorf("open log: %w", err)
	}

	// Close current session and persist a context_cleared marker. The marker
	// must be written before closing the old log so SeedTimeline can reset
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
				return nil, errors.Join(fmt.Errorf("write context cleared: %w", err), log.Close())
			}
		}
	}

	// Clear in-memory messages.
	t.ClearMessages(ctx)

	// Start new session.
	t.SetState(taskslog.StateStarting)
	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false, log)

	var branch string
	if p := t.Primary(); p != nil {
		branch = p.Branch
	}
	instanceID := t.RuntimeInstanceID()
	tlog := r.Log.With("br", branch, "instance", instanceID)
	tlog.Info(mode.logMessage(), "hns", t.Harness)
	r.recordCommitBaseline(ctx, t, log, instanceID)
	target := t.RuntimeConnectionTarget()
	opts := &agent.Options{
		Logger: r.Log,
		Target: target,
		Dir:    r.runtimeDir(t),
		Model:  t.RequestedModel,
		Effort: t.RequestedEffort,
		MsgCh:  msgCh,
		Log:    log,
	}
	if mode == replaceSessionRestart {
		opts.InitialPrompt = prompt
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		close(msgCh)
		<-dispatchDone
		_ = log.Close()
		t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		return nil, err
	}
	backend := r.Backends[t.Harness]
	if backend == nil {
		close(msgCh)
		<-dispatchDone
		_ = log.Close()
		t.SetStateUnless(taskslog.StateFailed, taskslog.StatePurging, taskslog.StatePurged, taskslog.StateStopping, taskslog.StateStopped)
		return nil, fmt.Errorf("unknown harness %q", t.Harness)
	}
	session, err := backend.Start(ctx, opts)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		_ = log.Close()
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
func (r *AgentRuntime) startMessageDispatch(ctx context.Context, t *Task, skipTitleGen bool, log agent.LogSink) (msgCh chan agent.TimedMessage, dispatchDone <-chan struct{}) {
	// Capture all repos outside the goroutine to avoid races.
	target := t.GitTarget()
	allRepos := target.Repos
	instanceID := target.InstanceID
	msgCh = make(chan agent.TimedMessage, 256)
	done := make(chan struct{})
	dispatchDone = done
	go func() {
		defer close(done)
		// Track tool_use IDs from ToolUseMessage that may mutate files.
		pendingMutating := make(map[string]struct{})
		for parsed := range msgCh {
			m := parsed.Message
			emitToolDiff := false
			var commitSnapshot *agent.TurnCommitSnapshotMessage
			var resultRead *repo.GitSnapshot
			switch msg := m.(type) {
			case *agent.ToolUseMessage:
				if _, ok := mutatingTools[msg.Name]; ok {
					pendingMutating[msg.ToolUseID] = struct{}{}
				}
			case *agent.ToolResultMessage:
				if _, ok := pendingMutating[msg.ToolUseID]; ok {
					delete(pendingMutating, msg.ToolUseID)
					emitToolDiff = r.Runtimes != nil && r.Checkout != nil
				}
			case *agent.ResultMessage:
				if r.Runtimes != nil && r.Checkout != nil {
					previous := t.latestCommitSnapshot()
					var baseline []v3.RepositoryCommit
					if previous != nil {
						baseline = previous.RepositoryCommits
					}
					snapshot, commits, change, _ := r.Checkout.TurnSnapshot(ctx, r.Log, r.Runtimes, target, baseline)
					resultRead = &snapshot
					msg.DiffStat = snapshot.DiffStat
					if len(commits) > 0 {
						commitSnapshot = agent.NewTurnCommitSnapshotMessage(commits, false, change)
					}
				}
			}
			stateChanged, generateTitle, persistErr := t.addParsedMessageWithGitSnapshot(parsed, skipTitleGen, resultRead, log)
			if persistErr != nil {
				r.Log.WarnContext(ctx, "persisting Git summary failed", "err", persistErr)
			}
			if commitSnapshot != nil {
				r.persistCommitSnapshot(ctx, log, commitSnapshot, instanceID)
				t.addMessage(ctx, commitSnapshot, false)
			}
			if stateChanged {
				r.NotifyTaskChange()
			}
			if generateTitle {
				go t.GenerateTitle(ctx, r.Log)
			}
			if emitToolDiff {
				r.emitDiffStatBranch(ctx, t, instanceID, allRepos, log)
			}
		}
	}()
	return msgCh, dispatchDone
}

// fetchTurnCommits makes the finished turn's committed work durable on the
// host without committing pending work or marking it integrated into the host
// branch. The returned immutable tips are persisted as a standalone caic-owned
// turn-boundary log control.
//
// A failure leaves the turn without a commit snapshot, so it is logged and
// the turn still completes.
func (r *AgentRuntime) fetchTurnCommits(ctx context.Context, id runtime.ID) []v3.RepositoryCommit {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Checkout.GitTimeout)
	defer cancel()
	fetched, err := r.Runtimes.Fetch(fetchCtx, id, runtime.FetchOpts{})
	if err != nil {
		r.Log.WarnContext(ctx, "fetching turn commits failed", "id", id, "err", err)
		return nil
	}
	commits := make([]v3.RepositoryCommit, len(fetched))
	for i, f := range fetched {
		commits[i] = v3.RepositoryCommit{
			RepositoryPath: f.RepositoryPath,
			BranchName:     f.BranchName,
			CommitHash:     f.CommitHash,
		}
	}
	return commits
}

// recordCommitBaseline records the branch tips from immediately before a new
// agent session, making the first completed turn comparable to the task state
// it inherited.
func (r *AgentRuntime) recordCommitBaseline(ctx context.Context, t *Task, log agent.LogSink, id runtime.ID) {
	if r.Checkout == nil || r.Runtimes == nil || log == nil {
		return
	}
	commits := r.fetchTurnCommits(ctx, id)
	if len(commits) == 0 {
		return
	}
	snapshot := agent.NewTurnCommitSnapshotMessage(commits, true, nil)
	if err := log.AppendMessage(snapshot); err != nil {
		r.Log.WarnContext(ctx, "persisting commit baseline failed", "id", id, "err", err)
		return
	}
	t.addMessage(ctx, snapshot, false)
}

// persistCommitSnapshot writes a completed-turn commit snapshot without
// preventing the turn from completing when persistence is unavailable.
func (r *AgentRuntime) persistCommitSnapshot(ctx context.Context, log agent.LogSink, snapshot *agent.TurnCommitSnapshotMessage, id runtime.ID) {
	if log == nil {
		r.Log.WarnContext(ctx, "persisting turn commit snapshot failed", "id", id, "err", taskslog.ErrNoLog)
		return
	}
	if err := log.AppendMessage(snapshot); err != nil {
		r.Log.WarnContext(ctx, "persisting turn commit snapshot failed", "id", id, "err", err)
	}
}

// emitDiffStatBranch emits a DiffStatMessage from the current in-container
// diff. This keeps live UI diff stats and repository states fresh during a
// running turn.
func (r *AgentRuntime) emitDiffStatBranch(ctx context.Context, t *Task, id runtime.ID, repos []runtime.Repo, log agent.LogSink) {
	if r.Checkout == nil {
		return
	}
	snapshot, _ := r.Checkout.DiffStatAndRepoStates(ctx, r.Log, r.Runtimes, repo.GitTarget{InstanceID: id, Repos: repos})
	if len(snapshot.DiffStat) == 0 && len(snapshot.RepoStates) == 0 && len(snapshot.FailedRepos) == 0 {
		return
	}
	changed, _, persistErr := t.addParsedMessageWithGitSnapshot(agent.TimedMessage{Message: &agent.DiffStatMessage{
		MessageType: "caic_diff_stat", DiffStat: snapshot.DiffStat, Repos: snapshot.RepoStates,
	}}, false, &snapshot, log)
	if persistErr != nil {
		r.Log.WarnContext(ctx, "persisting Git summary failed", "err", persistErr)
	}
	if changed {
		r.NotifyTaskChange()
	}
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

// StartupError identifies a task startup failure without discarding its cause.
// It is persisted in the task result and exposed to clients as structured data.
type StartupError struct {
	Harness harness.Name
	Phase   string
	Err     error
}

// Error implements error.
func (e *StartupError) Error() string {
	return fmt.Sprintf("%s startup failed during %s: %v", e.Harness, e.Phase, e.Err)
}

// Unwrap returns the underlying startup diagnostic.
func (e *StartupError) Unwrap() error { return e.Err }

// Details returns the durable client-facing startup diagnostic.
func (e *StartupError) Details() v3.StartupFailure {
	return v3.StartupFailure{Harness: string(e.Harness), Phase: e.Phase, Cause: e.Err.Error()}
}

type replaceSessionMode int

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

const (
	replaceSessionRestart replaceSessionMode = iota
	replaceSessionClearContext
)

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
