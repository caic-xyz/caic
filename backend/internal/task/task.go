// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
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

// SessionHandle bundles the three resources associated with an active agent
// session: the SSH session, the message dispatch channel, and the log writer.
type SessionHandle struct {
	Session *agent.Session
	MsgCh   chan agent.Message
	LogW    io.WriteCloser
}

// Task represents a single unit of work.
type Task struct {
	ID             ksid.ID
	Prompt         string
	Repo           string        // Relative repo path (for display/API).
	Harness        agent.Harness // Agent harness ("claude", "gemini", etc.).
	Image          string        // Custom Docker base image; empty means use the default.
	MaxTurns       int
	Branch         string
	Container      string
	State          State
	StateUpdatedAt time.Time // UTC timestamp of the last state transition.
	SessionID      string    // Claude Code session ID, captured from SystemInitMessage.
	Model          string    // Model name, captured from SystemInitMessage.
	AgentVersion   string    // Agent version, captured from SystemInitMessage.
	PlanFile       string    // Path to plan file inside container, captured from Write tool_use.
	InPlanMode     bool      // True while the agent is in plan mode (between EnterPlanMode and ExitPlanMode).
	StartedAt      time.Time
	RelayOffset    int64 // Bytes received from relay output.jsonl, for reconnect.

	mu     sync.Mutex
	msgs   []agent.Message
	subs   []chan agent.Message // active SSE subscribers
	handle *SessionHandle       // current active session; nil when no session is attached

	// Live stats accumulated from ResultMessages during execution.
	liveCostUSD    float64
	liveNumTurns   int
	liveDurationMs int64
	liveUsage      agent.Usage
}

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s State) {
	t.State = s
	t.StateUpdatedAt = time.Now().UTC()
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
//
// State inference rules (applied only for non-terminal states):
//   - Last message is ResultMessage + last assistant has AskUserQuestion → StateAsking
//   - Last message is ResultMessage (no ask) → StateWaiting
//   - Last message is NOT ResultMessage → state unchanged (agent was mid-output)
//
// Called during both log loading (loadTerminatedTasks) and container adoption
// (adoptOne). For adoption, the caller must handle the case where state
// remains StateRunning with no relay alive — see adoptOne.
func (t *Task) RestoreMessages(msgs []agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = msgs
	for i := len(msgs) - 1; i >= 0; i-- {
		if init, ok := msgs[i].(*agent.SystemInitMessage); ok && init.SessionID != "" {
			t.SessionID = init.SessionID
			t.Model = init.Model
			t.AgentVersion = init.Version
			break
		}
	}
	// Restore plan state from tool_use events.
	for _, m := range msgs {
		if am, ok := m.(*agent.AssistantMessage); ok {
			t.trackPlanState(am)
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
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
	// Capture metadata from the init message.
	if init, ok := m.(*agent.SystemInitMessage); ok && init.SessionID != "" {
		t.SessionID = init.SessionID
		t.Model = init.Model
		t.AgentVersion = init.Version
	}
	// Track plan mode and plan file from tool_use events.
	// Capture plan file path from Write tool_use targeting .claude/plans/.
	if am, ok := m.(*agent.AssistantMessage); ok {
		t.trackPlanState(am)
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

// trackPlanState inspects an AssistantMessage for plan-related tool_use blocks
// and updates PlanFile and InPlanMode accordingly. The caller must hold t.mu.
func (t *Task) trackPlanState(am *agent.AssistantMessage) {
	for _, b := range am.Message.Content {
		if b.Type != "tool_use" {
			continue
		}
		switch b.Name {
		case "EnterPlanMode":
			t.InPlanMode = true
		case "ExitPlanMode":
			t.InPlanMode = false
		case "Write":
			var input struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal(b.Input, &input) == nil && strings.Contains(input.FilePath, ".claude/plans/") {
				t.PlanFile = input.FilePath
			}
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

// AttachSession stores a SessionHandle on the task. The caller must not hold
// t.mu.
func (t *Task) AttachSession(h *SessionHandle) {
	t.mu.Lock()
	t.handle = h
	t.mu.Unlock()
}

// DetachSession atomically removes and returns the current SessionHandle,
// or nil if no session is attached. The caller must not hold t.mu.
func (t *Task) DetachSession() *SessionHandle {
	t.mu.Lock()
	h := t.handle
	t.handle = nil
	t.mu.Unlock()
	return h
}

// SessionDone returns the Done channel for the current session, or nil if no
// session is attached. The caller must not hold t.mu.
func (t *Task) SessionDone() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == nil {
		return nil
	}
	return t.handle.Session.Done()
}

// CloseAndDetachSession gracefully shuts down the current agent session
// (close stdin, wait up to 10s for exit) and returns the detached handle.
// Returns nil if no session was attached. Used by RestartSession which needs
// the graceful drain before starting a new session.
func (t *Task) CloseAndDetachSession() *SessionHandle {
	h := t.DetachSession()
	if h == nil {
		return nil
	}

	// Graceful: close stdin, wait for exit with timeout.
	h.Session.Close()
	timer := time.NewTimer(10 * time.Second)
	select {
	case <-h.Session.Done():
		timer.Stop()
		_, _ = h.Session.Wait()
	case <-timer.C:
	}
	return h
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

// SessionStatus describes why SendInput could not deliver a message.
//
// Session lifecycle:
//   - A session wraps an SSH process bridging the server to the in-container
//     relay daemon. It is set by Runner.Start, Runner.Reconnect, or
//     Runner.RestartSession.
//   - The session is cleared by CloseSession (during restart), Kill (during
//     termination), or lazily by SendInput when it detects the SSH process
//     already exited (Done channel closed).
//   - "none" means no session was ever attached for this task — either the task
//     hasn't started, or the relay died and reconnect failed.
//   - "exited" means a session existed but the underlying SSH process terminated
//     (relay or agent crashed, SSH dropped) before the user sent input.
type SessionStatus string

const (
	// SessionNone indicates no session was set on the task.
	SessionNone SessionStatus = "none"
	// SessionExited indicates the session's SSH process had already exited.
	SessionExited SessionStatus = "exited"
)

// SendInput sends a user message to the running agent.
//
// Returns an error if no session is active. The error includes the task state
// and a SessionStatus so the caller can diagnose why the session is missing
// (e.g. relay died vs. never connected). The session watcher now handles
// dead-session detection proactively, so SendInput no longer does lazy
// cleanup.
func (t *Task) SendInput(prompt string) error {
	t.mu.Lock()
	h := t.handle
	sessionStatus := SessionNone
	if h != nil {
		select {
		case <-h.Session.Done():
			sessionStatus = SessionExited
			h = nil
		default:
		}
	}
	if h != nil {
		t.setState(StateRunning)
	}
	state := t.State
	t.mu.Unlock()
	if h == nil {
		return fmt.Errorf("no active session (state=%s session=%s)", state, sessionStatus)
	}
	t.addMessage(syntheticUserInput(prompt))
	return h.Session.Send(prompt)
}
