// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai"
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
	StateHasPlan            // Agent finished planning (ExitPlanMode with plan content), awaiting approval.
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
	case StateHasPlan:
		return "has_plan"
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
	// Immutable fields — set at creation, never modified.
	ID            ksid.ID
	InitialPrompt agent.Prompt  // Initial prompt text and optional images.
	Repo          string        // Relative repo path (for display/API).
	BaseBranch    string        // Branch to fork from; empty means use the runner's default.
	Harness       agent.Harness // Agent harness ("claude", "gemini", etc.).
	Model         string        // User-requested model; passed to agent CLI.
	DockerImage   string        // Custom Docker base image; empty means use the default.
	Tailscale     bool          // Enable Tailscale networking in the container.
	USB           bool          // Enable USB passthrough in the container.
	Display       bool          // Enable Xvfb display in the container.
	MaxTurns      int           // Maximum number of turns before task is terminated.
	StartedAt     time.Time     // When the task was created.
	Provider      genai.Provider

	// Write-once fields — set during setup/adoption, never modified after.
	Branch        string
	Container     string
	TailscaleFQDN string // Tailscale FQDN assigned to the container (empty if not available).
	RelayOffset   int64  // Bytes received from relay output.jsonl, for reconnect.

	// mu protects all fields below.
	mu             sync.Mutex
	state          State
	stateUpdatedAt time.Time // UTC timestamp of the last state transition.
	sessionID      string    // Agent session ID, captured from SystemInitMessage.
	reportedModel  string    // Model reported by SystemInitMessage (may differ from Model).
	agentVersion   string    // Agent version, captured from SystemInitMessage.
	planFile       string    // Path to plan file inside container, captured from Write tool_use.
	planContent    string    // Content of the plan file, captured from Write tool_use input.
	planDismissed  bool      // True after ClearMessages; suppresses plan tracking until the next ResultMessage.
	inPlanMode     bool      // True while the agent is in plan mode (between EnterPlanMode and ExitPlanMode).
	title          string    // LLM-generated short title; set via SetTitle.
	msgs           []agent.Message
	subs           []*sub         // active SSE subscribers
	handle         *SessionHandle // current active session; nil when no session is attached
	liveCostUSD    float64
	liveNumTurns   int
	liveDuration   time.Duration
	liveUsage      agent.Usage
	lastUsage      agent.Usage    // Most recent ResultMessage usage (active context).
	lastAPIUsage   agent.Usage    // Most recent per-API-call usage from AssistantMessage (context window fill).
	liveDiffStat   agent.DiffStat // Updated by DiffStatMessage from relay.
}

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s State) {
	t.state = s
	t.stateUpdatedAt = time.Now().UTC()
}

// SetState updates the state under the mutex and records the transition time.
func (t *Task) SetState(s State) {
	t.mu.Lock()
	t.setState(s)
	t.mu.Unlock()
}

// SetStateAt updates the state under the mutex with an explicit timestamp.
// Used during adoption to preserve the original transition time.
func (t *Task) SetStateAt(s State, at time.Time) {
	t.mu.Lock()
	t.state = s
	t.stateUpdatedAt = at
	t.mu.Unlock()
}

// SetStateIf atomically transitions the state to next only if the current
// state equals expected. Returns true if the transition occurred.
func (t *Task) SetStateIf(expected, next State) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != expected {
		return false
	}
	t.setState(next)
	return true
}

// GetState returns the current state under the mutex.
func (t *Task) GetState() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// GetSessionID returns the agent session ID under the mutex.
func (t *Task) GetSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// GetModel returns the agent-reported model if available, otherwise the
// user-requested model. Read under the mutex.
func (t *Task) GetModel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reportedModel != "" {
		return t.reportedModel
	}
	return t.Model
}

// GetPlanFile returns the plan file path under the mutex.
func (t *Task) GetPlanFile() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.planFile
}

// HasSession reports whether a session handle is attached.
func (t *Task) HasSession() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.handle != nil
}

// LiveStats returns the latest cost, turn count, duration, cumulative token
// usage, and the most recent turn's usage (active context).
func (t *Task) LiveStats() (costUSD float64, numTurns int, duration time.Duration, cumulativeUsage, lastTurnUsage agent.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveCostUSD, t.liveNumTurns, t.liveDuration, t.liveUsage, t.lastUsage
}

// LiveDiffStat returns the latest diff stat from the relay's periodic polling.
func (t *Task) LiveDiffStat() agent.DiffStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveDiffStat
}

// SetLiveDiffStat overwrites the live diff stat. Used by adoptOne to set
// the host-side branch diff after RestoreMessages, because the relay's
// diff_watcher only tracks uncommitted changes (git diff HEAD) which
// becomes empty after the agent commits.
func (t *Task) SetLiveDiffStat(ds agent.DiffStat) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.liveDiffStat = ds
}

// Title returns the task title under the mutex.
func (t *Task) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.title
}

// Snapshot holds volatile task fields read under the mutex. Used by the
// server to build API responses without data races on fields that
// addMessage/RestoreMessages modify concurrently.
type Snapshot struct {
	State          State
	StateUpdatedAt time.Time
	Title          string
	SessionID      string
	Model          string
	AgentVersion   string
	InPlanMode     bool
	PlanFile       string
	PlanContent    string
	CostUSD        float64
	NumTurns       int
	Duration       time.Duration
	Usage          agent.Usage
	LastUsage      agent.Usage
	LastAPIUsage   agent.Usage
	DiffStat       agent.DiffStat
}

// Snapshot returns a consistent read of all volatile fields under the mutex.
func (t *Task) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	model := t.reportedModel
	if model == "" {
		model = t.Model
	}
	return Snapshot{
		State:          t.state,
		StateUpdatedAt: t.stateUpdatedAt,
		Title:          t.title,
		SessionID:      t.sessionID,
		Model:          model,
		AgentVersion:   t.agentVersion,
		InPlanMode:     t.inPlanMode,
		PlanFile:       t.planFile,
		PlanContent:    t.planContent,
		CostUSD:        t.liveCostUSD,
		NumTurns:       t.liveNumTurns,
		Duration:       t.liveDuration,
		Usage:          t.liveUsage,
		LastUsage:      t.lastUsage,
		LastAPIUsage:   t.lastAPIUsage,
		DiffStat:       t.liveDiffStat,
	}
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
// Metadata-only messages (DiffStatMessage, RawMessage) after the
// ResultMessage are skipped during inference.
//
// State inference rules (applied only for non-terminal states):
//   - Trailing ResultMessage + last assistant has AskUserQuestion → StateAsking
//   - Trailing ResultMessage (no ask) → StateWaiting
//   - No trailing ResultMessage → state unchanged (agent was mid-output)
//
// Called during both log loading (loadTerminatedTasks) and container adoption
// (adoptOne). For adoption, the caller must handle the case where state
// remains StateRunning with no relay alive — see adoptOne.
func (t *Task) RestoreMessages(msgs []agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = msgs
	for i := len(msgs) - 1; i >= 0; i-- {
		if init, ok := msgs[i].(*agent.InitMessage); ok && init.SessionID != "" {
			t.sessionID = init.SessionID
			t.reportedModel = init.Model
			t.agentVersion = init.Version
			break
		}
	}
	// Restore plan state from tool_use events. A context_cleared marker
	// resets plan state — it means ClearMessages was called (e.g. "Clear
	// and execute plan"), so plan data before the marker is stale and plan
	// tracking is suppressed until the next ResultMessage.
	for _, m := range msgs {
		if sm, ok := m.(*agent.SystemMessage); ok && sm.Subtype == "context_cleared" {
			t.inPlanMode = false
			t.planFile = ""
			t.planContent = ""
			t.planDismissed = true
		}
		if tu, ok := m.(*agent.ToolUseMessage); ok {
			t.trackToolUse(tu)
		}
		if u, ok := m.(*agent.UsageMessage); ok {
			t.lastAPIUsage = u.Usage
		}
		if _, ok := m.(*agent.ResultMessage); ok {
			t.planDismissed = false
		}
	}
	// Restore live diff stat from the last DiffStatMessage or ResultMessage,
	// whichever appears later. ResultMessage carries the authoritative
	// host-side diff stat but a DiffStatMessage from the relay may follow it.
	for i := len(msgs) - 1; i >= 0; i-- {
		if ds, ok := msgs[i].(*agent.DiffStatMessage); ok {
			t.liveDiffStat = ds.DiffStat
			break
		}
		if rm, ok := msgs[i].(*agent.ResultMessage); ok && len(rm.DiffStat) > 0 {
			t.liveDiffStat = rm.DiffStat
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
		t.liveDuration = time.Duration(rm.DurationMs) * time.Millisecond
		t.liveUsage.InputTokens += rm.Usage.InputTokens
		t.liveUsage.OutputTokens += rm.Usage.OutputTokens
		t.liveUsage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		t.liveUsage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
		t.lastUsage = rm.Usage
	}
	// Infer state: if the last agent-emitted message is a ResultMessage, the
	// agent finished its turn and is waiting for user input (or asking a
	// question). Skip trailing DiffStatMessages — the relay emits periodic
	// diff stats that can appear after the ResultMessage.
	// Only override non-terminal states — terminated/failed tasks loaded from
	// logs must keep their recorded state.
	if len(msgs) > 0 && t.state != StateTerminated && t.state != StateFailed && t.state != StateTerminating {
		if lastAgentMessage(msgs) != nil {
			switch {
			case lastTurnHasAsk(msgs):
				t.setState(StateAsking)
			case lastTurnHasExitPlan(msgs) && t.planContent != "":
				t.setState(StateHasPlan)
			default:
				t.setState(StateWaiting)
			}
		}
	}
}

func (t *Task) addMessage(ctx context.Context, m agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
	// Capture metadata from the init message.
	if init, ok := m.(*agent.InitMessage); ok && init.SessionID != "" {
		t.sessionID = init.SessionID
		t.reportedModel = init.Model
		t.agentVersion = init.Version
	}
	// Track plan mode and plan file from tool_use events.
	if tu, ok := m.(*agent.ToolUseMessage); ok {
		t.trackToolUse(tu)
	}
	if u, ok := m.(*agent.UsageMessage); ok {
		t.lastAPIUsage = u.Usage
	}
	// Transition to running when the agent starts producing output
	// while the task is in a waiting state. This covers the case where
	// the server restarts and RestoreMessages inferred StateWaiting
	// from a trailing ResultMessage, but the agent already started a
	// new turn on the relay before we reattached.
	switch m.(type) {
	case *agent.TextMessage, *agent.ToolUseMessage, *agent.AskMessage, *agent.TodoMessage:
		if t.state == StateWaiting || t.state == StateAsking || t.state == StateHasPlan {
			t.setState(StateRunning)
		}
	}
	// Update live diff stat from relay polling.
	if ds, ok := m.(*agent.DiffStatMessage); ok {
		t.liveDiffStat = ds.DiffStat
	}
	// Transition to waiting/asking when a result arrives.
	if rm, ok := m.(*agent.ResultMessage); ok {
		if len(rm.DiffStat) > 0 {
			t.liveDiffStat = rm.DiffStat
		}
		t.liveCostUSD = rm.TotalCostUSD
		t.liveNumTurns = rm.NumTurns
		t.liveDuration = time.Duration(rm.DurationMs) * time.Millisecond
		t.liveUsage.InputTokens += rm.Usage.InputTokens
		t.liveUsage.OutputTokens += rm.Usage.OutputTokens
		t.liveUsage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		t.liveUsage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
		t.lastUsage = rm.Usage
		t.planDismissed = false
		// Transition Running→Waiting/Asking/HasPlan. Also handle
		// Running/Waiting because watchSession may have already set
		// Waiting before the dispatch goroutine processed this
		// ResultMessage (it does a blocking Fetch first). In that case
		// we still need to distinguish Waiting from Asking/HasPlan.
		if t.state == StateRunning || t.state == StateWaiting {
			switch {
			case lastTurnHasAsk(t.msgs):
				t.setState(StateAsking)
			case lastTurnHasExitPlan(t.msgs) && t.planContent != "":
				t.setState(StateHasPlan)
			default:
				t.setState(StateWaiting)
			}
		}
		go t.GenerateTitle(ctx)
	}
	// Fan out to subscribers (non-blocking).
	for i := 0; i < len(t.subs); i++ {
		select {
		case t.subs[i].ch <- m:
		default:
			// Slow subscriber — drop and remove.
			t.subs[i].close()
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			i--
		}
	}
}

// writeToolInput is the JSON input schema for the Write tool_use block.
type writeToolInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editToolInput is the JSON input schema for the Edit tool_use block.
type editToolInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// trackToolUse inspects a ToolUseMessage for plan-related tools and updates
// PlanFile and InPlanMode accordingly. The caller must hold t.mu.
func (t *Task) trackToolUse(tu *agent.ToolUseMessage) {
	switch tu.Name {
	case "EnterPlanMode":
		t.inPlanMode = true
	case "ExitPlanMode":
		t.inPlanMode = false
		tu.PlanContent = t.planContent
	case "Write":
		if t.planDismissed {
			return
		}
		var input writeToolInput
		if json.Unmarshal(tu.Input, &input) == nil && strings.Contains(input.FilePath, ".claude/plans/") {
			t.planFile = input.FilePath
			t.planContent = input.Content
		}
	case "Edit":
		if t.planDismissed {
			return
		}
		var input editToolInput
		if json.Unmarshal(tu.Input, &input) == nil && t.planFile == input.FilePath && t.planContent != "" {
			if input.ReplaceAll {
				t.planContent = strings.ReplaceAll(t.planContent, input.OldString, input.NewString)
			} else {
				t.planContent = strings.Replace(t.planContent, input.OldString, input.NewString, 1)
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
func (t *Task) ClearMessages(ctx context.Context) {
	t.addMessage(ctx, syntheticContextCleared())

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = ""
	t.liveCostUSD = 0
	t.liveNumTurns = 0
	t.liveDuration = 0
	t.inPlanMode = false
	t.planFile = ""
	t.planContent = ""
	t.planDismissed = true
}

// syntheticUserInput creates a UserInputMessage representing user-provided
// text/image input. It is injected into the message stream so that the JSONL
// log and SSE events contain an explicit record of every user message.
func syntheticUserInput(p agent.Prompt) *agent.UserInputMessage {
	var images []agent.ImageData
	if len(p.Images) > 0 {
		images = make([]agent.ImageData, len(p.Images))
		copy(images, p.Images)
	}
	return &agent.UserInputMessage{
		Text:   p.Text,
		Images: images,
	}
}

// lastAgentMessage scans backwards through msgs, skipping non-semantic
// messages (DiffStatMessage, TextDeltaMessage, RawMessage), and returns the
// trailing ResultMessage if the last semantically meaningful message is a
// result. Returns nil if it is not a ResultMessage (agent still producing
// output) or msgs is empty.
func lastAgentMessage(msgs []agent.Message) *agent.ResultMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch m := msgs[i].(type) {
		case *agent.DiffStatMessage:
			continue // Relay metadata; skip.
		case *agent.TextDeltaMessage:
			continue // Streaming delta; skip.
		case *agent.RawMessage:
			continue // tool_progress, etc.; skip.
		case *agent.UsageMessage:
			continue // Token usage metadata; skip.
		case *agent.ResultMessage:
			return m
		default:
			return nil
		}
	}
	return nil
}

// lastTurnHasAsk reports whether the current turn contains an AskMessage.
// It scans backwards from the end until it hits a previous turn's
// ResultMessage boundary. The caller may include the current turn's
// ResultMessage in the slice (it's the trigger for this check), so we skip
// the first ResultMessage we encounter.
func lastTurnHasAsk(msgs []agent.Message) bool {
	skippedResult := false
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].(type) {
		case *agent.AskMessage:
			return true
		case *agent.ResultMessage:
			if skippedResult {
				return false
			}
			skippedResult = true
		}
	}
	return false
}

// lastTurnHasExitPlan reports whether the current turn contains an ExitPlanMode
// tool call. It scans backwards from the end until it hits a previous turn's
// ResultMessage boundary, mirroring lastTurnHasAsk.
func lastTurnHasExitPlan(msgs []agent.Message) bool {
	skippedResult := false
	for i := len(msgs) - 1; i >= 0; i-- {
		switch m := msgs[i].(type) {
		case *agent.ToolUseMessage:
			if m.Name == "ExitPlanMode" {
				return true
			}
		case *agent.ResultMessage:
			if skippedResult {
				return false
			}
			skippedResult = true
		}
	}
	return false
}

// sub is an SSE subscriber with a once-guarded close to prevent double-close
// panics when both the fan-out (slow subscriber drop) and context cancellation
// race to close the channel.
type sub struct {
	ch   chan agent.Message
	once sync.Once
}

func (s *sub) close() {
	s.once.Do(func() { close(s.ch) })
}

// Subscribe returns a snapshot of the message history and a channel that
// receives only live messages arriving after the snapshot. The caller must
// write the history to the client first, then range over the channel.
// The returned function unsubscribes and must be called exactly once.
func (t *Task) Subscribe(ctx context.Context) (history []agent.Message, live <-chan agent.Message, unsubFn func()) {
	s := &sub{ch: make(chan agent.Message, 256)}

	t.mu.Lock()
	// Snapshot history under lock — no channel writes, so no deadlock risk
	// regardless of history size.
	history = append([]agent.Message(nil), t.msgs...)
	t.subs = append(t.subs, s)
	t.mu.Unlock()

	unsub := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, ss := range t.subs {
			if ss == s {
				t.subs = append(t.subs[:i], t.subs[i+1:]...)
				break
			}
		}
	}

	// Close channel when context is done.
	go func() {
		<-ctx.Done()
		unsub()
		s.close()
	}()

	return history, s.ch, unsub
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
func (t *Task) SendInput(ctx context.Context, p agent.Prompt) error {
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
	state := t.state
	if h != nil && (state == StateWaiting || state == StateAsking || state == StateHasPlan) {
		t.setState(StateRunning)
		// Plan content is preserved — the UI hides naturally while the
		// task is Running (isWaiting is false). When the agent finishes,
		// the plan reappears (original or updated via Write/Edit).
		// ClearMessages (the "Clear and execute plan" path) is the only
		// place that erases plan state.
	}
	t.mu.Unlock()
	if h == nil {
		return fmt.Errorf("no active session (state=%s session=%s)", state, sessionStatus)
	}
	t.addMessage(ctx, syntheticUserInput(p))
	return h.Session.Send(p)
}

const titleSystemPrompt = "Summarize this coding task conversation in 3-8 words as a short title. Reply with ONLY the title, no quotes."

// SetTitle sets the title under the mutex. Empty strings are ignored to
// preserve the prompt-fallback invariant.
func (t *Task) SetTitle(title string) {
	if title == "" {
		return
	}
	t.mu.Lock()
	t.title = title
	t.mu.Unlock()
}

// GenerateTitle asks the LLM for a short title from the prompt and any result
// messages. No-op when the provider is unconfigured.
func (t *Task) GenerateTitle(ctx context.Context) {
	if t.Provider == nil {
		return
	}
	msgs := t.Messages()
	var b strings.Builder
	for _, m := range msgs {
		if v, ok := m.(*agent.ResultMessage); ok && v.Result != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("Result: ")
			b.WriteString(v.Result)
		}
	}
	// Prepend the original prompt.
	// TODO: Use the images too.
	input := "Prompt: " + t.InitialPrompt.Text
	if b.Len() > 0 {
		input += "\n" + b.String()
	}
	// Truncate to keep it working on most providers.
	const maxChars = 50000
	if len(input) > maxChars {
		input = input[:maxChars]
	}

	start := time.Now()
	res, err := t.Provider.GenSync(ctx,
		genai.Messages{genai.NewTextMessage(input)},
		&genai.GenOptionText{SystemPrompt: titleSystemPrompt},
	)
	d := time.Since(start).Round(time.Millisecond)
	if err != nil {
		slog.Warn("title generation failed", "task", t.ID, "err", err, "d", d)
		return
	}
	// Strip surrounding quotes if the model adds them despite instructions.
	title := strings.Trim(strings.TrimSpace(res.String()), "\"'`")
	if title == "" {
		slog.Warn("title generation returned empty", "task", t.ID, "d", d, "raw", res.String())
		return
	}
	slog.Info("title generated", "task", t.ID, "title", title, "d", d)
	t.SetTitle(title)
}
