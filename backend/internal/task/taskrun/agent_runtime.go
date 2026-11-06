// AgentRuntime owns task sessions, recorded reconnect history and cursors, Git summaries, and turn measurements.

package taskrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
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
	LogStore         *taskhistory.Store
	LogPath          *taskslog.Path
	Runtimes         *runtime.Router
	Log              *slog.Logger
	NotifyTaskChange func()

	Checkout             *repo.Checkout // nil for no-repository tasks
	RuntimeMetadata      runtime.Metadata
	RuntimeStartTimeout  time.Duration // Timeout for instance start (image pull). Must be non-zero.
	EnsureSessionTimeout time.Duration // Timeout for session verification in EnsureSession. Defaults to 10s if <= 0.
	MCPRegistry          mcp.Registry  // Task-scoped CAIC MCP registry; nil when disabled.
}

// Reconnect attaches to the existing relay using validated persistent history.
// A dead relay returns an error; startup recovery separately revives eligible
// retained instances rather than replacing an unverified live relay.
func (r *AgentRuntime) Reconnect(ctx context.Context, t *Task) (*SessionHandle, error) {
	if t.HasSession() {
		return nil, errors.New("session already active")
	}
	if t.RuntimeInstanceID() == "" {
		return nil, errors.New("no instance to reconnect to")
	}
	sessionID := t.GetSessionID()
	if harness.RequiresResumeSessionID(t.Harness) && sessionID == "" {
		return nil, fmt.Errorf("%s session ID missing; cannot reconnect", t.Harness)
	}
	run, err := r.beginRun(ctx, t, true, true)
	if err != nil {
		return nil, err
	}
	defer close(run.startupDone)
	opts := &agent.Options{Logger: r.Log, Target: t.RuntimeConnectionTarget(), RelayOffset: t.RelayOffsetValue(), ResumeSessionID: sessionID, Effort: t.RequestedEffort, PendingUserActions: t.PendingUserActions(), Records: run.records}
	if err = r.configureTaskMCP(t, opts); err != nil {
		return nil, errors.Join(err, r.closeRun(t, run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	}
	session, err := r.Backends[t.Harness].AttachRelay(run.ctx, opts) //nolint:contextcheck // The execution owns this transport lifetime.
	if err != nil {
		return nil, errors.Join(fmt.Errorf("reconnect: %w", err), r.closeRun(t, run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	}
	h := &SessionHandle{Session: session, Run: run}
	t.AttachSession(h)
	return h, nil
}

// EnsureSession waits briefly for h to confirm it's alive. If the session
// exits within 10 seconds (agent had already finished), it detaches and
// starts a fresh idle relay only when the task has no saved session.
// Existing conversations fail instead of silently losing history. The supplied
// context must also own the session process; cancellation drains its local
// transport without sending a stop sentinel to the retained remote agent.
func (r *AgentRuntime) EnsureSession(ctx context.Context, tlog *slog.Logger, t *Task, h *SessionHandle) (*SessionHandle, error) {
	timeout := r.EnsureSessionTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-h.Done():
		// Session exited immediately (agent was already done).
		t.DetachSession()
		err := h.Drain()
		closeErr := r.closeRun(t, h.Run) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
		if closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		if t.GetSessionID() != "" {
			return nil, errors.Join(errors.New("agent session exited before becoming ready"), err)
		}
		tlog.Info("attached session exited, starting idle relay", "err", err)
		if s := t.GetState(); s == task.StateStopping || s == task.StateStopped || s == task.StatePurged {
			return nil, fmt.Errorf("task is %s", s)
		}
		t.SetState(task.StateWaiting)
		return r.StartSession(ctx, t, agent.Prompt{})
	case <-time.After(timeout):
		// Session is alive — all good.
		return h, nil
	case <-ctx.Done():
		t.DetachSession()
		return nil, errors.Join(ctx.Err(), h.Drain(), r.closeRun(t, h.Run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
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
	run, err := r.beginRun(ctx, t, false, false)
	if err != nil {
		return nil, err
	}
	defer close(run.startupDone)
	h, err := r.startSessionWithRun(t, prompt, run)
	if err != nil {
		return nil, errors.Join(err, r.closeRun(t, run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
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
//
//nolint:contextcheck // Run owns setup and transport cancellation independently of its caller.
func (r *AgentRuntime) Start(ctx context.Context, t *Task, resolvedGitHubToken string) (*SessionHandle, error) {
	ctx, span := trace.NewTask(ctx, "task.start:"+t.ID.String())
	defer span.End()

	// The Manager has already assigned every repo's branch name. The branch is
	// created during setup, so the log can open with the durable, branch-derived
	// filename and persist output from its first line.
	run, err := r.beginRun(ctx, t, false, false)
	if err != nil {
		startupErr := &StartupError{Harness: t.Harness, Phase: "task log setup", Err: err}
		t.SetState(task.StateFailed)
		return nil, startupErr
	}

	defer close(run.startupDone)
	ctx = run.ctx
	records := run.records

	if r.Checkout != nil {
		t.SetState(task.StateBranching)
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
	sr, err := r.setup(ctx, t, metadata, resolvedGitHubToken, records)
	region.End()
	if err != nil {
		return nil, r.finishStartupFailure(ctx, t, run, &StartupError{Harness: t.Harness, Phase: "runtime setup", Err: err})
	}
	t.SetRuntimeConnectionInfo(sr.InstanceID, sr.AgentTarget, sr.TailscaleFQDN, sr.TailscaleAuthURL, r.Runtimes.VNCPort(ctx, sr.InstanceID))
	if err := r.recordCommitBaseline(ctx, records, sr.InstanceID); err != nil {
		return nil, r.finishStartupFailure(ctx, t, run, err)
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Log.Info("checkout", "msg", "ready", "br", primaryBranch, "instance", sr.InstanceID, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(task.StateStarting)

	tSession := time.Now()
	tlog := r.Log.With("br", primaryBranch, "instance", sr.InstanceID)
	tlog.Info("starting session", "hns", t.Harness)
	region = trace.StartRegion(ctx, "agent-session")
	target := sr.AgentTarget
	opts := &agent.Options{
		Logger:          r.Log,
		Target:          target,
		Dir:             r.runtimeDir(t),
		Model:           t.RequestedModel,
		Effort:          t.RequestedEffort,
		InitialPrompt:   t.InitialPrompt,
		ResumeSessionID: t.GetSessionID(),
		Records:         records,
	}
	if err := r.configureTaskMCP(t, opts); err != nil {
		return nil, r.finishStartupFailure(ctx, t, run, &StartupError{Harness: t.Harness, Phase: "task-scoped MCP setup", Err: err})
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
	region.End()
	if err != nil {
		tlog.Error("session start failed", "err", err)
		return nil, r.finishStartupFailure(ctx, t, run, &StartupError{Harness: t.Harness, Phase: "agent startup", Err: err})
	}

	// Store handle so SendInput can reach it.
	h := &SessionHandle{Session: session, Run: run}
	t.AttachSession(h)

	// Keep a result already applied by the recording owner during startup
	// from being overwritten back to Running.
	t.SetStateIf(task.StateStarting, task.StateRunning)
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
func (r *AgentRuntime) Cleanup(ctx context.Context, t *Task, reason task.State) task.Result {
	ctx, span := trace.NewTask(ctx, "task.cleanup:"+t.ID.String())
	defer span.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", name)
	tlog.InfoContext(ctx, "cleanup starting", "reason", reason, "state", t.GetState(), "has_session", t.HasSession())

	// Graceful shutdown: send stop sentinel so the relay sends SIGINT.
	// Stats come from the committed-message fold.
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
	var summaryErr error
	if reason == task.StatePurged && !t.DiffCreated() && name != "" && r.Checkout != nil {
		snapshot, err := r.Checkout.DiffStat(ctx, r.Log, r.Runtimes, t.GitTarget())
		switch {
		case err != nil:
			tlog.WarnContext(ctx, "verify empty task branch failed", "err", err)
		case len(snapshot.DiffStat) > 0:
			_, summaryErr = r.RecordRepositorySummary(ctx, t, &snapshot)
		default:
			branchConfirmedEmpty = true
		}
	}

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

	res := task.Result{
		Err:         summaryErr,
		State:       reason,
		AgentResult: t.LastAgentResult(),
	}
	liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats()
	res.CostUSD = liveCost
	res.CostKnown = t.LiveCostKnown()
	res.NumTurns = liveTurns
	res.Duration = liveDur
	res.Usage = liveUsage
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	if reason == task.StatePurged && runtimeRemovedOrAbsent && branchConfirmedEmpty {
		if r.Checkout != nil {
			r.Checkout.DeleteUnmodifiedTaskBranches(ctx, r.Log, t)
		}
	}
	var recordErr error
	if h != nil {
		recordErr = r.finalizeRun(ctx, t, h.Run, &res)
	} else {
		recordErr = r.CommitSettledResult(ctx, t, &res)
	}
	if recordErr != nil {
		res.Err = errors.Join(res.Err, recordErr)
	} else {
		t.SetState(reason)
		res.Err = errors.Join(res.Err, r.compressLog(nil, t, &res))
	}
	tlog.InfoContext(ctx, "cleanup done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns, "reason", reason)
	return res
}

// StopTask gracefully shuts down the agent session and stops the instance
// without removing it. The instance can be revived later. Unlike Cleanup,
// this preserves git remotes and runtime config. It returns the persisted
// result, or nil when a concurrent teardown supersedes the stop.
func (r *AgentRuntime) StopTask(ctx context.Context, t *Task) *task.Result {
	ctx, span := trace.NewTask(ctx, "task.stop:"+t.ID.String())
	defer span.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Log.With("br", primaryBranch, "instance", name)
	tlog.InfoContext(ctx, "stop starting", "state", t.GetState())
	if _, changed := t.SetStateUnless(task.StateStopping, task.StatePurging, task.StatePurged, task.StateCrashed, task.StateFailed, task.StateStopped); !changed {
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

	if _, changed := t.SetStateUnless(task.StateStopped, task.StatePurging, task.StatePurged, task.StateCrashed, task.StateFailed); !changed {
		// A purge or a crash already settled this incarnation and owns its record
		// owner, so a stop must not record a result over it.
		tlog.InfoContext(ctx, "stop abandoned", "state", t.GetState())
		return nil
	}
	// Announce the settled state now: a scheduled purge waits for the change
	// signal to observe it, and unrelated activity must not be what wakes it.
	r.NotifyTaskChange()

	// Write log trailer so the task reloads as "stopped" (not "failed")
	// after a server restart, preserving live stats for the UI.
	res := task.Result{State: task.StateStopped, AgentResult: t.LastAgentResult()}
	if diskUsed, ok := t.DiskUsage(); ok {
		res.DiskUsedBytes = &diskUsed
	}
	liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats()
	res.CostUSD = liveCost
	res.CostKnown = t.LiveCostKnown()
	res.NumTurns = liveTurns
	res.Duration = liveDur
	res.Usage = liveUsage
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	var recordErr error
	if h != nil {
		recordErr = r.finalizeRun(ctx, t, h.Run, &res)
	} else {
		recordErr = r.CommitSettledResult(ctx, t, &res)
	}

	tlog.InfoContext(ctx, "stop done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns)
	res.Err = recordErr
	return &res
}

// ReviveTask restarts a stopped or crashed instance and resumes the agent session.
// The instance's filesystem is preserved from the previous run.
func (r *AgentRuntime) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	if (t.InitialPrompt.Text != "" || len(t.InitialPrompt.Images) > 0) && !t.HasAcceptedInputEvidence() {
		return nil, ErrInitialPromptNotAccepted
	}
	id := t.RuntimeInstanceID()
	if id == "" {
		return nil, errors.New("no instance to revive")
	}
	_, accepted, err := r.RecordLifecycleIntent(ctx, t, task.StateProvisioning, task.StateProvisioning, task.StateStopped, task.StateCrashed)
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, fmt.Errorf("cannot revive in state %s", t.GetState())
	}
	run, err := r.beginRun(ctx, t, true, false)
	if err != nil {
		t.SetState(task.StateCrashed)
		r.NotifyTaskChange()
		return nil, err
	}
	defer close(run.startupDone)
	return r.reviveRun(t, run)
}

// RecoverRetainedSession records recovery intent before stopping an unverified
// retained runtime, then resumes it through the same newly owned execution.
func (r *AgentRuntime) RecoverRetainedSession(ctx context.Context, t *Task, expectedState task.State) (*SessionHandle, error) {
	_, accepted, err := r.RecordLifecycleIntent(ctx, t, task.StateProvisioning, task.StateProvisioning, expectedState)
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, fmt.Errorf("cannot recover in state %s", t.GetState())
	}
	run, err := r.beginRun(ctx, t, true, false)
	if err != nil {
		t.SetState(task.StateCrashed)
		r.NotifyTaskChange()
		return nil, err
	}
	defer close(run.startupDone)
	if err := r.Runtimes.Stop(run.ctx, t.RuntimeInstanceID()); err != nil { //nolint:contextcheck // Retained recovery belongs to the independently cancelable execution.
		failure := fmt.Errorf("stop retained instance before recovery: %w", err)
		t.SetState(task.StateCrashed)
		return nil, errors.Join(failure, r.finalizeRun(context.WithoutCancel(ctx), t, run, &task.Result{State: task.StateCrashed, Err: failure}))
	}
	return r.reviveRun(t, run)
}

// ForkTask snapshots the source task's instance and starts an idle agent
// session in the forked instance. The new task must already have its ID,
// Harness, Model, and other immutable fields set. The method fills in
// Runtime, Repos[*].Branch, and starts the session.
//
//nolint:contextcheck // The new execution owns fork setup and transport cancellation independently of its caller.
func (r *AgentRuntime) ForkTask(ctx context.Context, source, fork *Task, forkOpts *runtime.ForkOptions, resolvedGitHubToken string) (*SessionHandle, error) {
	ctx, span := trace.NewTask(ctx, "task.fork:"+source.ID.String()+"->"+fork.ID.String())
	defer span.End()

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
	fork.SetState(task.StateProvisioning)
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

	run, err := r.beginRun(ctx, fork, false, false)
	if err != nil {
		fork.SetState(task.StateFailed)
		return nil, err
	}

	defer close(run.startupDone)
	ctx = run.ctx

	// 2. Fork the runtime instance. The runtime creates exactly the branch we
	// reserved (it owns uniqueness otherwise); provisioning output streams
	// straight to the already-open log.
	tlog.Info("forking instance", "br", forkBranch)
	tlog.Debug("checkout", "msg", "calling instance.Fork", "source", sourceInstanceID, "harness", forkOpts.Harness, "tailscale", forkOpts.Tailscale, "usb", forkOpts.USB, "display", forkOpts.Display, "sudo", forkOpts.Sudo, "gitHubToken", fork.GitHubTokenEnabled())
	provisioningLog := &provisioningWriter{ctx: ctx, records: run.records}
	forkOpts.LogWriter = provisioningLog
	forkName, forkConn, err := r.Runtimes.Fork(ctx, sourceInstanceID, forkOpts)
	if flushErr := provisioningLog.Flush(); flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		tlog.Error("checkout", "msg", "instance.Fork failed", "source", sourceInstanceID, "err", err)
		return nil, r.finishStartupFailure(ctx, fork, run, fmt.Errorf("fork instance: %w", err))
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
	fork.SetState(task.StateStarting)
	h, err := r.startSessionWithRun(fork, fork.InitialPrompt, run)
	if err != nil {
		startupErr := fmt.Errorf("start session on fork: %w", err)
		return nil, r.finishStartupFailure(ctx, fork, run, startupErr)
	}
	tlog.Info("fork session running", "instance", forkName)
	return h, nil
}

// RecordHost records an intentional mutation through the active owner, or an
// exclusive short-lived owner when the execution has settled.
func (r *AgentRuntime) RecordHost(ctx context.Context, t *Task, m agent.Message) error {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if err := t.recordingAdmissionErrorLocked(); err != nil {
		return err
	}
	if t.run.Load() != nil {
		return t.run.Load().records.AppendMessage(ctx, m)
	}
	return r.recordSettled(ctx, t, []agent.Message{m}, nil)
}

// RecordRepositorySummary admits a completed Git read in physical stream order.
func (r *AgentRuntime) RecordRepositorySummary(ctx context.Context, t *Task, read *repo.GitSnapshot) (bool, error) {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if err := t.recordingAdmissionErrorLocked(); err != nil {
		return false, err
	}
	intent := &repositoryIntent{task: t, read: read}
	var err error
	if t.run.Load() != nil {
		err = t.run.Load().records.CommitIntent(ctx, intent)
	} else {
		err = r.recordSettled(ctx, t, nil, intent)
	}
	return intent.changed, err
}

// RecordLifecycleIntent persists an accepted state intent before publishing its
// runtime transition. Rejected intents publish neither records nor transitions.
func (r *AgentRuntime) RecordLifecycleIntent(ctx context.Context, t *Task, persistedState, runtimeState task.State, allowedStates ...task.State) (task.State, bool, error) {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if err := t.recordingAdmissionErrorLocked(); err != nil {
		return t.GetState(), false, err
	}
	intent := &lifecycleIntent{runtime: r, task: t, persisted: persistedState, state: runtimeState, allowed: allowedStates}
	if t.run.Load() == nil {
		intent.previous = t.GetState()
		if !slices.Contains(allowedStates, intent.previous) {
			return intent.previous, false, nil
		}
	}
	var err error
	if t.run.Load() != nil {
		err = t.run.Load().records.CommitIntent(ctx, intent)
	} else {
		err = r.recordSettled(ctx, t, nil, intent)
	}
	return intent.previous, intent.accepted && err == nil, err
}

// CommitSettledResult records a result only when no execution owns the journal.
func (r *AgentRuntime) CommitSettledResult(ctx context.Context, t *Task, res *task.Result) error {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.run.Load() != nil {
		return errors.New("cannot finalize a settled task with an active execution")
	}
	return r.recordSettled(ctx, t, []agent.Message{taskhistory.ResultMessage(t.Title(), res)}, nil)
}

// Shutdown cancels and joins local producers without stopping a retained remote
// relay. It also joins startup before claiming and closing the record owner.
func (r *AgentRuntime) Shutdown(ctx context.Context, t *Task) error {
	r.Cancel(t)
	t.recordingMu.Lock()
	run := t.run.Load()
	t.recordingMu.Unlock()
	if run == nil {
		return nil
	}
	<-run.startupDone
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.run.Load() != run {
		return nil
	}
	var transportErr error
	if h := t.DetachSession(); h != nil {
		transportErr = h.Drain()
	}
	t.run.Store(nil)
	_ = ctx
	return localShutdownError(errors.Join(transportErr, run.close())) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
}

// Cancel stops future execution admission and interrupts existing local
// producers without waiting or closing recording. Cancellation is repeatable;
// Shutdown is the single joining and physical-close operation.
func (r *AgentRuntime) Cancel(t *Task) {
	t.shuttingDown.Store(true)
	if run := t.run.Load(); run != nil {
		run.cancel(context.Canceled)
	}
}

// FinishSession claims the expected execution after its transport has joined.
// Operational failures affect runtime health without inventing agent messages.
func (r *AgentRuntime) FinishSession(ctx context.Context, t *Task, h *SessionHandle, sessionErr error) (bool, error) {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.run.Load() != h.Run {
		return false, nil
	}
	t.DetachSession()
	joinErr := h.Run.joinTitles() //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	if sessionErr != nil {
		if !t.MarkSessionFailed() {
			t.MarkSessionCrashed()
		}
	}
	var appendErr error
	state := t.GetState()
	if state == task.StateFailed || state == task.StateCrashed {
		appendErr = h.Run.records.AppendMessage(ctx, taskhistory.ResultMessage(t.Title(), &task.Result{State: state, Err: sessionErr, AgentResult: t.LastAgentResult()}))
	}
	t.run.Store(nil)
	closeErr := h.Run.close() //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	return true, errors.Join(joinErr, appendErr, closeErr)
}

func (r *AgentRuntime) reviveRun(t *Task, run *Run) (*SessionHandle, error) {
	ctx := run.ctx
	id := t.RuntimeInstanceID()
	if err := r.Runtimes.Revive(ctx, id); err != nil {
		return nil, r.finishReviveFailure(ctx, t, fmt.Errorf("revive instance: %w", err), run)
	}
	t.SetVNCPort(r.Runtimes.VNCPort(ctx, id))
	t.SetState(task.StateStarting)
	opts := &agent.Options{Logger: r.Log, Target: t.RuntimeConnectionTarget(), Dir: r.runtimeDir(t), Model: t.RequestedModel, Effort: t.RequestedEffort, ResumeSessionID: t.GetSessionID(), Records: run.records}
	if err := r.configureTaskMCP(t, opts); err != nil {
		return nil, r.finishReviveFailure(ctx, t, err, run)
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
	if err != nil {
		return nil, r.finishReviveFailure(ctx, t, err, run)
	}
	h := &SessionHandle{Session: session, Run: run}
	t.SetStateIf(task.StateStarting, task.StateWaiting)
	t.AttachSession(h)
	h, err = r.EnsureSession(run.parent, r.Log, t, h)
	if err != nil {
		return nil, r.finishReviveFailure(ctx, t, err, nil)
	}
	if r.Checkout != nil {
		snapshot, _ := r.Checkout.DiffStatAndRepoStates(ctx, r.Log, r.Runtimes, t.GitTarget())
		if _, err := r.RecordRepositorySummary(ctx, t, &snapshot); err != nil {
			return nil, err
		}
	}
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

func (r *AgentRuntime) compressLog(log agent.LogSink, t *Task, res *task.Result) error {
	path, err := r.LogStore.CompressTerminal(r.LogPath.Get(), log, t.terminalLogSummary(res))
	if err != nil {
		return err
	}
	r.LogPath.Set(path)
	return nil
}

// setup creates the reserved task branch before launching the runtime, then
// connects to the instance. The ordering ensures runtimes never receive a
// mapped branch that does not exist yet.
func (r *AgentRuntime) setup(ctx context.Context, t *Task, metadata runtime.Metadata, resolvedGitHubToken string, records *agent.Records) (setupResult, error) {
	t.SetState(task.StateProvisioning)
	detached := ctx
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
	provisioningLog := &provisioningWriter{ctx: ctx, records: records}
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
func (r *AgentRuntime) finishReviveFailure(ctx context.Context, t *Task, reviveErr error, run *Run) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	stopErr := r.Runtimes.Stop(stopCtx, t.RuntimeInstanceID())
	cancel()
	t.SetStateUnless(task.StateCrashed, task.StatePurging, task.StatePurged, task.StateStopping, task.StateStopped)
	res := &task.Result{State: task.StateCrashed, Err: reviveErr}
	if run == nil {
		return errors.Join(reviveErr, stopErr, r.CommitSettledResult(context.WithoutCancel(ctx), t, res))
	}
	_, drainErr := t.CloseAndDetachSession(context.WithoutCancel(ctx))
	err := run.records.AppendMessage(context.WithoutCancel(ctx), taskhistory.ResultMessage(t.Title(), res))
	return errors.Join(reviveErr, stopErr, drainErr, err, r.closeRun(t, run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
}

// finishStartupFailure publishes operational health even when recording fails,
// but only accepted controls enter the task timeline.
func (r *AgentRuntime) finishStartupFailure(ctx context.Context, t *Task, run *Run, startupErr error) error {
	res := &task.Result{State: task.StateFailed, Err: startupErr}
	if failure, ok := errors.AsType[*StartupError](startupErr); ok {
		details := failure.Details()
		res.StartupFailure = &details
	}
	writeErr := run.records.AppendMessages(context.WithoutCancel(ctx), []agent.Message{&agent.LogMessage{MessageType: "caic_log", Line: "Task startup failed: " + startupErr.Error()}, taskhistory.ResultMessage(t.Title(), res)})
	t.SetState(task.StateFailed)
	purgeErr := r.purgeFailedStartupInstance(ctx, t)
	closeErr := r.closeRun(t, run) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	var compressErr error
	if writeErr == nil && closeErr == nil {
		compressErr = r.compressLog(nil, t, res)
	}
	return errors.Join(startupErr, writeErr, purgeErr, closeErr, compressErr)
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

func (r *AgentRuntime) startSessionWithRun(t *Task, prompt agent.Prompt, run *Run) (*SessionHandle, error) {
	ctx := run.ctx
	if t.RuntimeInstanceID() == "" {
		return nil, errors.New("no instance")
	}
	if err := r.recordCommitBaseline(ctx, run.records, t.RuntimeInstanceID()); err != nil {
		return nil, err
	}
	opts := &agent.Options{Logger: r.Log, Target: t.RuntimeConnectionTarget(), Dir: r.runtimeDir(t), Model: t.RequestedModel, Effort: t.RequestedEffort, InitialPrompt: prompt, Records: run.records}
	if err := r.configureTaskMCP(t, opts); err != nil {
		return nil, err
	}
	if prompt.Text != "" || len(prompt.Images) > 0 {
		t.SetStateIfAny(task.StateStarting, task.StateWaiting, task.StateAsking, task.StateHasPlan)
	}
	session, err := r.Backends[t.Harness].Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	h := &SessionHandle{Session: session, Run: run}
	t.AttachSession(h)
	return h, nil
}

func (r *AgentRuntime) replaceSession(ctx context.Context, t *Task, prompt agent.Prompt, mode replaceSessionMode) (*SessionHandle, error) {
	state := t.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateStarting {
		return nil, fmt.Errorf("cannot %s in state %s", mode, state)
	}
	old, drainErr := t.CloseAndDetachSession(ctx)
	if old != nil {
		if drainErr != nil {
			return nil, errors.Join(drainErr, r.closeRun(t, old.Run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
		}
		if err := old.Run.joinTitles(); err != nil { //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
			return nil, errors.Join(err, r.closeRun(t, old.Run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
		}
		if err := old.Run.records.AppendMessage(ctx, agent.ContextCleared()); err != nil {
			return nil, errors.Join(err, r.closeRun(t, old.Run)) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
		}
		if err := r.closeRun(t, old.Run); err != nil { //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
			return nil, err
		}
	} else {
		if err := r.RecordHost(ctx, t, agent.ContextCleared()); err != nil {
			return nil, err
		}
	}
	r.NotifyTaskChange()
	if mode == replaceSessionClearContext {
		prompt = agent.Prompt{}
	}
	h, err := r.StartSession(ctx, t, prompt)
	if err != nil {
		t.SetStateUnless(task.StateFailed, task.StatePurging, task.StatePurged, task.StateStopping, task.StateStopped)
		return nil, err
	}
	if mode == replaceSessionClearContext {
		t.SetStateIf(task.StateStarting, task.StateWaiting)
	}
	return h, nil
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
func (r *AgentRuntime) recordCommitBaseline(ctx context.Context, records *agent.Records, id runtime.ID) error {
	if r.Checkout == nil || r.Runtimes == nil {
		return nil
	}
	commits := r.fetchTurnCommits(ctx, id)
	if len(commits) == 0 {
		return nil
	}
	return records.AppendMessage(ctx, agent.NewTurnCommitSnapshotMessage(commits, true, nil))
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

func (r *AgentRuntime) beginRun(ctx context.Context, t *Task, reopen, warm bool) (*Run, error) {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.shuttingDown.Load() {
		return nil, errors.New("task runtime is shutting down")
	}
	if t.run.Load() != nil {
		return nil, errors.New("execution already active")
	}
	var sink agent.LogSink
	var err error
	if reopen {
		sink, err = r.reopenLog(t)
	} else {
		sink, err = r.openLog(t)
	}
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	run := &Run{parent: ctx, ctx: lifetime, cancel: cancel, runtime: r, task: t, startupDone: make(chan struct{}), pendingMutating: make(map[agent.ToolKey]struct{})}
	run.records, err = agent.NewRecords(context.WithoutCancel(ctx), agent.ApplyRequestedEffort(r.Backends[t.Harness].NewWire(), t.RequestedEffort), sink, run)
	if err != nil {
		cancel(err)
		return nil, errors.Join(err, sink.Close())
	}
	t.run.Store(run)
	if t.shuttingDown.Load() {
		run.cancel(context.Canceled)
	}
	if warm {
		err = run.records.Replay(run.ctx, r.LogPath.Get()) //nolint:contextcheck // Reconstruction is interrupted by execution shutdown before attachment.
	}
	if err == nil {
		err = run.ctx.Err()
	}
	if err != nil {
		run.cancel(err)
		close(run.startupDone)
		t.run.Store(nil)
		return nil, errors.Join(err, run.close()) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	}

	return run, nil
}

// recordSettled requires recordingMu. It never installs a live execution and
// cannot compete with a live writer or a concurrent execution installation.
func (r *AgentRuntime) recordSettled(ctx context.Context, t *Task, messages []agent.Message, intent agent.RecordIntent) error {
	sink, err := r.reopenLog(t)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	run := &Run{parent: ctx, ctx: lifetime, cancel: cancel, runtime: r, task: t, pendingMutating: make(map[agent.ToolKey]struct{})}
	run.records, err = agent.NewRecords(ctx, agent.ApplyRequestedEffort(r.Backends[t.Harness].NewWire(), t.RequestedEffort), sink, run)
	if err != nil {
		cancel(err)
		return errors.Join(err, sink.Close())
	}
	if err = run.records.Replay(ctx, r.LogPath.Get()); err == nil {
		if intent != nil {
			err = run.records.CommitIntent(ctx, intent)
		} else {
			err = run.records.AppendMessages(ctx, messages)
		}
	}
	return errors.Join(err, run.close()) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
}

func (r *AgentRuntime) closeRun(t *Task, run *Run) error {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.run.Load() != run {
		return errors.New("execution ownership changed")
	}
	t.run.Store(nil)
	return run.close()
}

func (r *AgentRuntime) finalizeRun(ctx context.Context, t *Task, run *Run, res *task.Result) error {
	t.recordingMu.Lock()
	defer t.recordingMu.Unlock()
	if t.run.Load() != run {
		return errors.New("execution ownership changed")
	}
	joinErr := run.joinTitles() //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
	err := run.records.AppendMessage(ctx, taskhistory.ResultMessage(t.Title(), res))
	t.run.Store(nil)
	return errors.Join(joinErr, err, run.close()) //nolint:contextcheck // Final producer drain uses its execution owner, independently of request cancellation.
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
	ctx     context.Context
	records *agent.Records

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
	return w.records.AppendMessage(w.ctx, &agent.LogMessage{MessageType: "caic_log", Line: line})
}

// localShutdownError removes expected cancellation leaves after local shutdown,
// preserving independent transport and cleanup failures in joined error trees.
func localShutdownError(err error) error {
	if !errors.Is(err, context.Canceled) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures []error
		for _, child := range joined.Unwrap() {
			if failure := localShutdownError(child); failure != nil {
				failures = append(failures, failure)
			}
		}
		return errors.Join(failures...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			failure := localShutdownError(child)
			if failure == nil {
				return nil
			}
			// Keep the operation's diagnostic context while exposing only the
			// independent failures in the filtered error tree.
			return &shutdownError{message: err.Error(), cause: failure}
		}
	}
	return nil
}

// shutdownError retains a wrapping operation's diagnostic after cancellation
// leaves have been removed from its underlying failure tree.
type shutdownError struct {
	message string
	cause   error
}

func (e *shutdownError) Error() string { return e.message }
func (e *shutdownError) Unwrap() error { return e.cause }
