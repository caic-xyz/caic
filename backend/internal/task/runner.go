// Runner orchestrates task runtime lifecycle: branch/instance setup, agent
// session start, and result finalization. It composes RepoWorkspace (repo/git/
// runtime state) and SessionRunner (agent sessions/logs) rather than owning
// git or log details itself.

package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
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

// Result holds the outcome of a completed task.
type Result struct {
	State       State
	DiffStat    agent.DiffStat
	CostUSD     float64
	Duration    time.Duration
	NumTurns    int
	Usage       agent.Usage
	AgentResult string
	Err         error `json:"-"`
}

type persistedResult Result

// MarshalJSON preserves Result's error text in rebuildable task metadata.
func (r *Result) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	errText := ""
	if r.Err != nil {
		errText = r.Err.Error()
	}
	return json.Marshal(struct {
		persistedResult

		Error string `json:"error,omitempty"`
	}{persistedResult: persistedResult(*r), Error: errText})
}

// UnmarshalJSON restores Result's persisted error text from task metadata.
func (r *Result) UnmarshalJSON(data []byte) error {
	var decoded struct {
		persistedResult

		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = Result(decoded.persistedResult)
	if decoded.Error != "" {
		r.Err = errors.New(decoded.Error)
	}
	return nil
}

// Runner is the high-level task lifecycle orchestrator. It coordinates
// RepoWorkspace (branch/git/runtime) and SessionRunner (agent sessions/logs)
// to implement Start, Cleanup, StopTask, ReviveTask, and ForkTask. It holds no
// low-level git, log, or message reducer details itself.
type Runner struct {
	// Workspace is the task's primary repo workspace: it owns branch allocation
	// for repo 0, the shared runtime backend, and git config (timeout, logger).
	// It is a single value, not a slice, because a RepoWorkspace is a per-repo,
	// cross-task serialization point owned by the Manager registry, not a
	// per-task list. Extra repos' branches are allocated by the Manager on their
	// own workspaces before Start; multi-repo diff/sync then fans out from
	// t.RuntimeRepos() through the runtime backend, keyed by repo index.
	Workspace           *repowork.Workspace
	Sessions            *SessionRunner
	RuntimeStartTimeout time.Duration // Timeout for instance start (image pull). Must be non-zero.
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
func (r *Runner) Start(ctx context.Context, t *Task, resolvedGitHubToken string) (*SessionHandle, error) {
	ctx, task := trace.NewTask(ctx, "task.start:"+t.ID.String())
	defer task.End()

	if r.Workspace.Dir != "" {
		t.SetState(StateBranching)
	}
	// The Manager has already assigned every repo's branch name (the branch itself
	// is created below, concurrently with launch), so the log can open with the
	// durable, branch-derived filename and persist output from its first line.
	logW, err := r.Sessions.Logs.Open(t)
	if err != nil {
		t.recordStartupFailure(ctx, err)
		return nil, err
	}

	tStart := time.Now()
	// 1. Create branch (serialized) + start instance (concurrent).
	r.Workspace.Log.Info("setup task")
	region := trace.StartRegion(ctx, "setup")
	sr, err := r.setup(ctx, t, MakeMetadata(t), resolvedGitHubToken, logW)
	region.End()
	if err != nil {
		return nil, r.finishStartupFailure(ctx, t, logW, err)
	}
	t.SetRuntimeConnectionInfo(sr.InstanceID, sr.AgentTarget, sr.TailscaleFQDN, sr.TailscaleAuthURL, r.Workspace.Runtimes.VNCPort(ctx, sr.InstanceID))
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Workspace.Log.Info("workspace", "msg", "ready", "br", primaryBranch, "instance", sr.InstanceID, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(StateStarting)
	var msgCh chan agent.ParsedMessage
	var dispatchDone <-chan struct{}
	{
		region := trace.StartRegion(ctx, "dispatch-init")
		msgCh, dispatchDone = r.Sessions.startMessageDispatch(ctx, t, false)
		region.End()
	}

	tSession := time.Now()
	tlog := r.Workspace.Log.With("br", primaryBranch, "instance", sr.InstanceID)
	tlog.Info("starting session", "hns", t.Harness)
	region = trace.StartRegion(ctx, "agent-session")
	target := sr.AgentTarget
	session, err := r.Sessions.backend(t.Harness).Start(ctx, &agent.Options{
		Target:        target,
		Dir:           r.Sessions.runtimeDir(t),
		Model:         t.Model,
		Effort:        t.Effort,
		InitialPrompt: t.InitialPrompt,
		LogVersion:    t.RelayLogVersion(),
		MsgCh:         msgCh,
		LogW:          logW,
	})
	region.End()
	if err != nil {
		close(msgCh)
		<-dispatchDone
		tlog.Error("session start failed", "err", err)
		return nil, r.finishStartupFailure(ctx, t, logW, err)
	}

	// Store handle so SendInput can reach it.
	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(t.InitialPrompt), false)
	// Use SetStateIf so that a fast agent subprocess that already
	// produced a result (and was processed by the dispatch goroutine
	// via addMessage) isn't overwritten back to Running.
	t.SetStateIf(StateStarting, StateRunning)
	tlog.Info("agent running", "session_dur", time.Since(tSession), "total_startup_dur", time.Since(tStart))
	return h, nil
}

// Cleanup is the single shutdown path for a task (Flow 1 in the relay
// shutdown protocol — see package agent). It sends the null-byte sentinel
// to trigger graceful agent exit, then purges the instance.
//
// This is only called for intentional purge (user action or instance
// death), never during backend restart. On restart, the relay daemon stays
// alive and the server reconnects via adoptOne → Reconnect.
//
// Steps:
//  1. Detach the session handle from the task.
//  2. If a session exists: Stop (sends \x00, waits up to 20s), then Close.
//  3. Set task state to reason (StatePurged or StateFailed).
//  4. Purge the instance (stop + remove + cleanup git remotes/runtime config).
//  5. If graceful wait timed out, drain session now (runtime connection severed).
//  6. Close msgCh and logW, write log trailer.
//  7. Build and return Result.
func (r *Runner) Cleanup(ctx context.Context, t *Task, reason State) Result {
	ctx, task := trace.NewTask(ctx, "task.cleanup:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Workspace.Log.With("br", primaryBranch, "instance", name)
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
	if reason == StatePurged && !t.DiffCreated() && name != "" && r.Workspace.Dir != "" {
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
		purgeCtx, purgeCancel := context.WithTimeout(context.WithoutCancel(ctx), r.Workspace.GitTimeout)
		err := r.Workspace.Runtimes.Purge(purgeCtx, name)
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

	res := Result{
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
	if reason == StatePurged && runtimeRemovedOrAbsent && branchConfirmedEmpty {
		r.Workspace.DeleteUnmodifiedTaskBranches(ctx, t)
	}
	var logW io.WriteCloser
	if h != nil {
		logW = h.LogW
	} else {
		// Task was stopped before purge: the session handle (and its LogW) was
		// released by StopTask. Reopen the log for appending so we can write
		// the caic_result trailer; without it the task would load as "failed"
		// on the next server restart instead of "purged".
		tlog.DebugContext(ctx, "cleanup: no session handle, reopening log for trailer")
		var reopenErr error
		logW, reopenErr = r.Sessions.Logs.Reopen(t)
		if reopenErr != nil {
			tlog.WarnContext(ctx, "reopen log for trailer failed", "err", reopenErr)
		}
	}
	trailerErr := r.Sessions.Logs.WriteResultTrailer(logW, t.Title(), &res)
	if trailerErr != nil {
		tlog.WarnContext(ctx, "write log trailer failed", "err", trailerErr)
	}
	if logW != nil {
		closeErr := logW.Close()
		if closeErr != nil {
			tlog.WarnContext(ctx, "close log failed", "err", closeErr)
		} else {
			tlog.DebugContext(ctx, "cleanup: log trailer written and closed")
		}
		if trailerErr == nil && closeErr == nil {
			if err := t.compressLogIfDone(reason); err != nil {
				tlog.WarnContext(ctx, "compress task log failed", "err", err)
			}
		}
	}
	if err := t.CommitEventReplay(ctx); err != nil {
		tlog.WarnContext(ctx, "commit event replay failed", "err", err)
	}
	tlog.InfoContext(ctx, "cleanup done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns, "reason", reason)
	return res
}

// StopTask gracefully shuts down the agent session and stops the instance
// without removing it. The instance can be revived later. Unlike Cleanup,
// this preserves git remotes and runtime config.
func (r *Runner) StopTask(ctx context.Context, t *Task) {
	ctx, task := trace.NewTask(ctx, "task.stop:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.Workspace.Log.With("br", primaryBranch, "instance", name)
	tlog.InfoContext(ctx, "stop starting", "state", t.GetState())
	if _, changed := t.SetStateUnless(StateStopping, StatePurging, StatePurged, StateCrashed, StateFailed, StateStopped); !changed {
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
		if err := r.Workspace.Runtimes.Stop(ctx, name); err != nil {
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

	if _, changed := t.SetStateUnless(StateStopped, StatePurging, StatePurged, StateCrashed, StateFailed); !changed {
		if h != nil && h.LogW != nil {
			_ = h.LogW.Close()
		}
		if err := t.CommitEventReplay(ctx); err != nil {
			tlog.WarnContext(ctx, "commit event replay failed", "err", err)
		}
		tlog.InfoContext(ctx, "stop abandoned", "state", t.GetState())
		return
	}

	// Write log trailer so the task reloads as "stopped" (not "failed")
	// after a server restart, preserving live stats for the UI.
	res := Result{State: StateStopped, AgentResult: t.LastAgentResult()}
	if liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats(); liveCost > 0 {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.Duration = liveDur
		res.Usage = liveUsage
	}
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	var logW io.WriteCloser
	if h != nil {
		logW = h.LogW
	}
	if err := r.Sessions.Logs.WriteResultTrailer(logW, t.Title(), &res); err != nil {
		tlog.WarnContext(ctx, "write log trailer failed", "err", err)
	}
	if logW != nil {
		_ = logW.Close()
	}
	if err := t.CommitEventReplay(ctx); err != nil {
		tlog.WarnContext(ctx, "commit event replay failed", "err", err)
	}
	tlog.InfoContext(ctx, "stop done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns)
}

// ReviveTask restarts a stopped or crashed instance and resumes the agent session.
// The instance's filesystem is preserved from the previous run.
func (r *Runner) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
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
	tlog := r.Workspace.Log.With("br", primaryBranch, "instance", instanceID)

	// 1. Revive the instance.
	if state, changed := t.SetStateIfAny(StateProvisioning, StateStopped, StateCrashed, StateProvisioning); !changed {
		return nil, fmt.Errorf("cannot revive in state %s", state)
	}
	tlog.Info("reviving instance")
	tlog.Debug("workspace", "msg", "calling instance.Revive")
	if err := r.Workspace.Runtimes.Revive(ctx, instanceID); err != nil {
		tlog.Error("workspace", "msg", "Revive failed", "err", err)
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("revive instance: %w", err)
	}
	tlog.Debug("workspace", "msg", "Revive succeeded", "instance", instanceID)
	t.SetVNCPort(r.Workspace.Runtimes.VNCPort(ctx, instanceID))

	// 2. Start a new relay with --resume to continue the previous session.
	// skipSideEffects=true: --resume replays all historical messages and
	// each would trigger fetch+diff+title if side effects were enabled.
	// Instead we do a single BranchDiffStat at the end.
	t.SetState(StateStarting)
	tlog.Info("resuming session after revive", "sess", t.GetSessionID())

	msgCh, dispatchDone := r.Sessions.startMessageDispatch(ctx, t, true)
	logW, err := r.Sessions.Logs.Open(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("open log: %w", err)
	}

	t.SetState(StateRunning)
	target := t.RuntimeConnectionTarget()
	session, err := r.Sessions.backend(t.Harness).Start(ctx, &agent.Options{
		Target:          target,
		Dir:             r.Sessions.runtimeDir(t),
		Model:           t.Model,
		Effort:          t.Effort,
		ResumeSessionID: t.GetSessionID(),
		LogVersion:      t.RelayLogVersion(),
		MsgCh:           msgCh,
		LogW:            logW,
	})
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("resume session after revive: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)

	// 3. If --resume exits immediately (previous session was complete),
	// start a fresh idle relay so the task can accept new prompts.
	h, err = r.Sessions.EnsureSession(ctx, t, h, tlog)
	if err != nil {
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, err
	}

	// 4. Compute host-side diff stat once.
	if ds := r.Workspace.BranchDiffStat(ctx, t); len(ds) > 0 {
		t.SetLiveDiffStat(ds)
	}
	tlog.Info("agent ready after revive", "state", t.GetState())
	return h, nil
}

// ForkTask snapshots the source task's instance and starts an idle agent
// session in the forked instance. The new task must already have its ID,
// Harness, Model, and other immutable fields set. The method fills in
// Runtime, Repos[*].Branch, and starts the session.
func (r *Runner) ForkTask(ctx context.Context, source, fork *Task, forkOpts *runtime.ForkOptions, resolvedGitHubToken string) (*SessionHandle, error) {
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
	tlog := r.Workspace.Log.With("src_br", sourcePrimaryBranch, "src_instance", sourceInstanceID)

	// Every fork branch — primary and extras alike — was assigned by the Manager
	// before ForkTask, so the log can open with a correct metadata header up
	// front and provisioning output is durable from the first line.
	fork.SetState(StateProvisioning)
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

	logW, err := r.Sessions.Logs.Open(fork)
	if err != nil {
		fork.recordStartupFailure(ctx, err)
		return nil, err
	}

	// 2. Fork the runtime instance. The runtime creates exactly the branch we
	// reserved (it owns uniqueness otherwise); provisioning output streams
	// straight to the already-open log.
	tlog.Info("forking instance", "br", forkBranch)
	tlog.Debug("workspace", "msg", "calling instance.Fork", "source", sourceInstanceID, "harness", forkOpts.Harness, "tailscale", forkOpts.Tailscale, "usb", forkOpts.USB, "display", forkOpts.Display, "sudo", forkOpts.Sudo, "gitHubToken", fork.GitHubTokenEnabled())
	provisioningLog := &provisioningWriter{ctx: ctx, t: fork, logW: logW}
	forkOpts.LogWriter = provisioningLog
	forkName, forkConn, forkRepos, err := r.Workspace.Runtimes.Fork(ctx, sourceInstanceID, forkOpts)
	if flushErr := provisioningLog.Flush(); flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		tlog.Error("workspace", "msg", "instance.Fork failed", "source", sourceInstanceID, "err", err)
		return nil, r.finishStartupFailure(ctx, fork, logW, fmt.Errorf("fork instance: %w", err))
	}
	tlog.Debug("workspace", "msg", "instance.Fork succeeded", "source", sourceInstanceID, "fork", forkName)
	fork.SetRuntimeConnectionInfo(forkName, forkConn.AgentTarget, "", "", r.Workspace.Runtimes.VNCPort(ctx, forkName))
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
	fork.SetState(StateStarting)
	h, err := r.Sessions.startSessionWithLog(ctx, fork, fork.InitialPrompt, logW)
	if err != nil {
		startupErr := fmt.Errorf("start session on fork: %w", err)
		return nil, r.finishStartupFailure(ctx, fork, logW, startupErr)
	}
	tlog.Info("fork session running", "instance", forkName)
	return h, nil
}

func (r *Runner) branchDiffStat(ctx context.Context, t *Task) (agent.DiffStat, error) {
	if r.Workspace.Dir == "" {
		return nil, nil
	}
	id := t.RuntimeInstanceID()
	if id == "" {
		return nil, errors.New("task has no runtime instance")
	}
	repos := t.RuntimeRepos()
	if len(repos) > 0 && repos[0].GitRoot == "" {
		repos[0].GitRoot = r.Workspace.Dir
	}
	return r.Workspace.DiffStat(ctx, id, repos, repowork.DiffFetchRequired, "fetch for branch diff stat")
}

// setup reserves a branch name, starts the instance (Phase A) and creates the
// git branch concurrently, then completes instance startup (Phase B).
// Phase A (runtime launch) and git fetch+branch-create overlap, cutting the
// branch-allocation time off the critical path.
func (r *Runner) setup(ctx context.Context, t *Task, metadata runtime.Metadata, resolvedGitHubToken string, logW io.Writer) (setupResult, error) {
	t.SetState(StateProvisioning)
	detached := context.WithoutCancel(ctx)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.Workspace.Log.Info("starting instance", "br", primaryBranch, "img", t.BaseImage, "platform", t.ContainerPlatform, "hns", t.Harness, "ts", t.Tailscale, "usb", t.USB, "dpy", t.Display, "sudo", t.Sudo, "gitHubToken", t.GitHubTokenEnabled())
	tContainer := time.Now()
	startCtx, startCancel := context.WithTimeout(detached, r.RuntimeStartTimeout)
	defer startCancel()

	runtimeName := t.RuntimeName
	if runtimeName == "" {
		runtimeName = r.Workspace.Runtimes.Runtimes[0].Name()
	}
	provisioningLog := &provisioningWriter{ctx: ctx, t: t, logW: logW}
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
	if r.Workspace.Dir != "" {
		repos = t.RuntimeRepos()
	}
	var instanceID runtime.ID
	r.Workspace.Log.Debug("workspace", "msg", "provisioning phase A: launching instance and creating branch", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo, "repos_count", len(repos))
	eg, egCtx := errgroup.WithContext(startCtx)
	eg.Go(func() error {
		defer trace.StartRegion(egCtx, "instance-launch").End()
		r.Workspace.Log.Debug("workspace", "msg", "calling instance.Launch", "branch", primaryBranch)
		id, err := r.Workspace.Runtimes.Launch(egCtx, repos, opts)
		if err != nil {
			r.Workspace.Log.Error("workspace", "msg", "instance.Launch failed", "branch", primaryBranch, "err", err)
			return err
		}
		r.Workspace.Log.Debug("workspace", "msg", "instance.Launch succeeded", "instance", id)
		instanceID = id
		return nil
	})
	if r.Workspace.Dir != "" {
		eg.Go(func() error {
			defer trace.StartRegion(egCtx, "branch-create").End()
			r.Workspace.Log.Debug("workspace", "msg", "fetching and creating branch", "branch", primaryBranch)
			err := r.Workspace.FetchAndCreateBranch(egCtx, t, primaryBranch)
			if err != nil {
				r.Workspace.Log.Error("workspace", "msg", "fetchAndCreateBranch failed", "branch", primaryBranch, "err", err)
			} else {
				r.Workspace.Log.Debug("workspace", "msg", "fetchAndCreateBranch succeeded", "branch", primaryBranch)
			}
			return err
		})
	}
	r.Workspace.Log.Debug("workspace", "msg", "waiting for phase A errgroup")
	if err := eg.Wait(); err != nil {
		return setupResult{}, errors.Join(err, provisioningLog.Flush())
	}
	if err := provisioningLog.Flush(); err != nil {
		return setupResult{}, err
	}
	r.Workspace.Log.Debug("workspace", "msg", "phase A complete", "instance", instanceID)

	// Phase B: wait for runtime connection + push (branch now exists locally).
	r.Workspace.Log.Debug("workspace", "msg", "provisioning phase B: connecting to instance", "instance", instanceID)
	conn, err := r.Workspace.Runtimes.Connect(startCtx, instanceID, opts)
	if err != nil {
		r.Workspace.Log.Error("workspace", "msg", "instance.Connect failed", "instance", instanceID, "err", err)
		return setupResult{}, errors.Join(fmt.Errorf("start instance: %w", err), provisioningLog.Flush())
	}
	if err := provisioningLog.Flush(); err != nil {
		return setupResult{}, err
	}
	r.Workspace.Log.Info("workspace", "msg", "started", "br", primaryBranch, "dur", time.Since(tContainer), "instance", instanceID, "fqdn", conn.TailscaleFQDN)
	return setupResult{
		InstanceID:       instanceID,
		AgentTarget:      conn.AgentTarget,
		TailscaleFQDN:    conn.TailscaleFQDN,
		TailscaleAuthURL: conn.TailscaleAuthURL,
	}, nil
}

// finishStartupFailure records a startup error in the task log and finalizes
// its event replay so the failure survives a server restart.
func (r *Runner) finishStartupFailure(ctx context.Context, t *Task, logW io.WriteCloser, startupErr error) error {
	failure := &agent.LogMessage{MessageType: "caic_log", Line: "Task startup failed: " + startupErr.Error()}
	writeErr := appendTaskLogMessage(logW, failure)
	t.SetState(StateFailed)
	t.addMessage(ctx, failure, false)

	res := Result{State: StateFailed, Err: startupErr}
	trailerErr := r.Sessions.Logs.WriteResultTrailer(logW, t.Title(), &res)
	closeErr := logW.Close()
	commitErr := t.CommitEventReplay(ctx)
	return errors.Join(startupErr, writeErr, trailerErr, closeErr, commitErr)
}

// appendTaskLogMessage appends a backend-neutral task message to an open task log.
func appendTaskLogMessage(w io.Writer, m agent.Message) error {
	if w == nil {
		return ErrNoLog
	}
	data, err := agent.MarshalMessage(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// logRelayDiag reads the relay daemon's relay.log from the instance and logs
// its tail. Called when GracefulStop times out to capture relay-side diagnostics.
func (r *Runner) logRelayDiag(ctx context.Context, tlog *slog.Logger, target runtime.ConnectionTarget) {
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
	ctx  context.Context
	t    *Task
	logW io.Writer

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
	if w.logW != nil {
		if err := appendTaskLogMessage(w.logW, m); err != nil {
			return err
		}
	}
	w.t.addMessage(w.ctx, m, false)
	return nil
}
