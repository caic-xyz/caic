// RepoExecutor orchestrates task runtime lifecycle and agent sessions.
// Repo branch/git/fetch/diff work belongs to RepoWorkspace.

package task

import (
	"bytes"
	"context"
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
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Result holds the outcome of a completed task.
type Result struct {
	State       State
	DiffStat    agent.DiffStat
	CostUSD     float64
	Duration    time.Duration
	NumTurns    int
	Usage       agent.Usage
	AgentResult string
	Err         error
}

// RepoExecutor orchestrates task lifecycle and agent sessions for a repo.
// Repo-level branch/git/fetch/diff state lives in the embedded RepoWorkspace.
type RepoExecutor struct {
	RepoWorkspace

	RuntimeStartTimeout time.Duration       // Timeout for instance start (image pull); defaults to 1 hour.
	LogDir              string              // Directory for raw JSONL session logs (required).
	CacheDir            string              // Cache directory (e.g. ~/.cache/caic) for harness model lists.
	HarnessEnv          map[string][]string // Per-harness KEY=VALUE env vars for runtime instances.
	EventReplayFactory  func(logPath string, h harness.Name) EventReplayWriter

	// Backends maps harness names to their Backend implementations. The executor
	// selects the backend matching Task.Harness.
	Backends map[harness.Name]agent.Backend

	initOnce sync.Once
}

// provisioningWriter is an io.Writer that converts line-by-line output from the
// instance backend into LogMessage events stored on the task for SSE streaming.
type provisioningWriter struct {
	ctx context.Context
	t   *Task
	buf []byte
}

func (w *provisioningWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.t.addMessage(w.ctx, &agent.LogMessage{Line: line}, false)
		}
	}
	return len(p), nil
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
func (r *RepoExecutor) Start(ctx context.Context, t *Task, resolvedGitHubToken string) (*SessionHandle, error) {
	r.initDefaults()
	ctx, task := trace.NewTask(ctx, "task.start:"+t.ID.String())
	defer task.End()

	if r.Runtime == nil {
		return nil, errors.New("executor has no instance backend configured")
	}
	if r.Dir != "" {
		t.SetState(StateBranching)
	}

	tStart := time.Now()
	// 1. Create branch (serialized) + start instance (concurrent).
	r.log.Info("setup task")
	region := trace.StartRegion(ctx, "setup")
	sr, err := r.setup(ctx, t, MakeMetadata(t), resolvedGitHubToken)
	region.End()
	if err != nil {
		t.recordStartupFailure(ctx, err)
		return nil, err
	}
	t.SetRuntimeConnectionInfo(sr.InstanceID, sr.AgentTarget, sr.TailscaleFQDN, sr.TailscaleAuthURL, r.Runtime.VNCPort(ctx, sr.InstanceID))
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.log.Info("executor", "msg", "ready", "br", primaryBranch, "instance", sr.InstanceID, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(StateStarting)
	sessions := r.sessionRunner()
	var msgCh chan agent.Message
	var dispatchDone <-chan struct{}
	var logW io.WriteCloser
	{
		region := trace.StartRegion(ctx, "dispatch-init")
		msgCh, dispatchDone = sessions.startMessageDispatch(ctx, t, false)
		logW, err = r.logStore().Open(t)
		region.End()
	}
	if err != nil {
		close(msgCh)
		<-dispatchDone
		t.recordStartupFailure(ctx, err)
		return nil, err
	}

	tSession := time.Now()
	tlog := r.log.With("br", primaryBranch, "instance", sr.InstanceID)
	tlog.Info("starting session", "hns", t.Harness)
	region = trace.StartRegion(ctx, "agent-session")
	target := sr.AgentTarget
	session, err := sessions.backend(t.Harness).Start(ctx, &agent.Options{
		Target:        target,
		Dir:           sessions.runtimeDir(t),
		Model:         t.Model,
		Effort:        t.Effort,
		InitialPrompt: t.InitialPrompt,
		MsgCh:         msgCh,
		LogW:          logW,
	})
	region.End()
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		t.recordStartupFailure(ctx, err)
		tlog.Error("session start failed", "err", err)
		return nil, err
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
func (r *RepoExecutor) Cleanup(ctx context.Context, t *Task, reason State) Result {
	r.initDefaults()
	ctx, task := trace.NewTask(ctx, "task.cleanup:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "instance", name)
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

	t.SetState(reason)

	if name != "" && r.Runtime != nil {
		tlog.InfoContext(ctx, "cleanup: purging instance")
		pStart := time.Now()
		purgeCtx, purgeCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
		err := r.Runtime.Purge(purgeCtx, name)
		purgeCancel()
		if err != nil {
			tlog.WarnContext(ctx, "purge instance failed", "err", err, "dur", time.Since(pStart).Round(time.Millisecond))
		} else {
			tlog.DebugContext(ctx, "cleanup: instance purged", "dur", time.Since(pStart).Round(time.Millisecond))
		}
	} else {
		tlog.DebugContext(ctx, "cleanup: no instance to purge", "name", name, "has_backend", r.Runtime != nil)
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
		logW, reopenErr = r.logStore().Reopen(t)
		if reopenErr != nil {
			tlog.WarnContext(ctx, "reopen log for trailer failed", "err", reopenErr)
		}
	}
	trailerErr := r.logStore().WriteResultTrailer(logW, t.Title(), &res)
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
	t.CommitEventReplay()
	tlog.InfoContext(ctx, "cleanup done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns, "reason", reason)
	return res
}

// StopTask gracefully shuts down the agent session and stops the instance
// without removing it. The instance can be revived later. Unlike Cleanup,
// this preserves git remotes and runtime config.
func (r *RepoExecutor) StopTask(ctx context.Context, t *Task) {
	r.initDefaults()
	ctx, task := trace.NewTask(ctx, "task.stop:"+t.ID.String())
	defer task.End()

	start := time.Now()
	name := t.RuntimeInstanceID()
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "instance", name)
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
	if name != "" && r.Runtime != nil {
		cStart := time.Now()
		if err := r.Runtime.Stop(ctx, name); err != nil {
			tlog.WarnContext(ctx, "stop: instance Stop failed", "err", err, "dur", time.Since(cStart).Round(time.Millisecond))
		} else {
			tlog.DebugContext(ctx, "stop: instance Stop succeeded", "dur", time.Since(cStart).Round(time.Millisecond))
		}
	} else {
		tlog.DebugContext(ctx, "stop: no instance to stop", "name", name, "has_backend", r.Runtime != nil)
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
		t.CommitEventReplay()
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
	if err := r.logStore().WriteResultTrailer(logW, t.Title(), &res); err != nil {
		tlog.WarnContext(ctx, "write log trailer failed", "err", err)
	}
	if logW != nil {
		_ = logW.Close()
	}
	t.CommitEventReplay()
	tlog.InfoContext(ctx, "stop done", "dur", time.Since(start).Round(time.Millisecond),
		"cost", res.CostUSD, "turns", res.NumTurns)
}

// ReviveTask restarts a stopped or crashed instance and resumes the agent session.
// The instance's filesystem is preserved from the previous run.
func (r *RepoExecutor) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	ctx, task := trace.NewTask(ctx, "task.revive:"+t.ID.String())
	defer task.End()

	if r.Runtime == nil {
		return nil, errors.New("executor has no instance backend configured")
	}
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		return nil, errors.New("no instance to revive")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "instance", instanceID)

	// 1. Revive the instance.
	if state, changed := t.SetStateIfAny(StateProvisioning, StateStopped, StateCrashed, StateProvisioning); !changed {
		return nil, fmt.Errorf("cannot revive in state %s", state)
	}
	tlog.Info("reviving instance")
	tlog.Debug("executor", "msg", "calling instance.Revive")
	if err := r.Runtime.Revive(ctx, instanceID); err != nil {
		tlog.Error("executor", "msg", "Revive failed", "err", err)
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("revive instance: %w", err)
	}
	tlog.Debug("executor", "msg", "Revive succeeded", "instance", instanceID)
	t.SetVNCPort(r.Runtime.VNCPort(ctx, instanceID))

	// 2. Start a new relay with --resume to continue the previous session.
	// skipSideEffects=true: --resume replays all historical messages and
	// each would trigger fetch+diff+title if side effects were enabled.
	// Instead we do a single BranchDiffStat at the end.
	t.SetState(StateStarting)
	tlog.Info("resuming session after revive", "sess", t.GetSessionID())

	sessions := r.sessionRunner()
	msgCh, dispatchDone := sessions.startMessageDispatch(ctx, t, true)
	logW, err := r.logStore().Open(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("open log: %w", err)
	}

	t.SetState(StateRunning)
	target := t.RuntimeConnectionTarget()
	session, err := sessions.backend(t.Harness).Start(ctx, &agent.Options{
		Target:          target,
		Dir:             sessions.runtimeDir(t),
		Model:           t.Model,
		Effort:          t.Effort,
		ResumeSessionID: t.GetSessionID(),
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
	h, err = r.EnsureSession(ctx, t, h, tlog)
	if err != nil {
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, err
	}

	// 4. Compute host-side diff stat once.
	if ds := r.BranchDiffStat(ctx, t); len(ds) > 0 {
		t.SetLiveDiffStat(ds)
	}
	tlog.Info("agent ready after revive", "state", t.GetState())
	return h, nil
}

// ForkTask snapshots the source task's instance and starts an idle agent
// session in the forked instance. The new task must already have its ID,
// Harness, Model, and other immutable fields set. The method fills in
// Runtime, Repos[*].Branch, and starts the session.
func (r *RepoExecutor) ForkTask(ctx context.Context, source, fork *Task, forkOpts *runtime.ForkOptions, resolvedGitHubToken string) (*SessionHandle, error) {
	r.initDefaults()
	ctx, task := trace.NewTask(ctx, "task.fork:"+source.ID.String()+"->"+fork.ID.String())
	defer task.End()

	if r.Runtime == nil {
		return nil, errors.New("executor has no instance backend configured")
	}
	sourceInstanceID := source.RuntimeInstanceID()
	if sourceInstanceID == "" {
		return nil, errors.New("source task has no instance")
	}

	var sourcePrimaryBranch string
	if p := source.Primary(); p != nil {
		sourcePrimaryBranch = p.Branch
	}
	tlog := r.log.With("src_br", sourcePrimaryBranch, "src_instance", sourceInstanceID)

	// 1. Fork the runtime instance. Branch names are generated by the runtime adapter.
	fork.SetState(StateProvisioning)
	tlog.Info("forking instance")
	tlog.Debug("executor", "msg", "calling instance.Fork", "source", sourceInstanceID, "harness", forkOpts.Harness, "tailscale", forkOpts.Tailscale, "usb", forkOpts.USB, "display", forkOpts.Display, "sudo", forkOpts.Sudo, "gitHubToken", fork.GitHubTokenEnabled())
	forkOpts.LogWriter = &provisioningWriter{ctx: ctx, t: fork}
	forkName, forkConn, forkRepos, err := r.Runtime.Fork(ctx, sourceInstanceID, source.RuntimeRepos(), forkOpts)
	if err != nil {
		tlog.Error("executor", "msg", "instance.Fork failed", "source", sourceInstanceID, "err", err)
		fork.SetState(StateFailed)
		return nil, fmt.Errorf("fork instance: %w", err)
	}
	tlog.Debug("executor", "msg", "instance.Fork succeeded", "source", sourceInstanceID, "fork", forkName)
	fork.SetRuntimeConnectionInfo(forkName, forkConn.AgentTarget, "", "", r.Runtime.VNCPort(ctx, forkName))
	for i := range fork.ReposSnapshot() {
		if i < len(forkRepos) {
			fork.SetRepoBranch(i, forkRepos[i].Branch)
		}
	}
	tlog.Info("fork instance ready", "instance", forkName)

	// 2. Clean relay state from the source instance's snapshot so the
	// forked task starts with an empty output.jsonl.
	if err := agent.CleanRelayState(ctx, string(forkName)); err != nil {
		tlog.Warn("clean relay state failed (non-fatal)", "err", err)
	}

	// 3. Start a fresh agent session with the fork prompt.
	// No --resume: the fork gets its own session ID and clean message history.
	fork.SetState(StateStarting)
	h, err := r.StartSession(ctx, fork, fork.InitialPrompt)
	if err != nil {
		fork.SetState(StateFailed)
		return nil, fmt.Errorf("start session on fork: %w", err)
	}
	tlog.Info("fork session running", "instance", forkName)
	return h, nil
}

// setupResult holds the outputs of setup: the instance name and optional Tailscale FQDN.
// The primary branch is written into the task repo metadata during setup.
type setupResult struct {
	InstanceID       runtime.InstanceID
	AgentTarget      runtime.ConnectionTarget
	TailscaleFQDN    string
	TailscaleAuthURL string
}

// AllocateBranch allocates a caic-N branch through the repo workspace.
func (r *RepoExecutor) AllocateBranch(ctx context.Context) (string, error) {
	r.initDefaults()
	return r.RepoWorkspace.AllocateBranch(ctx)
}

// SyncToOrigin delegates repo sync to the workspace.
func (r *RepoExecutor) SyncToOrigin(ctx context.Context, t *Task, force bool) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	return r.RepoWorkspace.SyncToOrigin(ctx, t, force)
}

// SyncToDefault delegates default-branch sync to the workspace.
func (r *RepoExecutor) SyncToDefault(ctx context.Context, t *Task, message string) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	return r.RepoWorkspace.SyncToDefault(ctx, t, message)
}

// Reconnect delegates agent relay reconnection to SessionRunner.
func (r *RepoExecutor) Reconnect(ctx context.Context, t *Task, skipSideEffects bool) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().Reconnect(ctx, t, skipSideEffects)
}

// EnsureSession delegates live-session confirmation to SessionRunner.
func (r *RepoExecutor) EnsureSession(ctx context.Context, t *Task, h *SessionHandle, tlog *slog.Logger) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().EnsureSession(ctx, t, h, tlog)
}

// StartSession delegates existing-instance session start to SessionRunner.
func (r *RepoExecutor) StartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().StartSession(ctx, t, prompt)
}

// RestartSession delegates context replacement to SessionRunner.
func (r *RepoExecutor) RestartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().RestartSession(ctx, t, prompt)
}

// ClearContextSession delegates context clearing to SessionRunner.
func (r *RepoExecutor) ClearContextSession(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().ClearContextSession(ctx, t)
}

// DiffContent delegates repo diff rendering to the workspace.
func (r *RepoExecutor) DiffContent(ctx context.Context, t *Task, path string) (string, error) {
	r.initDefaults()
	return r.RepoWorkspace.DiffContent(ctx, t, path)
}

// BranchDiffStat delegates host-side diff stat restoration to the workspace.
func (r *RepoExecutor) BranchDiffStat(ctx context.Context, t *Task) agent.DiffStat {
	r.initDefaults()
	return r.RepoWorkspace.BranchDiffStat(ctx, t)
}

func (r *RepoExecutor) taskRuntime(t *Task) (runtime.InstanceID, []runtime.Repo, error) {
	r.initDefaults()
	return r.RepoWorkspace.taskRuntime(t)
}

// initDefaults populates timeout values and harness backend maps.
// Safe to call multiple times (sync.Once).
func (r *RepoExecutor) initDefaults() {
	r.RepoWorkspace.initDefaults()
	r.initOnce.Do(func() {
		if r.Backends == nil {
			r.Backends = map[harness.Name]agent.Backend{}
		}
		if r.RuntimeStartTimeout == 0 {
			r.RuntimeStartTimeout = time.Hour
		}
	})
}

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

// setup reserves a branch name, starts the instance (Phase A) and creates the
// git branch concurrently, then completes instance startup (Phase B).
// Phase A (runtime launch) and git fetch+branch-create overlap, cutting the
// branch-allocation time off the critical path.
func (r *RepoExecutor) setup(ctx context.Context, t *Task, metadata runtime.Metadata, resolvedGitHubToken string) (setupResult, error) {
	// Reserve the branch ID instantly (under lock, ~µs). The branch itself is
	// created concurrently with runtime launch in Phase A.
	r.reserveBranch(t)

	t.SetState(StateProvisioning)
	detached := context.WithoutCancel(ctx)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.log.Info("starting instance", "br", primaryBranch, "img", t.BaseImage, "platform", t.ContainerPlatform, "hns", t.Harness, "ts", t.Tailscale, "usb", t.USB, "dpy", t.Display, "sudo", t.Sudo, "gitHubToken", t.GitHubTokenEnabled())
	tContainer := time.Now()
	startCtx, startCancel := context.WithTimeout(detached, r.RuntimeStartTimeout)
	defer startCancel()

	opts := &runtime.StartOptions{
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
		LogWriter:         &provisioningWriter{ctx: ctx, t: t},
	}

	// Phase A: runtime launch + connection config. Branch creation runs concurrently so
	// git fetch overlaps with instance connection startup.
	var repos []runtime.Repo
	if r.Dir != "" {
		repos = t.RuntimeRepos()
	}
	var instanceID runtime.InstanceID
	r.log.Debug("executor", "msg", "provisioning phase A: launching instance and creating branch", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo, "repos_count", len(repos))
	eg, egCtx := errgroup.WithContext(startCtx)
	eg.Go(func() error {
		defer trace.StartRegion(egCtx, "instance-launch").End()
		r.log.Debug("executor", "msg", "calling instance.Launch", "branch", primaryBranch)
		id, err := r.Runtime.Launch(egCtx, repos, opts)
		if err != nil {
			r.log.Error("executor", "msg", "instance.Launch failed", "branch", primaryBranch, "err", err)
			return err
		}
		r.log.Debug("executor", "msg", "instance.Launch succeeded", "instance", id)
		instanceID = id
		return nil
	})
	if r.Dir != "" {
		eg.Go(func() error {
			defer trace.StartRegion(egCtx, "branch-create").End()
			r.log.Debug("executor", "msg", "fetching and creating branch", "branch", primaryBranch)
			err := r.fetchAndCreateBranch(egCtx, t, primaryBranch)
			if err != nil {
				r.log.Error("executor", "msg", "fetchAndCreateBranch failed", "branch", primaryBranch, "err", err)
			} else {
				r.log.Debug("executor", "msg", "fetchAndCreateBranch succeeded", "branch", primaryBranch)
			}
			return err
		})
	}
	r.log.Debug("executor", "msg", "waiting for phase A errgroup")
	if err := eg.Wait(); err != nil {
		return setupResult{}, err
	}
	r.log.Debug("executor", "msg", "phase A complete", "instance", instanceID)

	// Phase B: wait for runtime connection + push (branch now exists locally).
	r.log.Debug("executor", "msg", "provisioning phase B: connecting to instance", "instance", instanceID)
	conn, err := r.Runtime.Connect(startCtx, instanceID, opts)
	if err != nil {
		r.log.Error("executor", "msg", "instance.Connect failed", "instance", instanceID, "err", err)
		return setupResult{}, fmt.Errorf("start instance: %w", err)
	}
	r.log.Info("executor", "msg", "started", "br", primaryBranch, "dur", time.Since(tContainer), "instance", instanceID, "fqdn", conn.TailscaleFQDN)
	return setupResult{
		InstanceID:       instanceID,
		AgentTarget:      conn.AgentTarget,
		TailscaleFQDN:    conn.TailscaleFQDN,
		TailscaleAuthURL: conn.TailscaleAuthURL,
	}, nil
}

// logRelayDiag reads the relay daemon's relay.log from the instance and logs
// its tail. Called when GracefulStop times out to capture relay-side diagnostics.
func (r *RepoExecutor) logRelayDiag(ctx context.Context, tlog *slog.Logger, target runtime.ConnectionTarget) {
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

func (r *RepoExecutor) sessionRunner() *SessionRunner {
	return &SessionRunner{
		Backends:  r.Backends,
		Workspace: &r.RepoWorkspace,
		Logs:      r.logStore(),
	}
}

func (r *RepoExecutor) logStore() *LogStore {
	return &LogStore{LogDir: r.LogDir, EventReplayFactory: r.EventReplayFactory}
}
