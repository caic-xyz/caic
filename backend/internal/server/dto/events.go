// SSE event types sent to the frontend for task event streams.
// These structs are generated into TypeScript via tygo.
package dto

import "encoding/json"

// EventKind identifies the type of SSE event.
type EventKind = string

// Event kind constants.
const (
	EventKindInit       EventKind = "init"
	EventKindText       EventKind = "text"
	EventKindToolUse    EventKind = "toolUse"
	EventKindToolResult EventKind = "toolResult"
	EventKindAsk        EventKind = "ask"
	EventKindUsage      EventKind = "usage"
	EventKindResult     EventKind = "result"
	EventKindSystem     EventKind = "system"
	EventKindUserInput  EventKind = "userInput"
	EventKindTodo       EventKind = "todo"
)

// EventMessage is a single SSE event sent to the frontend. The Kind field
// determines which payload field is non-nil.
type EventMessage struct {
	Kind       EventKind        `json:"kind"`
	Ts         int64            `json:"ts"`                   // Unix milliseconds when the backend received this message.
	Init       *EventInit       `json:"init,omitempty"`       // Kind "init".
	Text       *EventText       `json:"text,omitempty"`       // Kind "text".
	ToolUse    *EventToolUse    `json:"toolUse,omitempty"`    // Kind "toolUse".
	ToolResult *EventToolResult `json:"toolResult,omitempty"` // Kind "toolResult".
	Ask        *EventAsk        `json:"ask,omitempty"`        // Kind "ask".
	Usage      *EventUsage      `json:"usage,omitempty"`      // Kind "usage".
	Result     *EventResult     `json:"result,omitempty"`     // Kind "result".
	System     *EventSystem     `json:"system,omitempty"`     // Kind "system".
	UserInput  *EventUserInput  `json:"userInput,omitempty"`  // Kind "userInput".
	Todo       *EventTodo       `json:"todo,omitempty"`       // Kind "todo".
}

// EventInit is emitted once at the start of a session.
type EventInit struct {
	Model             string   `json:"model"`
	ClaudeCodeVersion string   `json:"claudeCodeVersion"`
	SessionID         string   `json:"sessionID"`
	Tools             []string `json:"tools"`
	Cwd               string   `json:"cwd"`
}

// EventText is an assistant text block.
type EventText struct {
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
	TotalCostUSD  float64    `json:"totalCostUSD"`
	DurationMs    int64      `json:"durationMs"`
	DurationAPIMs int64      `json:"durationAPIMs"`
	NumTurns      int        `json:"numTurns"`
	Usage         EventUsage `json:"usage"`
}

// EventSystem is a generic system event (status, compact_boundary, etc.).
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
