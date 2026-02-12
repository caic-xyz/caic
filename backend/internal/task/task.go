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

	"github.com/maruel/ksid"
	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/container"
	"github.com/maruel/wmao/backend/internal/gitutil"
)

// State represents the lifecycle state of a task.
type State int

// Task lifecycle states.
const (
	StatePending      State = iota
	StateBranching          // Creating git branch.
	StateProvisioning       // Starting docker container.
	StateStarting           // Launching agent session.
	StateRunning            // Agent is executing.
	StateWaiting            // Agent completed a turn, awaiting user input or terminate.
	StateAsking             // Agent asked a question (AskUserQuestion), needs answer.
	StatePulling            // Pulling changes from container.
	StatePushing            // Pushing to origin.
	StateTerminating        // User requested termination; cleanup in progress.
	StateFailed             // Failed at some stage.
	StateTerminated         // Terminated by user.
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateBranching:
		return "branching"
	case StateProvisioning:
		return "provisioning"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateWaiting:
		return "waiting"
	case StateAsking:
		return "asking"
	case StatePulling:
		return "pulling"
	case StatePushing:
		return "pushing"
	case StateTerminating:
		return "terminating"
	case StateFailed:
		return "failed"
	case StateTerminated:
		return "terminated"
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
	ID                ksid.ID
	Prompt            string
	Repo              string // Relative repo path (for display/API).
	MaxTurns          int
	Branch            string
	Container         string
	State             State
	StateUpdatedAt    time.Time // UTC timestamp of the last state transition.
	SessionID         string    // Claude Code session ID, captured from SystemInitMessage.
	Model             string    // Model name, captured from SystemInitMessage.
	ClaudeCodeVersion string    // Claude Code version, captured from SystemInitMessage.
	StartedAt         time.Time
	RelayOffset       int64 // Bytes received from relay output.jsonl, for reconnect.

	mu       sync.Mutex
	msgs     []agent.Message
	subs     []chan agent.Message // active SSE subscribers
	session  *agent.Session
	msgCh    chan agent.Message // message dispatch channel; closed by Terminate
	logW     io.Writer          // current log file writer (for writing trailer)
	closeLog func()             // closes the session log file
	doneCh   chan struct{}      // closed when user calls Terminate
	doneOnce sync.Once

	// Live stats accumulated from ResultMessages during execution.
	liveCostUSD    float64
	liveNumTurns   int
	liveDurationMs int64
}

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s State) {
	t.State = s
	t.StateUpdatedAt = time.Now().UTC()
}

// LiveStats returns the latest cost, turn count, and duration accumulated
// from ResultMessages received during execution.
func (t *Task) LiveStats() (costUSD float64, numTurns int, durationMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveCostUSD, t.liveNumTurns, t.liveDurationMs
}

// Messages returns a copy of all received agent messages.
func (t *Task) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agent.Message(nil), t.msgs...)
}

// RestoreMessages sets the initial message history from previously saved logs.
// It also extracts metadata from the last SystemInitMessage, if any, and
// infers the task state from the trailing messages: a trailing ResultMessage
// means the agent completed its turn (StateWaiting or StateAsking).
func (t *Task) RestoreMessages(msgs []agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = msgs
	for i := len(msgs) - 1; i >= 0; i-- {
		if init, ok := msgs[i].(*agent.SystemInitMessage); ok && init.SessionID != "" {
			t.SessionID = init.SessionID
			t.Model = init.Model
			t.ClaudeCodeVersion = init.Version
			break
		}
	}
	// Restore live stats from the last ResultMessage.
	for i := len(msgs) - 1; i >= 0; i-- {
		if rm, ok := msgs[i].(*agent.ResultMessage); ok {
			t.liveCostUSD = rm.TotalCostUSD
			t.liveNumTurns = rm.NumTurns
			t.liveDurationMs = rm.DurationMs
			break
		}
	}
	// Infer state: if the last message is a ResultMessage, the agent finished
	// its turn and is waiting for user input (or asking a question). Only
	// override non-terminal states — terminated/failed tasks loaded from logs
	// must keep their recorded state.
	if len(msgs) > 0 && t.State != StateTerminated && t.State != StateFailed && t.State != StateTerminating {
		if _, ok := msgs[len(msgs)-1].(*agent.ResultMessage); ok {
			if lastAssistantHasAsk(msgs) {
				t.setState(StateAsking)
			} else {
				t.setState(StateWaiting)
			}
		}
	}
}

func (t *Task) addMessage(m agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
	// Capture metadata from the init message.
	if init, ok := m.(*agent.SystemInitMessage); ok && init.SessionID != "" {
		t.SessionID = init.SessionID
		t.Model = init.Model
		t.ClaudeCodeVersion = init.Version
	}
	// Transition to waiting/asking when a result arrives while running.
	if rm, ok := m.(*agent.ResultMessage); ok {
		t.liveCostUSD = rm.TotalCostUSD
		t.liveNumTurns = rm.NumTurns
		t.liveDurationMs = rm.DurationMs
		if t.State == StateRunning {
			if lastAssistantHasAsk(t.msgs) {
				t.setState(StateAsking)
			} else {
				t.setState(StateWaiting)
			}
		}
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

// syntheticUserInput creates a UserMessage representing user-provided text
// input. It is injected into the message stream so that the JSONL log and SSE
// events contain an explicit record of every user message.
func syntheticUserInput(text string) *agent.UserMessage {
	raw, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: text})
	return &agent.UserMessage{
		MessageType: "user",
		Message:     raw,
	}
}

// lastAssistantHasAsk reports whether the last AssistantMessage in msgs
// contains an AskUserQuestion tool_use block.
func lastAssistantHasAsk(msgs []agent.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		am, ok := msgs[i].(*agent.AssistantMessage)
		if !ok {
			continue
		}
		for _, b := range am.Message.Content {
			if b.Type == "tool_use" && b.Name == "AskUserQuestion" {
				return true
			}
		}
		return false
	}
	return false
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
		t.setState(StateRunning)
	}
	t.mu.Unlock()
	if s == nil {
		return errors.New("no active session")
	}
	t.addMessage(syntheticUserInput(prompt))
	return s.Send(prompt)
}

// InitDoneCh initializes the done channel. Called by Runner.Start; exposed
// for tests that construct a Task directly.
func (t *Task) InitDoneCh() {
	t.doneCh = make(chan struct{})
}

// Terminate signals that the user is done interacting with this task. The
// session will be closed and the container killed.
func (t *Task) Terminate() {
	t.doneOnce.Do(func() {
		t.setState(StateTerminating)
		close(t.doneCh)
	})
}

// Done returns a channel that is closed when the user calls Terminate.
func (t *Task) Done() <-chan struct{} {
	return t.doneCh
}

// Runner manages the serialization of setup and push operations.
type Runner struct {
	BaseBranch string
	Dir        string // Absolute path to the git repository.
	MaxTurns   int
	LogDir     string // If set, raw JSONL session logs are written here.

	// Container provides md container lifecycle operations. Must be set before
	// calling Start (use container.NewLib to create one).
	Container container.Ops
	// AgentStartFn launches an agent session. Defaults to agent.Start.
	AgentStartFn func(ctx context.Context, container string, maxTurns int, msgCh chan<- agent.Message, logW io.Writer, resumeSessionID string) (*agent.Session, error)

	initOnce sync.Once
	branchMu sync.Mutex // Serializes operations that need a specific branch checked out (md commands).
	nextID   int        // Next branch sequence number (protected by branchMu).
}

func (r *Runner) initDefaults() {
	r.initOnce.Do(func() {
		if r.AgentStartFn == nil {
			r.AgentStartFn = agent.StartWithRelay
		}
	})
}

// Init sets nextID past any existing wmao/w* branches so that restarts don't
// waste attempts on branches that already exist.
func (r *Runner) Init(ctx context.Context) error {
	r.initDefaults()
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

// Reconnect reattaches to a running relay, or starts a new Claude session
// resuming the previous conversation if no relay is available. The caller must
// use SendInput to provide the next prompt after reconnecting.
func (r *Runner) Reconnect(ctx context.Context, t *Task) error {
	r.initDefaults()
	t.mu.Lock()
	if t.session != nil {
		t.mu.Unlock()
		return errors.New("session already active")
	}
	if t.Container == "" {
		t.mu.Unlock()
		return errors.New("no container to reconnect to")
	}
	// Remember the state inferred from restored messages so we don't
	// blindly override it to StateRunning for an idle relay.
	prevState := t.State
	t.mu.Unlock()

	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()

	logW, closeLog := r.openLog(t)

	// Prefer attaching to a live relay (claude process still running).
	relayAlive, err := agent.IsRelayRunning(ctx, t.Container)
	if err != nil {
		slog.Warn("relay check failed, falling back to --resume", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
	}

	var session *agent.Session
	if relayAlive {
		// Only transition to StateRunning if the restored messages indicate
		// the agent was still producing output (no trailing ResultMessage).
		// If the agent had already completed its turn, keep the inferred
		// StateWaiting/StateAsking so the UI shows the correct status.
		if prevState != StateWaiting && prevState != StateAsking {
			t.mu.Lock()
			t.setState(StateRunning)
			t.mu.Unlock()
		}
		session, err = agent.AttachRelay(ctx, t.Container, t.RelayOffset, msgCh, logW)
		if err != nil {
			slog.Warn("attach relay failed, falling back to --resume", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
			relayAlive = false
		}
	}
	if !relayAlive {
		// Starting a new session via --resume always re-engages the agent.
		t.mu.Lock()
		t.setState(StateRunning)
		t.mu.Unlock()
		maxTurns := t.MaxTurns
		if maxTurns == 0 {
			maxTurns = r.MaxTurns
		}
		session, err = r.AgentStartFn(ctx, t.Container, maxTurns, msgCh, logW, t.SessionID)
	}
	if err != nil {
		closeLog()
		close(msgCh)
		t.mu.Lock()
		t.setState(StateWaiting)
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

// Start performs branch/container setup, starts the agent session, and sends
// the initial prompt.
//
// The session is left open for follow-up messages via SendInput.
//
// Call Kill to close the session.
func (r *Runner) Start(ctx context.Context, t *Task) error {
	r.initDefaults()
	if r.Container == nil {
		return errors.New("runner has no container backend configured")
	}
	t.StartedAt = time.Now().UTC()
	t.setState(StateBranching)
	t.InitDoneCh()

	// 1. Create branch + start container (serialized).
	slog.Info("setting up task", "repo", t.Repo)
	r.branchMu.Lock()
	name, err := r.setup(ctx, t, []string{"wmao=" + t.ID.String()})
	r.branchMu.Unlock()
	if err != nil {
		t.setState(StateFailed)
		return err
	}
	t.Container = name
	slog.Info("container ready", "repo", t.Repo, "branch", t.Branch, "container", name)

	// 2. Start the agent session.
	t.setState(StateStarting)
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

	slog.Info("starting agent session", "repo", t.Repo, "branch", t.Branch, "container", name, "maxTurns", maxTurns)
	session, err := r.AgentStartFn(ctx, name, maxTurns, msgCh, logW, "")
	if err != nil {
		closeLog()
		close(msgCh)
		t.setState(StateFailed)
		slog.Warn("agent session failed to start", "repo", t.Repo, "branch", t.Branch, "container", name, "err", err)
		return err
	}

	// Store session so SendInput can reach it.
	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.closeLog = closeLog
	t.mu.Unlock()

	t.addMessage(syntheticUserInput(t.Prompt))
	if err := session.Send(t.Prompt); err != nil {
		closeLog()
		close(msgCh)
		t.setState(StateFailed)
		return fmt.Errorf("write prompt: %w", err)
	}
	t.setState(StateRunning)
	slog.Info("agent running", "repo", t.Repo, "branch", t.Branch, "container", name)
	return nil
}

// Kill terminates the agent session and kills the container. It blocks until
// t.Done() is signaled, then proceeds. Pull/push must be done separately.
func (r *Runner) Kill(ctx context.Context, t *Task) Result {
	// Wait for user to signal terminate.
	select {
	case <-t.Done():
	case <-ctx.Done():
		t.setState(StateFailed)
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

	// Terminate agent session if one exists.
	var result *agent.ResultMessage
	if session != nil {
		session.Close()
		result, _ = session.Wait()
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

	// Kill container.
	t.setState(StateTerminated)
	slog.Info("killing container", "repo", t.Repo, "branch", t.Branch, "container", name)
	if name != "" {
		if err := r.KillContainer(ctx, t.Branch); err != nil {
			slog.Warn("failed to kill container", "repo", t.Repo, "branch", t.Branch, "container", name, "err", err)
		}
	}

	res := Result{
		Task:      t.Prompt,
		Repo:      t.Repo,
		Branch:    t.Branch,
		Container: name,
		State:     StateTerminated,
	}
	if result != nil {
		res.CostUSD = result.TotalCostUSD
		res.DurationMs = result.DurationMs
		res.NumTurns = result.NumTurns
		res.AgentResult = result.Result
	}
	// Use accumulated live stats when they exceed the session result
	// (e.g. adopted container after restart where the session only
	// reflects the reconnected portion, not the full run).
	if liveCost, liveTurns, liveDur := t.LiveStats(); liveCost > res.CostUSD {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.DurationMs = liveDur
	}
	// waitErr is intentionally ignored: the user requested termination, so a
	// missing result message or non-zero exit is expected, not an error.
	finishLog(&res)
	return res
}

// setup creates the branch and starts the container. Must be called under
// branchMu.
func (r *Runner) setup(ctx context.Context, t *Task, labels []string) (string, error) {
	// Fetch so that origin/<BaseBranch> is up to date.
	if err := gitutil.Fetch(ctx, r.Dir); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	// Assign a sequential branch name, skipping existing ones.
	var err error
	for range 100 {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		t.Branch = fmt.Sprintf("wmao/w%d", r.nextID)
		r.nextID++
		slog.Info("creating branch", "repo", t.Repo, "branch", t.Branch)
		err = gitutil.CreateBranch(ctx, r.Dir, t.Branch, "origin/"+r.BaseBranch)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	t.setState(StateProvisioning)
	slog.Info("starting container", "repo", t.Repo, "branch", t.Branch)
	name, err := r.Container.Start(ctx, r.Dir, t.Branch, labels)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	slog.Info("container started", "repo", t.Repo, "branch", t.Branch)

	// Switch back to the base branch so the next task can create its branch.
	if err := gitutil.CheckoutBranch(ctx, r.Dir, r.BaseBranch); err != nil {
		return "", fmt.Errorf("checkout base: %w", err)
	}
	return name, nil
}

// PullChanges runs md diff + md pull for the given branch. Returns the diff
// stat and the first error encountered.
func (r *Runner) PullChanges(ctx context.Context, branch string) (string, error) {
	r.initDefaults()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	diffStat, _ := r.Container.Diff(ctx, r.Dir, branch, "--stat")
	slog.Info("pulling changes", "repo", filepath.Base(r.Dir), "branch", branch)
	if err := r.Container.Pull(ctx, r.Dir, branch); err != nil {
		return diffStat, err
	}
	return diffStat, nil
}

// PushChanges pushes local changes into the container.
func (r *Runner) PushChanges(ctx context.Context, branch string) error {
	r.initDefaults()
	slog.Info("pushing changes to container", "repo", filepath.Base(r.Dir), "branch", branch)
	return r.Container.Push(ctx, r.Dir, branch)
}

// KillContainer kills the md container for the given branch.
func (r *Runner) KillContainer(ctx context.Context, branch string) error {
	r.initDefaults()
	return r.Container.Kill(ctx, r.Dir, branch)
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
	safeRepo := strings.ReplaceAll(t.Repo, "/", "-")
	safeBranch := strings.ReplaceAll(t.Branch, "/", "-")
	name := t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
	f, err := os.OpenFile(filepath.Join(r.LogDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // name is derived from ksid, not arbitrary user input.
	if err != nil {
		slog.Warn("failed to create log file", "err", err)
		return nil, func() {}
	}
	// Write metadata header as the first line.
	meta := agent.MetaMessage{
		MessageType: "wmao_meta",
		Version:     1,
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
