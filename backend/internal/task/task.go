// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/container"
	"github.com/maruel/wmao/backend/internal/gitutil"
)

// State represents the lifecycle state of a task.
type State int

// Task lifecycle states.
const (
	StatePending  State = iota
	StateStarting       // Creating branch + container.
	StateRunning        // Agent is executing.
	StateWaiting        // Agent completed a turn, awaiting user input or finish.
	StatePulling        // Pulling changes from container.
	StatePushing        // Pushing to origin.
	StateDone           // Successfully completed.
	StateFailed         // Failed at some stage.
	StateEnded          // Force-killed, skipping pull/push.
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateWaiting:
		return "waiting"
	case StatePulling:
		return "pulling"
	case StatePushing:
		return "pushing"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	case StateEnded:
		return "ended"
	default:
		return "unknown"
	}
}

// Result holds the outcome of a completed task.
type Result struct {
	Task        string
	Repo        string
	Branch      string
	Container   string
	State       State
	DiffStat    string
	CostUSD     float64
	DurationMs  int64
	NumTurns    int
	AgentResult string
	Err         error
}

// Task represents a single unit of work.
type Task struct {
	Prompt    string
	Repo      string // Relative repo path (for display/API).
	MaxTurns  int
	Branch    string
	Container string
	State     State
	SessionID string // Claude Code session ID, captured from SystemInitMessage.
	StartedAt time.Time

	mu       sync.Mutex
	msgs     []agent.Message
	subs     []chan agent.Message // active SSE subscribers
	session  *agent.Session
	msgCh    chan agent.Message // message dispatch channel; closed by Finish
	logW     io.Writer          // current log file writer (for writing trailer)
	closeLog func()             // closes the session log file
	doneCh   chan struct{}      // closed when user calls Finish or End
	doneOnce sync.Once
	ended    bool // true when End() was called (skip pull/push)
}

// Messages returns a copy of all received agent messages.
func (t *Task) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agent.Message(nil), t.msgs...)
}

func (t *Task) addMessage(m agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
	// Capture session ID from the init message.
	if init, ok := m.(*agent.SystemInitMessage); ok && init.SessionID != "" {
		t.SessionID = init.SessionID
	}
	// Transition to waiting when a result arrives while running.
	if _, ok := m.(*agent.ResultMessage); ok && t.State == StateRunning {
		t.State = StateWaiting
	}
	// Fan out to subscribers (non-blocking).
	for i := 0; i < len(t.subs); i++ {
		select {
		case t.subs[i] <- m:
		default:
			// Slow subscriber — drop and remove.
			close(t.subs[i])
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			i--
		}
	}
}

// Subscribe returns a snapshot of the message history and a channel that
// receives only live messages arriving after the snapshot. The caller must
// write the history to the client first, then range over the channel.
// The returned function unsubscribes and must be called exactly once.
func (t *Task) Subscribe(ctx context.Context) (history []agent.Message, live <-chan agent.Message, unsubFn func()) {
	c := make(chan agent.Message, 256)

	t.mu.Lock()
	// Snapshot history under lock — no channel writes, so no deadlock risk
	// regardless of history size.
	history = append([]agent.Message(nil), t.msgs...)
	t.subs = append(t.subs, c)
	t.mu.Unlock()

	unsub := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, sub := range t.subs {
			if sub == c {
				t.subs = append(t.subs[:i], t.subs[i+1:]...)
				break
			}
		}
	}

	// Close channel when context is done.
	go func() {
		<-ctx.Done()
		unsub()
		close(c)
	}()

	return history, c, unsub
}

// SendInput sends a user message to the running agent. Returns an error if
// no session is active.
func (t *Task) SendInput(prompt string) error {
	t.mu.Lock()
	s := t.session
	if s != nil {
		t.State = StateRunning
	}
	t.mu.Unlock()
	if s == nil {
		return errors.New("no active session")
	}
	return s.Send(prompt)
}

// InitDoneCh initializes the done channel. Called by Runner.Start; exposed
// for tests that construct a Task directly.
func (t *Task) InitDoneCh() {
	t.doneCh = make(chan struct{})
}

// Finish signals that the user is done interacting with this task. The
// session will be closed and the pull/push/kill cycle will proceed.
func (t *Task) Finish() {
	t.doneOnce.Do(func() {
		close(t.doneCh)
	})
}

// End signals a force-kill: the session will be closed and the container
// killed without pulling or pushing changes.
func (t *Task) End() {
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()
	t.doneOnce.Do(func() {
		close(t.doneCh)
	})
}

// IsEnded reports whether End was called.
func (t *Task) IsEnded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ended
}

// Done returns a channel that is closed when the user calls Finish or End.
func (t *Task) Done() <-chan struct{} {
	return t.doneCh
}

// Runner manages the serialization of setup and push operations.
type Runner struct {
	BaseBranch string
	Dir        string // Absolute path to the git repository.
	MaxTurns   int
	LogDir     string // If set, raw JSONL session logs are written here.

	branchMu sync.Mutex // Serializes operations that need a specific branch checked out (md commands).
	nextID   int        // Next branch sequence number (protected by branchMu).
	pushMu   sync.Mutex // Serializes git push to origin.
}

// Init sets nextID past any existing wmao/w* branches so that restarts don't
// waste attempts on branches that already exist.
func (r *Runner) Init(ctx context.Context) error {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	highest, err := gitutil.MaxBranchSeqNum(ctx, r.Dir)
	if err != nil {
		return err
	}
	if highest >= r.nextID {
		r.nextID = highest + 1
	}
	return nil
}

// Reconnect starts a new Claude session in an existing container, resuming
// the previous conversation if a SessionID was captured. The caller must use
// SendInput to provide the next prompt after reconnecting.
func (r *Runner) Reconnect(ctx context.Context, t *Task) error {
	t.mu.Lock()
	if t.session != nil {
		t.mu.Unlock()
		return errors.New("session already active")
	}
	if t.Container == "" {
		t.mu.Unlock()
		return errors.New("no container to reconnect to")
	}
	t.State = StateRunning
	t.mu.Unlock()

	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()

	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	logW, closeLog := r.openLog(t)

	session, err := agent.Start(ctx, t.Container, maxTurns, msgCh, logW, t.SessionID)
	if err != nil {
		closeLog()
		close(msgCh)
		t.mu.Lock()
		t.State = StateWaiting
		t.mu.Unlock()
		return fmt.Errorf("reconnect: %w", err)
	}

	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.closeLog = closeLog
	t.mu.Unlock()
	return nil
}

// Takeover kills any running claude process in the container and starts a
// fresh session, optionally resuming the previous conversation. Use this when
// a container may still have a stale claude process (e.g. after wmao restart).
func (r *Runner) Takeover(ctx context.Context, t *Task) error {
	t.mu.Lock()
	if t.session != nil {
		t.mu.Unlock()
		return errors.New("session already active")
	}
	if t.Container == "" {
		t.mu.Unlock()
		return errors.New("no container to take over")
	}
	t.mu.Unlock()

	// Discover session ID from the running process, if any.
	sid, running, err := agent.FindRunningSession(ctx, t.Container)
	if err != nil {
		return fmt.Errorf("find running session: %w", err)
	}
	if running {
		// Preserve discovered session ID when we had none.
		if sid != "" && t.SessionID == "" {
			t.SessionID = sid
		}
		if err := agent.KillRunning(ctx, t.Container); err != nil {
			return fmt.Errorf("kill running: %w", err)
		}
	}

	// From here, same as Reconnect.
	t.mu.Lock()
	t.State = StateRunning
	t.mu.Unlock()

	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()

	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	logW, closeLog := r.openLog(t)

	session, err := agent.Start(ctx, t.Container, maxTurns, msgCh, logW, t.SessionID)
	if err != nil {
		closeLog()
		close(msgCh)
		t.mu.Lock()
		t.State = StateWaiting
		t.mu.Unlock()
		return fmt.Errorf("takeover: %w", err)
	}

	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.closeLog = closeLog
	t.mu.Unlock()
	return nil
}

// Run executes the full task lifecycle (single-shot). It is meant to be
// called in a goroutine. The result is returned; the task is self-contained.
func (r *Runner) Run(ctx context.Context, t *Task) Result {
	if err := r.Start(ctx, t); err != nil {
		return Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: err}
	}
	// Single-shot: finish immediately.
	t.Finish()
	return r.Finish(ctx, t)
}

// Start performs branch/container setup, starts the agent session, and sends
// the initial prompt. The session is left open for follow-up messages via
// SendInput. Call Finish (or t.Finish + r.Finish) to close the session and
// proceed to pull/push/kill.
func (r *Runner) Start(ctx context.Context, t *Task) error {
	t.StartedAt = time.Now()
	t.State = StateStarting
	t.InitDoneCh()

	// 1. Create branch + start container (serialized).
	r.branchMu.Lock()
	name, err := r.setup(ctx, t)
	r.branchMu.Unlock()
	if err != nil {
		t.State = StateFailed
		return err
	}
	t.Container = name

	// 2. Start the agent session.
	t.State = StateRunning
	slog.Info("running agent", "container", name, "task", t.Prompt)
	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()
	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	logW, closeLog := r.openLog(t)

	session, err := agent.Start(ctx, name, maxTurns, msgCh, logW, "")
	if err != nil {
		closeLog()
		close(msgCh)
		t.State = StateFailed
		return err
	}

	// Store session so SendInput can reach it.
	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.closeLog = closeLog
	t.mu.Unlock()

	if err := session.Send(t.Prompt); err != nil {
		closeLog()
		close(msgCh)
		t.State = StateFailed
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// Finish closes the agent session, waits for the result, and performs
// pull/push/kill. It blocks until t.Done() is signaled, then proceeds.
// If End() was called, it skips pull/push and only kills the container.
func (r *Runner) Finish(ctx context.Context, t *Task) Result {
	// Wait for user to signal finish or end.
	select {
	case <-t.Done():
	case <-ctx.Done():
		t.State = StateFailed
		return Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: ctx.Err()}
	}

	t.mu.Lock()
	session := t.session
	t.session = nil
	msgCh := t.msgCh
	logW := t.logW
	t.logW = nil
	closeLog := t.closeLog
	t.mu.Unlock()

	name := t.Container

	// Close session if one exists.
	var result *agent.ResultMessage
	var waitErr error
	if session != nil {
		session.Close()
		result, waitErr = session.Wait()
	}
	if msgCh != nil {
		close(msgCh)
	}

	// finishLog writes the result trailer and closes the log file.
	finishLog := func(res *Result) {
		writeLogTrailer(logW, res)
		if closeLog != nil {
			closeLog()
		}
	}

	// If ended, skip pull/push — just kill the container.
	if t.IsEnded() {
		t.State = StateEnded
		slog.Info("task ended, killing container", "container", name)
		if err := r.KillContainer(ctx, t.Branch); err != nil {
			slog.Warn("failed to kill container", "container", name, "err", err)
		}
		res := Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: name, State: StateEnded}
		finishLog(&res)
		return res
	}

	// No session and no container means nothing to do.
	if session == nil && name == "" {
		t.State = StateFailed
		res := Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: errors.New("no active session")}
		finishLog(&res)
		return res
	}
	if session != nil && waitErr != nil {
		t.State = StateFailed
		res := Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: name, State: StateFailed, Err: waitErr}
		finishLog(&res)
		return res
	}

	// 3. Diff + pull (requires task branch checked out).
	t.State = StatePulling
	diffStat, pullErr := r.PullChanges(ctx, t.Branch)

	if pullErr != nil {
		t.State = StateFailed
		res := Result{
			Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: pullErr,
		}
		finishLog(&res)
		return res
	}

	// 4. Push to origin (serialized; does not need checkout).
	t.State = StatePushing
	slog.Info("pushing to origin", "branch", t.Branch)
	r.pushMu.Lock()
	pushErr := gitutil.Push(ctx, r.Dir, t.Branch)
	r.pushMu.Unlock()
	if pushErr != nil {
		t.State = StateFailed
		res := Result{
			Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: pushErr,
		}
		finishLog(&res)
		return res
	}

	// 5. Kill container (requires task branch checked out).
	t.State = StateDone
	slog.Info("task done, killing container", "container", name)
	if err := r.KillContainer(ctx, t.Branch); err != nil {
		slog.Warn("failed to kill container", "container", name, "err", err)
	}

	res := Result{
		Task:      t.Prompt,
		Repo:      t.Repo,
		Branch:    t.Branch,
		Container: name,
		State:     StateDone,
		DiffStat:  diffStat,
	}
	if result != nil {
		res.CostUSD = result.TotalCostUSD
		res.DurationMs = result.DurationMs
		res.NumTurns = result.NumTurns
		res.AgentResult = result.Result
	}
	finishLog(&res)
	return res
}

// setup creates the branch and starts the container. Must be called under
// branchMu.
func (r *Runner) setup(ctx context.Context, t *Task) (string, error) {
	// Assign a sequential branch name, skipping existing ones.
	var err error
	for range 100 {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		t.Branch = fmt.Sprintf("wmao/w%d", r.nextID)
		r.nextID++
		slog.Info("creating branch", "branch", t.Branch)
		err = gitutil.CreateBranch(ctx, r.Dir, t.Branch, "origin/"+r.BaseBranch)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	slog.Info("starting container", "branch", t.Branch)
	name, err := container.Start(ctx, r.Dir)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}

	// Switch back to the base branch so the next task can create its branch.
	if err := gitutil.CheckoutBranch(ctx, r.Dir, r.BaseBranch); err != nil {
		return "", fmt.Errorf("checkout base: %w", err)
	}
	return name, nil
}

// PullChanges checks out the branch, runs md diff + md pull, then switches
// back. Returns the diff stat and the first error encountered.
func (r *Runner) PullChanges(ctx context.Context, branch string) (diffStat string, err error) {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()

	if err := gitutil.CheckoutBranch(ctx, r.Dir, branch); err != nil {
		return "", fmt.Errorf("checkout for pull: %w", err)
	}
	defer func() {
		if e := gitutil.CheckoutBranch(ctx, r.Dir, r.BaseBranch); e != nil {
			err = errors.Join(err, fmt.Errorf("checkout base after pull: %w", e))
		}
	}()

	diffStat, _ = container.Diff(ctx, r.Dir, "--stat")

	slog.Info("pulling changes", "branch", branch)
	if err := container.Pull(ctx, r.Dir); err != nil {
		return diffStat, err
	}
	return diffStat, nil
}

// PushChanges checks out the branch, runs md push, then switches back.
func (r *Runner) PushChanges(ctx context.Context, branch string) (err error) {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()

	if err := gitutil.CheckoutBranch(ctx, r.Dir, branch); err != nil {
		return fmt.Errorf("checkout for push: %w", err)
	}
	defer func() {
		if e := gitutil.CheckoutBranch(ctx, r.Dir, r.BaseBranch); e != nil {
			err = errors.Join(err, fmt.Errorf("checkout base after push: %w", e))
		}
	}()

	slog.Info("pushing changes to container", "branch", branch)
	return container.Push(ctx, r.Dir)
}

// KillContainer checks out the branch, kills the md container, then switches
// back.
func (r *Runner) KillContainer(ctx context.Context, branch string) (err error) {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()

	if err := gitutil.CheckoutBranch(ctx, r.Dir, branch); err != nil {
		return fmt.Errorf("checkout for kill: %w", err)
	}
	defer func() {
		if e := gitutil.CheckoutBranch(ctx, r.Dir, r.BaseBranch); e != nil {
			err = errors.Join(err, fmt.Errorf("checkout base after kill: %w", e))
		}
	}()

	return container.Kill(ctx, r.Dir)
}

// openLog creates a JSONL log file in LogDir and writes a metadata header as
// the first line. Returns a nil writer and a no-op closer if LogDir is empty
// or the file cannot be created.
func (r *Runner) openLog(t *Task) (w io.Writer, closeFn func()) {
	if r.LogDir == "" {
		return nil, func() {}
	}
	if err := os.MkdirAll(r.LogDir, 0o750); err != nil {
		slog.Warn("failed to create log dir", "dir", r.LogDir, "err", err)
		return nil, func() {}
	}
	safe := strings.ReplaceAll(t.Branch, "/", "-")
	name := time.Now().Format("20060102T150405") + "-" + safe + ".jsonl"
	f, err := os.Create(filepath.Join(r.LogDir, name)) //nolint:gosec // name is derived from branch name, not arbitrary user input.
	if err != nil {
		slog.Warn("failed to create log file", "err", err)
		return nil, func() {}
	}
	// Write metadata header as the first line.
	meta := agent.MetaMessage{
		MessageType: "wmao_meta",
		Prompt:      t.Prompt,
		Repo:        t.Repo,
		Branch:      t.Branch,
		StartedAt:   t.StartedAt,
	}
	if data, err := json.Marshal(meta); err == nil {
		_, _ = f.Write(append(data, '\n'))
	}
	return f, func() { _ = f.Close() }
}

// writeLogTrailer appends a MetaResultMessage to the log file. Must be called
// before closeLog.
func writeLogTrailer(w io.Writer, res *Result) {
	if w == nil {
		return
	}
	mr := agent.MetaResultMessage{
		MessageType: "wmao_result",
		State:       res.State.String(),
		CostUSD:     res.CostUSD,
		DurationMs:  res.DurationMs,
		NumTurns:    res.NumTurns,
		DiffStat:    res.DiffStat,
		AgentResult: res.AgentResult,
	}
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	if data, err := json.Marshal(mr); err == nil {
		_, _ = w.Write(append(data, '\n'))
	}
}
