// SSE event types sent to the frontend for task event streams.
// These structs are generated into TypeScript via tygo.
//
// Two separate event hierarchies exist:
//
//   - EventMessage (backend-neutral): the stable API contract consumed by the
//     frontend via /api/v1/tasks/{id}/events. Every backend (Claude, Gemini,
//     Codex, …) produces these events through its own converter. EventInit
//     includes a Harness field so the client knows which backend produced the
//     stream.
//
//   - ClaudeEventMessage (Claude-specific): the raw stream exposed on
//     /api/v1/tasks/{id}/raw_events for the Claude Code backend. When new
//     backends are added, each will get its own *EventMessage type for its raw
//     stream (e.g. GeminiEventMessage) while the generic EventMessage remains
//     the shared frontend contract.
//
// Each hierarchy has its own complete set of sub-event types (e.g. EventText
// vs ClaudeEventText). They are structurally identical today but kept separate
// so they can diverge as backends evolve independently.
package dto

import "encoding/json"

// EventKind identifies the type of SSE event.
type EventKind string

// Event kind constants.
const (
	EventKindInit       EventKind = "init"
	EventKindText       EventKind = "text"
	EventKindTextDelta  EventKind = "textDelta"
	EventKindToolUse    EventKind = "toolUse"
	EventKindToolResult EventKind = "toolResult"
	EventKindAsk        EventKind = "ask"
	EventKindUsage      EventKind = "usage"
	EventKindResult     EventKind = "result"
	EventKindSystem     EventKind = "system"
	EventKindUserInput  EventKind = "userInput"
	EventKindTodo       EventKind = "todo"
)

// EventMessage is a single SSE event in the backend-neutral stream
// (/api/v1/tasks/{id}/events). All backends produce these events.
type EventMessage struct {
	Kind       EventKind        `json:"kind"`
	Ts         int64            `json:"ts"`
	Init       *EventInit       `json:"init,omitempty"`
	Text       *EventText       `json:"text,omitempty"`
	TextDelta  *EventTextDelta  `json:"textDelta,omitempty"`
	ToolUse    *EventToolUse    `json:"toolUse,omitempty"`
	ToolResult *EventToolResult `json:"toolResult,omitempty"`
	Ask        *EventAsk        `json:"ask,omitempty"`
	Usage      *EventUsage      `json:"usage,omitempty"`
	Result     *EventResult     `json:"result,omitempty"`
	System     *EventSystem     `json:"system,omitempty"`
	UserInput  *EventUserInput  `json:"userInput,omitempty"`
	Todo       *EventTodo       `json:"todo,omitempty"`
}

// EventInit is emitted once at the start of a session. It includes a Harness
// field so the client knows which backend produced the stream.
type EventInit struct {
	Model        string   `json:"model"`
	AgentVersion string   `json:"agentVersion"`
	SessionID    string   `json:"sessionID"`
	Tools        []string `json:"tools"`
	Cwd          string   `json:"cwd"`
	Harness      string   `json:"harness"`
}

// EventText is an assistant text block.
type EventText struct {
	Text string `json:"text"`
}

// EventTextDelta is a streaming text fragment from --include-partial-messages.
type EventTextDelta struct {
	Text string `json:"text"`
}

// EventToolUse is emitted when the assistant invokes a tool.
type EventToolUse struct {
	ToolUseID string          `json:"toolUseID"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// EventToolResult is emitted when a tool call completes.
type EventToolResult struct {
	ToolUseID  string `json:"toolUseID"`
	DurationMs int64  `json:"durationMs"` // Server-computed; 0 if unknown.
	Error      string `json:"error,omitempty"`
}

// AskOption is a single option in an AskUserQuestion.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is a single question from AskUserQuestion.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// EventAsk is emitted when the agent asks the user a question.
type EventAsk struct {
	ToolUseID string        `json:"toolUseID"`
	Questions []AskQuestion `json:"questions"`
}

// EventUsage reports per-turn token usage.
type EventUsage struct {
	InputTokens              int    `json:"inputTokens"`
	OutputTokens             int    `json:"outputTokens"`
	CacheCreationInputTokens int    `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int    `json:"cacheReadInputTokens"`
	ServiceTier              string `json:"serviceTier,omitempty"`
	Model                    string `json:"model"`
}

// EventResult is emitted when the task reaches a terminal state.
type EventResult struct {
	Subtype       string     `json:"subtype"`
	IsError       bool       `json:"isError"`
	Result        string     `json:"result"`
	DiffStat      DiffStat   `json:"diffStat,omitzero"`
	TotalCostUSD  float64    `json:"totalCostUSD"`
	DurationMs    int64      `json:"durationMs"`
	DurationAPIMs int64      `json:"durationAPIMs"`
	NumTurns      int        `json:"numTurns"`
	Usage         EventUsage `json:"usage"`
}

// EventSystem is a system event (status, compact_boundary, etc.).
type EventSystem struct {
	Subtype string `json:"subtype"`
}

// EventUserInput is emitted when a user sends a text message to the agent.
type EventUserInput struct {
	Text string `json:"text"`
}

// TodoItem is a single todo entry from a TodoWrite tool call.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // "pending", "in_progress", "completed".
	ActiveForm string `json:"activeForm,omitempty"`
}

// EventTodo is emitted when the agent writes/updates its todo list.
type EventTodo struct {
	ToolUseID string     `json:"toolUseID"`
	Todos     []TodoItem `json:"todos"`
}

// ClaudeEventKind identifies the type of SSE event in the Claude-specific raw
// stream. Values are identical to EventKind; a separate type alias is used so
// that future backends can diverge if needed.
type ClaudeEventKind string

// Claude-specific event kind constants.
const (
	ClaudeEventKindInit       ClaudeEventKind = "init"
	ClaudeEventKindText       ClaudeEventKind = "text"
	ClaudeEventKindTextDelta  ClaudeEventKind = "textDelta"
	ClaudeEventKindToolUse    ClaudeEventKind = "toolUse"
	ClaudeEventKindToolResult ClaudeEventKind = "toolResult"
	ClaudeEventKindAsk        ClaudeEventKind = "ask"
	ClaudeEventKindUsage      ClaudeEventKind = "usage"
	ClaudeEventKindResult     ClaudeEventKind = "result"
	ClaudeEventKindSystem     ClaudeEventKind = "system"
	ClaudeEventKindUserInput  ClaudeEventKind = "userInput"
	ClaudeEventKindTodo       ClaudeEventKind = "todo"
)

// ClaudeEventMessage is a single SSE event in the Claude Code raw stream
// (/api/v1/tasks/{id}/raw_events). When additional backends are added, each
// will define its own *EventMessage (e.g. GeminiEventMessage) for its raw
// stream. The Kind field determines which payload field is non-nil.
//
// All sub-event types are Claude-specific duplicates of the generic types.
// They are structurally identical today but will diverge as backends evolve.
type ClaudeEventMessage struct {
	Kind       ClaudeEventKind        `json:"kind"`
	Ts         int64                  `json:"ts"`                   // Unix milliseconds when the backend received this message.
	Init       *ClaudeEventInit       `json:"init,omitempty"`       // Kind "init".
	Text       *ClaudeEventText       `json:"text,omitempty"`       // Kind "text".
	TextDelta  *ClaudeEventTextDelta  `json:"textDelta,omitempty"`  // Kind "textDelta".
	ToolUse    *ClaudeEventToolUse    `json:"toolUse,omitempty"`    // Kind "toolUse".
	ToolResult *ClaudeEventToolResult `json:"toolResult,omitempty"` // Kind "toolResult".
	Ask        *ClaudeEventAsk        `json:"ask,omitempty"`        // Kind "ask".
	Usage      *ClaudeEventUsage      `json:"usage,omitempty"`      // Kind "usage".
	Result     *ClaudeEventResult     `json:"result,omitempty"`     // Kind "result".
	System     *ClaudeEventSystem     `json:"system,omitempty"`     // Kind "system".
	UserInput  *ClaudeEventUserInput  `json:"userInput,omitempty"`  // Kind "userInput".
	Todo       *ClaudeEventTodo       `json:"todo,omitempty"`       // Kind "todo".
}

// ClaudeEventInit is emitted once at the start of a Claude session. Unlike the
// generic EventInit it omits the Harness field (always "claude" for this stream).
type ClaudeEventInit struct {
	Model        string   `json:"model"`
	AgentVersion string   `json:"agentVersion"`
	SessionID    string   `json:"sessionID"`
	Tools        []string `json:"tools"`
	Cwd          string   `json:"cwd"`
}

// ClaudeEventText is an assistant text block in the Claude raw stream.
type ClaudeEventText struct {
	Text string `json:"text"`
}

// ClaudeEventTextDelta is a streaming text fragment in the Claude raw stream.
type ClaudeEventTextDelta struct {
	Text string `json:"text"`
}

// ClaudeEventToolUse is emitted when Claude invokes a tool.
type ClaudeEventToolUse struct {
	ToolUseID string          `json:"toolUseID"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// ClaudeEventToolResult is emitted when a Claude tool call completes.
type ClaudeEventToolResult struct {
	ToolUseID  string `json:"toolUseID"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

// ClaudeAskOption is a single option in a Claude AskUserQuestion.
type ClaudeAskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ClaudeAskQuestion is a single question from Claude's AskUserQuestion.
type ClaudeAskQuestion struct {
	Question    string            `json:"question"`
	Header      string            `json:"header,omitempty"`
	Options     []ClaudeAskOption `json:"options"`
	MultiSelect bool              `json:"multiSelect,omitempty"`
}

// ClaudeEventAsk is emitted when Claude asks the user a question.
type ClaudeEventAsk struct {
	ToolUseID string              `json:"toolUseID"`
	Questions []ClaudeAskQuestion `json:"questions"`
}

// ClaudeEventUsage reports per-turn token usage in the Claude raw stream.
type ClaudeEventUsage struct {
	InputTokens              int    `json:"inputTokens"`
	OutputTokens             int    `json:"outputTokens"`
	CacheCreationInputTokens int    `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int    `json:"cacheReadInputTokens"`
	ServiceTier              string `json:"serviceTier,omitempty"`
	Model                    string `json:"model"`
}

// ClaudeEventResult is emitted when a Claude task reaches a terminal state.
type ClaudeEventResult struct {
	Subtype       string           `json:"subtype"`
	IsError       bool             `json:"isError"`
	Result        string           `json:"result"`
	DiffStat      DiffStat         `json:"diffStat,omitzero"`
	TotalCostUSD  float64          `json:"totalCostUSD"`
	DurationMs    int64            `json:"durationMs"`
	DurationAPIMs int64            `json:"durationAPIMs"`
	NumTurns      int              `json:"numTurns"`
	Usage         ClaudeEventUsage `json:"usage"`
}

// ClaudeEventSystem is a system event in the Claude raw stream.
type ClaudeEventSystem struct {
	Subtype string `json:"subtype"`
}

// ClaudeEventUserInput is emitted when a user sends a text message to Claude.
type ClaudeEventUserInput struct {
	Text string `json:"text"`
}

// ClaudeTodoItem is a single todo entry from a Claude TodoWrite tool call.
type ClaudeTodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
}

// ClaudeEventTodo is emitted when Claude writes/updates its todo list.
type ClaudeEventTodo struct {
	ToolUseID string           `json:"toolUseID"`
	Todos     []ClaudeTodoItem `json:"todos"`
}
