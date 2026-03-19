// SSE event types sent to the frontend for task event streams.
// These structs are generated into TypeScript via tygo.
//
// EventMessage is the backend-neutral SSE contract consumed by the frontend
// via both /api/v1/tasks/{id}/events and /api/v1/tasks/{id}/raw_events.
// Every backend (Claude, Gemini, Codex, …) produces these events through its
// converter. EventInit includes a Harness field so the client knows which
// backend produced the stream.
package v1

import "encoding/json"

// EventKind identifies the type of SSE event.
type EventKind string

// Event kind constants.
const (
	EventKindInit            EventKind = "init"
	EventKindText            EventKind = "text"
	EventKindTextDelta       EventKind = "textDelta"
	EventKindToolUse         EventKind = "toolUse"
	EventKindToolResult      EventKind = "toolResult"
	EventKindAsk             EventKind = "ask"
	EventKindUsage           EventKind = "usage"
	EventKindResult          EventKind = "result"
	EventKindSystem          EventKind = "system"
	EventKindUserInput       EventKind = "userInput"
	EventKindTodo            EventKind = "todo"
	EventKindDiffStat        EventKind = "diffStat"
	EventKindError           EventKind = "error"
	EventKindThinking        EventKind = "thinking"
	EventKindThinkingDelta   EventKind = "thinkingDelta"
	EventKindSubagentStart   EventKind = "subagentStart"
	EventKindSubagentEnd     EventKind = "subagentEnd"
	EventKindLog             EventKind = "log"
	EventKindToolOutputDelta EventKind = "toolOutputDelta"
	EventKindWidget          EventKind = "widget"
	EventKindWidgetDelta     EventKind = "widgetDelta"
	EventKindRateLimit       EventKind = "rateLimit"
	EventKindStats           EventKind = "stats"
)

// EventMessage is a single SSE event in the backend-neutral stream
// (/api/v1/tasks/{id}/events). All backends produce these events.
type EventMessage struct {
	Kind            EventKind             `json:"kind"`
	Ts              int64                 `json:"ts"`
	Init            *EventInit            `json:"init,omitempty"`
	Text            *EventText            `json:"text,omitempty"`
	TextDelta       *EventTextDelta       `json:"textDelta,omitempty"`
	ToolUse         *EventToolUse         `json:"toolUse,omitempty"`
	ToolResult      *EventToolResult      `json:"toolResult,omitempty"`
	Ask             *EventAsk             `json:"ask,omitempty"`
	Usage           *EventUsage           `json:"usage,omitempty"`
	Result          *EventResult          `json:"result,omitempty"`
	System          *EventSystem          `json:"system,omitempty"`
	UserInput       *EventUserInput       `json:"userInput,omitempty"`
	Todo            *EventTodo            `json:"todo,omitempty"`
	DiffStat        *EventDiffStat        `json:"diffStat,omitempty"`
	Error           *EventError           `json:"error,omitempty"`
	Thinking        *EventThinking        `json:"thinking,omitempty"`
	ThinkingDelta   *EventThinkingDelta   `json:"thinkingDelta,omitempty"`
	SubagentStart   *EventSubagentStart   `json:"subagentStart,omitempty"`
	SubagentEnd     *EventSubagentEnd     `json:"subagentEnd,omitempty"`
	Log             *EventLog             `json:"log,omitempty"`
	ToolOutputDelta *EventToolOutputDelta `json:"toolOutputDelta,omitempty"`
	Widget          *EventWidget          `json:"widget,omitempty"`
	WidgetDelta     *EventWidgetDelta     `json:"widgetDelta,omitempty"`
	RateLimit       *EventRateLimit       `json:"rateLimit,omitempty"`
	Stats           *EventStats           `json:"stats,omitempty"`
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
	ToolUseID      string          `json:"toolUseID"`
	Name           string          `json:"name"`
	Input          json.RawMessage `json:"input"`
	PlanContent    string          `json:"planContent,omitempty"`    // Snapshot of plan content for ExitPlanMode events.
	InputTruncated bool            `json:"inputTruncated,omitempty"` // True when Input was omitted due to size; fetch via GET /api/v1/tasks/{id}/tool/{toolUseID}.
}

// EventToolResult is emitted when a tool call completes.
type EventToolResult struct {
	ToolUseID string  `json:"toolUseID"`
	Duration  float64 `json:"duration"` // Seconds; server-computed; 0 if unknown.
	Error     string  `json:"error,omitempty"`
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
// ReasoningOutputTokens is a subset of OutputTokens for extended thinking (Claude)
// or reasoning summaries (Codex). Zero when the harness does not report it.
type EventUsage struct {
	InputTokens              int    `json:"inputTokens"`
	OutputTokens             int    `json:"outputTokens"`
	CacheCreationInputTokens int    `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int    `json:"cacheReadInputTokens"`
	ReasoningOutputTokens    int    `json:"reasoningOutputTokens,omitempty"`
	Model                    string `json:"model"`
}

// EventResult is emitted when the task reaches a terminal state.
type EventResult struct {
	Subtype      string     `json:"subtype"`
	IsError      bool       `json:"isError"`
	Result       string     `json:"result"`
	DiffStat     DiffStat   `json:"diffStat,omitzero"`
	TotalCostUSD float64    `json:"totalCostUSD"`
	Duration     float64    `json:"duration"`    // Seconds.
	DurationAPI  float64    `json:"durationAPI"` // Seconds.
	NumTurns     int        `json:"numTurns"`
	Usage        EventUsage `json:"usage"`
}

// EventSystem is a system event (status, compact_boundary, etc.).
type EventSystem struct {
	Subtype string `json:"subtype"`
	Detail  string `json:"detail,omitempty"` // Optional human-readable detail (e.g. model names for model_rerouted).
}

// EventUserInput is emitted when a user sends a text message to the agent.
type EventUserInput struct {
	Text   string      `json:"text"`
	Images []ImageData `json:"images,omitempty"`
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

// EventDiffStat is emitted when the relay reports updated diff statistics.
type EventDiffStat struct {
	DiffStat DiffStat `json:"diffStat,omitzero"`
}

// EventError is emitted when the backend fails to parse an agent output line.
type EventError struct {
	Err  string `json:"err"`
	Line string `json:"line"`
}

// EventThinking is an assistant thinking block.
type EventThinking struct {
	Text string `json:"text"`
}

// EventThinkingDelta is a streaming thinking fragment.
type EventThinkingDelta struct {
	Text string `json:"text"`
}

// EventSubagentStart is emitted when a subagent task begins.
type EventSubagentStart struct {
	TaskID      string `json:"taskID"`
	Description string `json:"description"`
}

// EventSubagentEnd is emitted when a subagent task completes, fails, or stops.
type EventSubagentEnd struct {
	TaskID string `json:"taskID"`
	Status string `json:"status"` // "completed", "failed", "stopped"
}

// EventLog is a provisioning/startup log line from the container backend.
type EventLog struct {
	Line string `json:"line"`
}

// EventToolOutputDelta is a streaming output fragment from a running tool.
// Codex only: emitted for Bash stdout (item/commandExecution/outputDelta) and
// MCP tool progress messages (item/mcpToolCall/progress).
type EventToolOutputDelta struct {
	ToolUseID string `json:"toolUseID"`
	Delta     string `json:"delta"`
}

// EventWidget is emitted when the agent produces a complete HTML widget.
type EventWidget struct {
	ToolUseID string `json:"toolUseID"`
	Title     string `json:"title"`
	HTML      string `json:"html"`
}

// EventWidgetDelta is a streaming HTML fragment for progressive widget rendering.
type EventWidgetDelta struct {
	ToolUseID string `json:"toolUseID"`
	Delta     string `json:"delta"`
}

// EventRateLimit is emitted when the agent's rate limit status changes.
type EventRateLimit struct {
	Status        string  `json:"status"`        // "allowed", "allowed_warning", "rejected".
	ResetsAt      float64 `json:"resetsAt"`      // Unix epoch seconds; 0 if unknown.
	RateLimitType string  `json:"rateLimitType"` // "five_hour", "seven_day", etc.
	Utilization   float64 `json:"utilization"`   // 0.0–1.0.
}

// EventStats is a container resource usage snapshot emitted periodically.
type EventStats struct {
	Ts         int64   `json:"ts"`
	CPUPerc    float64 `json:"cpuPerc"`
	MemUsed    uint64  `json:"memUsed"`
	MemLimit   uint64  `json:"memLimit"`
	MemPerc    float64 `json:"memPerc"`
	NetRx      uint64  `json:"netRx"`
	NetTx      uint64  `json:"netTx"`
	BlockRead  uint64  `json:"blockRead"`
	BlockWrite uint64  `json:"blockWrite"`
	DiskUsed   int64   `json:"diskUsed"`
}
