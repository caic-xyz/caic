// SessionRunner owns agent session lifecycle, message dispatch, and state-change notification for tasks.

package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/trace"
	"time"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// SessionRunner manages agent sessions and dispatches backend messages into tasks.
type SessionRunner struct {
	// Immutable.
	Backends         map[harness.Name]agent.Backend
	Logs             LogStore
	Checkout         *repo.Checkout
	NotifyTaskChange func()
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
func (r *SessionRunner) Reconnect(ctx context.Context, t *Task, skipSideEffects bool) (*SessionHandle, error) {
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
	log, err := r.Logs.Reopen(t)
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
	if prevState != StateWaiting && prevState != StateAsking {
		t.SetState(StateRunning)
	}
	target := t.RuntimeConnectionTarget()
	session, err := r.Backends[t.Harness].AttachRelay(ctx, &agent.Options{
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
		t.SetState(StateWaiting)
		r.Checkout.Log.Error("attach relay failed", "br", primaryBranch, "instance", instanceID, "err", err)
		return nil, fmt.Errorf("reconnect: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)
	return h, nil
}

// EnsureSession waits briefly for h to confirm it's alive. If the session
// exits within 10 seconds (agent had already finished), it detaches and
// starts a fresh idle relay so the task can accept new prompts.
func (r *SessionRunner) EnsureSession(ctx context.Context, t *Task, h *SessionHandle, tlog *slog.Logger) (*SessionHandle, error) {
	select {
	case <-h.Done():
		// Session exited immediately (agent was already done).
		t.DetachSession()
		err := h.Drain()
		_ = h.Log.Close()
		tlog.Info("attached session exited, starting idle relay", "err", err)
		if s := t.GetState(); s == StateStopping || s == StateStopped || s == StatePurged {
			return nil, fmt.Errorf("task is %s", s)
		}
		t.SetState(StateWaiting)
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
func (r *SessionRunner) StartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	if t.RuntimeInstanceID() == "" {
		return nil, errors.New("no instance")
	}
	log, err := r.Logs.Open(t)
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
func (r *SessionRunner) RestartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	return r.replaceSession(ctx, t, prompt, replaceSessionRestart)
}

// ClearContextSession closes the current agent session and starts a fresh one
// in the same instance without a prompt. The task transitions to StateWaiting
// so the user can send a new message when ready.
func (r *SessionRunner) ClearContextSession(ctx context.Context, t *Task) (*SessionHandle, error) {
	return r.replaceSession(ctx, t, agent.Prompt{}, replaceSessionClearContext)
}

func (r *SessionRunner) startSessionWithLog(ctx context.Context, t *Task, prompt agent.Prompt, log agent.LogSink) (*SessionHandle, error) {
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
	tlog := r.Checkout.Log.With("br", primaryBranch, "instance", instanceID)

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)
	tlog.Info("starting session", "hns", t.Harness)
	target := t.RuntimeConnectionTarget()
	session, err := r.Backends[t.Harness].Start(ctx, &agent.Options{
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
		t.SetState(StateRunning)
	}
	return h, nil
}

func (r *SessionRunner) replaceSession(ctx context.Context, t *Task, prompt agent.Prompt, mode replaceSessionMode) (*SessionHandle, error) {
	traceName := "task.clear-context:"
	if mode == replaceSessionRestart {
		traceName = "task.restart:"
	}
	ctx, task := trace.NewTask(ctx, traceName+t.ID.String())
	defer task.End()

	state := t.GetState()
	if state != StateWaiting && state != StateAsking && state != StateHasPlan && state != StateStarting {
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
			err := r.Logs.WriteContextCleared(oldH.Log)
			err = errors.Join(err, oldH.Log.Close())
			if err != nil {
				t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
				return nil, fmt.Errorf("write context cleared: %w", err)
			}
		}
	}

	// Clear in-memory messages.
	t.ClearMessages(ctx)

	// Open new log segment.
	log, err := r.Logs.Open(t)
	if err != nil {
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("open log: %w", err)
	}

	// Start new session.
	t.SetState(StateStarting)
	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)

	var branch string
	if p := t.Primary(); p != nil {
		branch = p.Branch
	}
	instanceID := t.RuntimeInstanceID()
	tlog := r.Checkout.Log
	if tlog == nil {
		tlog = slog.Default()
	}
	tlog = tlog.With("br", branch, "instance", instanceID)
	tlog.Info(mode.logMessage(), "hns", t.Harness)
	target := t.RuntimeConnectionTarget()
	opts := &agent.Options{
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
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("unknown harness %q", t.Harness)
	}
	session, err := backend.Start(ctx, opts)
	if err != nil {
		_ = log.Close()
		close(msgCh)
		<-dispatchDone
		t.SetStateUnless(StateFailed, StatePurging, StatePurged, StateStopping, StateStopped)
		return nil, fmt.Errorf("start session: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, Log: log}
	t.AttachSession(h)
	if mode == replaceSessionRestart {
		t.addMessage(ctx, syntheticUserInput(prompt), false)
		t.SetState(StateRunning)
		tlog.Info("session restarted")
	} else {
		t.SetState(StateWaiting)
		tlog.Info("context cleared")
	}
	return h, nil
}

// startMessageDispatch starts a goroutine that reads from msgCh, dispatches to
// t.addMessage, and reports task state transitions.
func (r *SessionRunner) startMessageDispatch(ctx context.Context, t *Task, skipSideEffects bool) (msgCh chan agent.ParsedMessage, dispatchDone <-chan struct{}) {
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
					emitToolDiff = !skipSideEffects && r.Checkout.Runtimes != nil && r.Checkout.Dir != ""
				}
			case *agent.ResultMessage:
				if !skipSideEffects && r.Checkout.Runtimes != nil && r.Checkout.Dir != "" {
					ds, _ := r.Checkout.DiffStat(ctx, instanceID, allRepos, repo.DiffFetchBestEffort, "")
					msg.DiffStat = ds
				}
			}
			stateChanged := t.addParsedMessage(ctx, parsed, skipSideEffects)
			if stateChanged {
				r.NotifyTaskChange()
			}
			if emitToolDiff {
				r.emitDiffStatBranch(ctx, t, instanceID, allRepos)
			}
		}
	}()
	return msgCh, dispatchDone
}

// mutatingTools lists tool names whose execution may change files in the
// instance, warranting a diff stat refresh after their result arrives.
var mutatingTools = map[string]struct{}{
	"Bash":         {},
	"Edit":         {},
	"Write":        {},
	"NotebookEdit": {},
}

// emitDiffStatBranch emits a DiffStatMessage from the current instance diff
// without fetching from the instance. This keeps live UI diff stats fresh during
// a running turn without triggering md fetch side effects.
func (r *SessionRunner) emitDiffStatBranch(ctx context.Context, t *Task, id runtime.ID, repos []runtime.Repo) {
	ds, _ := r.Checkout.DiffStat(ctx, id, repos, repo.DiffWithoutFetch, "")
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
func (r *SessionRunner) runtimeDir(t *Task) string {
	if p := t.Primary(); p != nil && p.ContainerPath != "" {
		return md.ResolveContainerPath(p.ContainerPath)
	}
	if r.Checkout.Dir == "" {
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
