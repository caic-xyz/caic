// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/server/dto"
	"github.com/maruel/ksid"
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
	logW     io.WriteCloser     // session log file; nil when no log dir is configured
	doneCh   chan struct{}      // closed when user calls Terminate
	doneOnce sync.Once

	// Live stats accumulated from ResultMessages during execution.
	liveCostUSD    float64
	liveNumTurns   int
	liveDurationMs int64
	liveUsage      agent.Usage

	// onResult is called when a ResultMessage arrives, before fan-out to
	// subscribers. It returns the parsed diff stat. May be nil.
	onResult func() dto.DiffStat
}

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s State) {
	t.State = s
	t.StateUpdatedAt = time.Now().UTC()
}

// SetOnResult registers a callback invoked when a ResultMessage arrives.
// The callback returns the parsed diff stat to attach to the message.
func (t *Task) SetOnResult(fn func() dto.DiffStat) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onResult = fn
}

// LiveStats returns the latest cost, turn count, duration, and token usage
// accumulated from ResultMessages received during execution.
func (t *Task) LiveStats() (costUSD float64, numTurns int, durationMs int64, usage agent.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveCostUSD, t.liveNumTurns, t.liveDurationMs, t.liveUsage
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
	// Restore live stats: cost/turns/duration are cumulative in the last
	// ResultMessage, but usage (tokens) is per-query and must be summed.
	for _, m := range msgs {
		rm, ok := m.(*agent.ResultMessage)
		if !ok {
			continue
		}
		t.liveCostUSD = rm.TotalCostUSD
		t.liveNumTurns = rm.NumTurns
		t.liveDurationMs = rm.DurationMs
		t.liveUsage.InputTokens += rm.Usage.InputTokens
		t.liveUsage.OutputTokens += rm.Usage.OutputTokens
		t.liveUsage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		t.liveUsage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
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
	// Run the onResult callback outside the lock to avoid holding it during I/O.
	if rm, ok := m.(*agent.ResultMessage); ok {
		t.mu.Lock()
		fn := t.onResult
		t.mu.Unlock()
		if fn != nil {
			rm.DiffStat = fn()
		}
	}

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
		t.liveUsage.InputTokens += rm.Usage.InputTokens
		t.liveUsage.OutputTokens += rm.Usage.OutputTokens
		t.liveUsage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		t.liveUsage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
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

// syntheticContextCleared creates a SystemMessage marking a context-clear
// boundary. Injected into the message stream so SSE subscribers see the
// marker before history is wiped.
func syntheticContextCleared() *agent.SystemMessage {
	return &agent.SystemMessage{
		MessageType: "system",
		Subtype:     "context_cleared",
	}
}

// CloseSession gracefully shuts down the current agent session without
// signaling doneCh (i.e., without triggering Kill). The caller must hold no
// locks.
func (t *Task) CloseSession() {
	t.mu.Lock()
	session := t.session
	t.session = nil
	msgCh := t.msgCh
	t.msgCh = nil
	logW := t.logW
	t.logW = nil
	t.mu.Unlock()

	if session == nil {
		return
	}

	// Graceful: close stdin, wait for exit with timeout.
	session.Close()
	timer := time.NewTimer(10 * time.Second)
	select {
	case <-session.Done():
		timer.Stop()
		_, _ = session.Wait()
	case <-timer.C:
	}

	if msgCh != nil {
		close(msgCh)
	}
	if logW != nil {
		_ = logW.Close()
	}
}

// ClearMessages injects a context_cleared boundary marker into the message
// stream and resets live stats. Message history is preserved so that SSE
// subscribers (including reconnecting clients) can see the full timeline.
func (t *Task) ClearMessages() {
	t.addMessage(syntheticContextCleared())

	t.mu.Lock()
	defer t.mu.Unlock()
	t.SessionID = ""
	t.liveCostUSD = 0
	t.liveNumTurns = 0
	t.liveDurationMs = 0
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
