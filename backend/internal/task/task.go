// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
	MaxTurns  int
	Branch    string
	Container string
	State     State
	StartedAt time.Time

	mu       sync.Mutex
	msgs     []agent.Message
	subs     []chan agent.Message // active SSE subscribers
	session  *agent.Session
	msgCh    chan agent.Message // message dispatch channel; closed by Finish
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

// Subscribe returns a channel that receives replayed history followed by live
// messages. The returned function unsubscribes and must be called exactly once.
func (t *Task) Subscribe(ctx context.Context) (msgCh <-chan agent.Message, unsubFn func()) {
	c := make(chan agent.Message, 256)

	t.mu.Lock()
	// Replay history.
	for _, m := range t.msgs {
		c <- m
	}
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

	return c, unsub
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
	MaxTurns   int
	LogDir     string // If set, raw JSONL session logs are written here.

	branchMu sync.Mutex // Serializes operations that need a specific branch checked out (md commands).
	pushMu   sync.Mutex // Serializes git push to origin.
}

// Run executes the full task lifecycle (single-shot). It is meant to be
// called in a goroutine. The result is returned; the task is self-contained.
func (r *Runner) Run(ctx context.Context, t *Task) Result {
	if err := r.Start(ctx, t); err != nil {
		return Result{Task: t.Prompt, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: err}
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
	t.Branch = branchName(t.Prompt)

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
	logW, closeLog := r.openLog(t.Prompt)

	session, err := agent.Start(ctx, name, maxTurns, msgCh, logW)
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
		return Result{Task: t.Prompt, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: ctx.Err()}
	}

	t.mu.Lock()
	session := t.session
	t.session = nil
	msgCh := t.msgCh
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
	if closeLog != nil {
		closeLog()
	}

	// If ended, skip pull/push — just kill the container.
	if t.IsEnded() {
		t.State = StateEnded
		slog.Info("task ended, killing container", "container", name)
		if err := r.KillContainer(ctx, t.Branch); err != nil {
			slog.Warn("failed to kill container", "container", name, "err", err)
		}
		return Result{Task: t.Prompt, Branch: t.Branch, Container: name, State: StateEnded}
	}

	if session == nil {
		t.State = StateFailed
		return Result{Task: t.Prompt, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: errors.New("no active session")}
	}
	if waitErr != nil {
		t.State = StateFailed
		return Result{Task: t.Prompt, Branch: t.Branch, Container: name, State: StateFailed, Err: waitErr}
	}

	// 3. Diff + pull (requires task branch checked out).
	t.State = StatePulling
	diffStat, pullErr := r.pullChanges(ctx, t)

	if pullErr != nil {
		t.State = StateFailed
		return Result{
			Task: t.Prompt, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: pullErr,
		}
	}

	// 4. Push to origin (serialized; does not need checkout).
	t.State = StatePushing
	slog.Info("pushing to origin", "branch", t.Branch)
	r.pushMu.Lock()
	pushErr := gitutil.Push(ctx, t.Branch)
	r.pushMu.Unlock()
	if pushErr != nil {
		t.State = StateFailed
		return Result{
			Task: t.Prompt, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: pushErr,
		}
	}

	// 5. Kill container (requires task branch checked out).
	t.State = StateDone
	slog.Info("task done, killing container", "container", name)
	if err := r.KillContainer(ctx, t.Branch); err != nil {
		slog.Warn("failed to kill container", "container", name, "err", err)
	}

	return Result{
		Task:        t.Prompt,
		Branch:      t.Branch,
		Container:   name,
		State:       StateDone,
		DiffStat:    diffStat,
		CostUSD:     result.TotalCostUSD,
		DurationMs:  result.DurationMs,
		NumTurns:    result.NumTurns,
		AgentResult: result.Result,
	}
}

// setup creates the branch and starts the container. Must be called under
// branchMu.
func (r *Runner) setup(ctx context.Context, t *Task) (string, error) {
	slog.Info("creating branch", "branch", t.Branch)
	if err := gitutil.CreateBranch(ctx, t.Branch); err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	slog.Info("starting container", "branch", t.Branch)
	name, err := container.Start(ctx, t.Branch)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}

	// Switch back to the base branch so the next task can create its branch.
	if err := gitutil.CheckoutBranch(ctx, r.BaseBranch); err != nil {
		return "", fmt.Errorf("checkout base: %w", err)
	}
	return name, nil
}

// pullChanges checks out the task branch, runs md diff + md pull, then
// switches back. Returns the diff stat and the first error encountered.
func (r *Runner) pullChanges(ctx context.Context, t *Task) (diffStat string, err error) {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()

	if err := gitutil.CheckoutBranch(ctx, t.Branch); err != nil {
		return "", fmt.Errorf("checkout for pull: %w", err)
	}
	defer func() {
		if e := gitutil.CheckoutBranch(ctx, r.BaseBranch); e != nil {
			err = errors.Join(err, fmt.Errorf("checkout base after pull: %w", e))
		}
	}()

	diffStat, _ = container.Diff(ctx, "--stat")

	slog.Info("pulling changes", "container", t.Container)
	if err := container.Pull(ctx); err != nil {
		return diffStat, err
	}
	return diffStat, nil
}

// KillContainer checks out the branch, kills the md container, then switches
// back.
func (r *Runner) KillContainer(ctx context.Context, branch string) (err error) {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()

	if err := gitutil.CheckoutBranch(ctx, branch); err != nil {
		return fmt.Errorf("checkout for kill: %w", err)
	}
	defer func() {
		if e := gitutil.CheckoutBranch(ctx, r.BaseBranch); e != nil {
			err = errors.Join(err, fmt.Errorf("checkout base after kill: %w", e))
		}
	}()

	return container.Kill(ctx)
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// branchName generates a short, Docker-safe branch name from a prompt.
// Docker container names only allow [a-zA-Z0-9_.-], so no slashes.
func branchName(prompt string) string {
	return "wmao/" + slugify(prompt)
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 20 {
		s = s[:20]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// openLog creates a JSONL log file in LogDir. Returns a nil writer and a no-op
// closer if LogDir is empty or the file cannot be created.
func (r *Runner) openLog(prompt string) (w io.Writer, closeFn func()) {
	if r.LogDir == "" {
		return nil, func() {}
	}
	if err := os.MkdirAll(r.LogDir, 0o750); err != nil {
		slog.Warn("failed to create log dir", "dir", r.LogDir, "err", err)
		return nil, func() {}
	}
	name := time.Now().Format("20060102T150405") + "-" + slugify(prompt) + ".jsonl"
	f, err := os.Create(filepath.Join(r.LogDir, name)) //nolint:gosec // name is derived from slugify, not arbitrary user input.
	if err != nil {
		slog.Warn("failed to create log file", "err", err)
		return nil, func() {}
	}
	return f, func() { _ = f.Close() }
}
