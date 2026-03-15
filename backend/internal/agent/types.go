package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Harness identifies the coding agent harness (e.g. Claude Code CLI, Gemini CLI).
type Harness string

// Supported agent harnesses.
const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
	Gemini Harness = "gemini"
	Kilo   Harness = "kilo"
)

// DiffFileStat describes changes to a single file.
type DiffFileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// DiffStat summarises the changes in a branch relative to its base.
type DiffStat []DiffFileStat

// Message is the interface for all agent streaming messages.
type Message interface {
	// Type returns the message type string.
	Type() string
}

// InitMessage is emitted when a session starts.
type InitMessage struct {
	SessionID string   `json:"session_id"`
	Cwd       string   `json:"cwd"`
	Tools     []string `json:"tools"`
	Model     string   `json:"model"`
	Version   string   `json:"claude_code_version"`
}

// Type implements Message.
func (m *InitMessage) Type() string { return "init" }

// SystemMessage is a generic system message (status, compact_boundary, etc.).
type SystemMessage struct {
	MessageType string `json:"type"`
	Subtype     string `json:"subtype"`
	SessionID   string `json:"session_id"`
	UUID        string `json:"uuid"`
	Detail      string `json:"detail,omitempty"` // Optional human-readable detail (e.g. model names for model_rerouted).
	Model       string `json:"model,omitempty"`  // Active model after model_rerouted; used to update task.reportedModel.
}

// Type implements Message.
func (m *SystemMessage) Type() string { return "system" }

// TextMessage is emitted when the agent produces text output.
type TextMessage struct {
	Text  string `json:"text"`
	Phase string `json:"phase,omitempty"` // Codex only: "commentary" | "final_answer" | "".
}

// Type implements Message.
func (m *TextMessage) Type() string { return "text" }

// ToolUseMessage is emitted when the agent invokes a tool (except
// AskUserQuestion and TodoWrite which have their own types).
type ToolUseMessage struct {
	ToolUseID   string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input,omitempty"`
	PlanContent string          `json:"-"` // Snapshot of plan content; set by task on ExitPlanMode.
}

// Type implements Message.
func (m *ToolUseMessage) Type() string { return "tool_use" }

// AskMessage is emitted when the agent asks the user a question via the
// AskUserQuestion tool.
type AskMessage struct {
	ToolUseID string        `json:"id"`
	Questions []AskQuestion `json:"questions"`
}

// Type implements Message.
func (m *AskMessage) Type() string { return "ask" }

// TodoMessage is emitted when the agent updates its todo list via the
// TodoWrite tool.
type TodoMessage struct {
	ToolUseID string     `json:"id"`
	Todos     []TodoItem `json:"todos"`
}

// Type implements Message.
func (m *TodoMessage) Type() string { return "todo" }

// UserInputMessage represents direct user text/image input (not a tool result).
type UserInputMessage struct {
	Text   string      `json:"text,omitempty"`
	Images []ImageData `json:"images,omitempty"`
}

// Type implements Message.
func (m *UserInputMessage) Type() string { return "user_input" }

// ToolResultMessage is emitted when a tool returns its result.
type ToolResultMessage struct {
	ToolUseID string `json:"tool_use_id"`
	Error     string `json:"error,omitempty"` // Non-empty when the tool reported an error.
}

// Type implements Message.
func (m *ToolResultMessage) Type() string { return "tool_result" }

// UsageMessage reports token consumption for a single API call.
type UsageMessage struct {
	Usage         Usage  `json:"usage"`
	Model         string `json:"model,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"` // Non-zero when the backend reports the active context window size.
}

// Type implements Message.
func (m *UsageMessage) Type() string { return "usage" }

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

// TodoItem is a single todo entry from a TodoWrite tool call.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // "pending", "in_progress", "completed".
	ActiveForm string `json:"activeForm,omitempty"`
}

// Usage tracks per-API-call token consumption as reported by the Anthropic API.
//
// The three input token fields are disjoint; total input context for one call
// equals InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// InputTokens is only the small non-cached, non-cache-creation portion
// (typically single-digit). The bulk of the input context lands in cache
// fields.
//
// In ResultMessage these values are per-query (sum of all API calls in the turn).
// Task.liveUsage sums them across all queries for cumulative totals.
//
// ReasoningOutputTokens is a subset of OutputTokens used for extended thinking
// (Claude) or reasoning summaries (Codex). Zero when the harness does not report it.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens,omitempty"`
}

// ResultMessage is the terminal message for a query.
type ResultMessage struct {
	MessageType   string   `json:"type"`
	Subtype       string   `json:"subtype"`
	IsError       bool     `json:"is_error"`
	DurationMs    int64    `json:"duration_ms"`
	DurationAPIMs int64    `json:"duration_api_ms"`
	NumTurns      int      `json:"num_turns"`
	Result        string   `json:"result"`
	SessionID     string   `json:"session_id"`
	TotalCostUSD  float64  `json:"total_cost_usd"`
	Usage         Usage    `json:"usage"`
	UUID          string   `json:"uuid"`
	DiffStat      DiffStat `json:"diff_stat,omitzero"` // Set by caic after running container diff.
}

// Type implements Message.
func (m *ResultMessage) Type() string { return "result" }

// TextDeltaMessage is a streaming text fragment, emitted when
// --include-partial-messages is enabled. Extracted from the nested wire
// format (stream_event → content_block_delta → text_delta) during parsing.
type TextDeltaMessage struct {
	Text string
}

// Type implements Message.
func (m *TextDeltaMessage) Type() string { return "text_delta" }

// ThinkingMessage is emitted when the agent produces a thinking block.
type ThinkingMessage struct {
	Text string `json:"text"`
}

// Type implements Message.
func (m *ThinkingMessage) Type() string { return "thinking" }

// ThinkingDeltaMessage is a streaming thinking fragment.
type ThinkingDeltaMessage struct {
	Text string
}

// Type implements Message.
func (m *ThinkingDeltaMessage) Type() string { return "thinking_delta" }

// ToolOutputDeltaMessage is a streaming output fragment from a tool execution.
// Codex only: emitted via item/commandExecution/outputDelta (Bash stdout) and
// item/mcpToolCall/progress (MCP tool progress messages).
type ToolOutputDeltaMessage struct {
	ToolUseID string
	Delta     string
}

// Type implements Message.
func (m *ToolOutputDeltaMessage) Type() string { return "tool_output_delta" }

// SubagentStartMessage is emitted when a subagent task begins.
type SubagentStartMessage struct {
	TaskID      string `json:"task_id"`
	Description string `json:"description"`
}

// Type implements Message.
func (m *SubagentStartMessage) Type() string { return "subagent_start" }

// SubagentEndMessage is emitted when a subagent task completes, fails, or stops.
type SubagentEndMessage struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // "completed", "failed", "stopped"
}

// Type implements Message.
func (m *SubagentEndMessage) Type() string { return "subagent_end" }

// MaxWidgetHTMLBytes is the maximum size of widget HTML the backend will
// forward to clients. Widgets exceeding this limit are replaced with an
// error message.
const MaxWidgetHTMLBytes = 256 * 1024 // 256 KB

// WidgetToolNames is the set of tool names that produce HTML widgets.
// Each harness parser checks this set to decide whether a tool_use block
// should emit WidgetMessage instead of ToolUseMessage.
var WidgetToolNames = map[string]bool{
	"show_widget":                                 true, // Direct tool name.
	"mcp__widget__show_widget":                    true, // MCP-prefixed (server "widget", tool "show_widget").
	"mcp__plugin_caic-widget_widget__show_widget": true, // Plugin MCP-prefixed.
}

// widgetInput is the expected JSON schema for the show_widget tool's input.
type widgetInput struct {
	Title      string `json:"title"`
	WidgetCode string `json:"widget_code"`
}

// NewWidgetMessage creates a WidgetMessage from raw tool input JSON. It
// extracts the title and widget_code fields and enforces MaxWidgetHTMLBytes.
// Shared by all backend parsers.
func NewWidgetMessage(toolUseID string, input json.RawMessage) *WidgetMessage {
	var w widgetInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &w)
	}
	html := w.WidgetCode
	if len(html) > MaxWidgetHTMLBytes {
		html = `<p style="color:red;font-family:system-ui">Widget too large (256 KB limit exceeded)</p>`
	}
	return &WidgetMessage{
		ToolUseID: toolUseID,
		Title:     w.Title,
		HTML:      html,
	}
}

// WidgetMessage is emitted when the agent produces an interactive HTML widget
// via a tool call (e.g. Claude's show_widget). The HTML is the complete,
// renderable widget code.
type WidgetMessage struct {
	ToolUseID string `json:"id"`
	Title     string `json:"title"`
	HTML      string `json:"html"`
}

// Type implements Message.
func (m *WidgetMessage) Type() string { return "widget" }

// WidgetDeltaMessage is a streaming fragment of widget HTML, emitted as the
// agent generates the widget code. Clients accumulate deltas for progressive
// rendering; the final WidgetMessage replaces them.
type WidgetDeltaMessage struct {
	ToolUseID string
	Delta     string // Partial HTML fragment.
}

// Type implements Message.
func (m *WidgetDeltaMessage) Type() string { return "widget_delta" }

// RawMessage is a pass-through for message types we don't need to inspect
// (tool_progress, etc.).
type RawMessage struct {
	MessageType string
	Raw         []byte
}

// Type implements Message.
func (m *RawMessage) Type() string { return m.MessageType }

// ParseErrorMessage is emitted when a backend output line cannot be decoded.
// It carries the error and the raw line for diagnostic display.
type ParseErrorMessage struct {
	Err  string
	Line string
}

// Type implements Message.
func (m *ParseErrorMessage) Type() string { return "parse_error" }

// LogMessage is a provisioning/startup log line from the container backend.
type LogMessage struct {
	Line string
}

// Type implements Message.
func (m *LogMessage) Type() string { return "log" }

// DiffStatMessage is emitted periodically by the relay's diff watcher thread
// with the current in-container git diff stats.
type DiffStatMessage struct {
	MessageType string   `json:"type"`
	DiffStat    DiffStat `json:"diff_stat"`
}

// Type implements Message.
func (m *DiffStatMessage) Type() string { return "caic_diff_stat" }

// MetaRepo describes one repository entry in a MetaMessage.
type MetaRepo struct {
	Name       string `json:"name"`
	BaseBranch string `json:"base_branch,omitempty"`
	Branch     string `json:"branch"`
}

// MetaMessage is written as the first line of a JSONL log file. It captures
// task-level metadata so logs can be reloaded on restart.
type MetaMessage struct {
	MessageType string     `json:"type"`
	Version     int        `json:"version"`
	Prompt      string     `json:"prompt"`
	Title       string     `json:"title,omitempty"`
	Repos       []MetaRepo `json:"repos"`
	Harness     Harness    `json:"harness"`
	Model       string     `json:"model,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	ForgeIssue  int        `json:"forge_issue,omitempty"` // Originating issue/PR number for bot comment callbacks.
}

// Type implements Message.
func (m *MetaMessage) Type() string { return "caic_meta" }

// Validate checks that all required fields are present and the version is supported.
func (m *MetaMessage) Validate() error {
	if m.MessageType != "caic_meta" {
		return fmt.Errorf("unexpected type %q", m.MessageType)
	}
	if m.Version != 1 {
		return fmt.Errorf("unsupported version %d", m.Version)
	}
	if m.Prompt == "" {
		return errors.New("missing prompt")
	}
	if m.Harness == "" {
		return errors.New("missing harness")
	}
	return nil
}

// MetaResultMessage is appended as the last line of a JSONL log file when a
// task reaches a terminal state.
type MetaResultMessage struct {
	MessageType              string   `json:"type"`
	State                    string   `json:"state"`
	Title                    string   `json:"title,omitempty"`
	CostUSD                  float64  `json:"cost_usd,omitempty"`
	Duration                 float64  `json:"duration,omitempty"` // Seconds.
	NumTurns                 int      `json:"num_turns,omitempty"`
	InputTokens              int      `json:"input_tokens,omitempty"`
	OutputTokens             int      `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens,omitempty"`
	DiffStat                 DiffStat `json:"diff_stat,omitzero"`
	Error                    string   `json:"error,omitempty"`
	AgentResult              string   `json:"agent_result,omitempty"`
}

// Type implements Message.
func (m *MetaResultMessage) Type() string { return "caic_result" }

// MetaPRMessage is written to the JSONL log when a PR is created so that the
// PR number can be restored on server restart.
type MetaPRMessage struct {
	MessageType string `json:"type"`
	ForgeOwner  string `json:"forge_owner"`
	ForgeRepo   string `json:"forge_repo"`
	ForgePR     int    `json:"forge_pr"`
}

// Type implements Message.
func (m *MetaPRMessage) Type() string { return "caic_pr" }

// MarshalMessage serializes a Message to JSON. For RawMessage, returns the
// original bytes to preserve unknown fields. For typed messages, uses
// json.Marshal.
func MarshalMessage(m Message) ([]byte, error) {
	if rm, ok := m.(*RawMessage); ok {
		return rm.Raw, nil
	}
	return json.Marshal(m)
}
