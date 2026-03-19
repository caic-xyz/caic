// Wire types for the Claude Code NDJSON streaming protocol.

package claude

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// ---------- Envelope probe ----------

// typeProbe extracts the type discriminator from a Claude Code JSONL record.
type typeProbe struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
}

// ---------- system/init ----------

// initWire is the wire representation of a system/init record.
type initWire struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"session_id"`
	Tools     []string        `json:"tools"`
	Model     string          `json:"model"`
	Version   string          `json:"claude_code_version"`
	UUID      string          `json:"uuid"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`

	Agents         json.RawMessage `json:"agents,omitempty"`
	APIKeySource   json.RawMessage `json:"apiKeySource,omitempty"`
	FastModeState  json.RawMessage `json:"fast_mode_state,omitempty"`
	MCPServers     json.RawMessage `json:"mcp_servers,omitempty"`
	OutputStyle    json.RawMessage `json:"output_style,omitempty"`
	PermissionMode json.RawMessage `json:"permissionMode,omitempty"`
	Plugins        json.RawMessage `json:"plugins,omitempty"`
	Skills         json.RawMessage `json:"skills,omitempty"`
	SlashCommands  json.RawMessage `json:"slash_commands,omitempty"`

	jsonutil.Overflow
}

var initWireKnown = jsonutil.KnownFields(initWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *initWire) UnmarshalJSON(data []byte) error {
	type Alias initWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, initWireKnown, "initWire")
}

// ---------- system (non-init) ----------

// systemWire is the wire representation of a non-init system record.
type systemWire struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`

	// task_started / task_progress / task_notification fields.
	Description  json.RawMessage `json:"description,omitempty"`
	TaskID       json.RawMessage `json:"task_id,omitempty"`
	TaskType     json.RawMessage `json:"task_type,omitempty"`
	ToolUseID    json.RawMessage `json:"tool_use_id,omitempty"`
	LastToolName json.RawMessage `json:"last_tool_name,omitempty"`
	Status       json.RawMessage `json:"status,omitempty"`
	UsageExtra   json.RawMessage `json:"usage,omitempty"`
	OutputFile   json.RawMessage `json:"output_file,omitempty"`
	Summary      json.RawMessage `json:"summary,omitempty"`

	// api_retry fields.
	Attempt      json.RawMessage `json:"attempt,omitempty"`
	MaxRetries   json.RawMessage `json:"max_retries,omitempty"`
	RetryDelayMs json.RawMessage `json:"retry_delay_ms,omitempty"`
	ErrorStatus  json.RawMessage `json:"error_status,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`

	// Other optional fields.
	PermissionMode  json.RawMessage `json:"permissionMode,omitempty"`
	CompactMetadata json.RawMessage `json:"compact_metadata,omitempty"`
	Prompt          json.RawMessage `json:"prompt,omitempty"`

	jsonutil.Overflow
}

var systemWireKnown = jsonutil.KnownFields(systemWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *systemWire) UnmarshalJSON(data []byte) error {
	type Alias systemWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, systemWireKnown, "systemWire")
}

// ---------- assistant ----------

// assistantWire is the wire representation of an assistant record.
type assistantWire struct {
	Type            string               `json:"type"`
	SessionID       string               `json:"session_id"`
	UUID            string               `json:"uuid"`
	Timestamp       json.RawMessage      `json:"timestamp,omitempty"`
	Message         assistantMessageBody `json:"message"`
	ParentToolUseID string               `json:"parent_tool_use_id"`
	Error           string               `json:"error"`
	jsonutil.Overflow
}

var assistantWireKnown = jsonutil.KnownFields(assistantWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *assistantWire) UnmarshalJSON(data []byte) error {
	type Alias assistantWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, assistantWireKnown, "assistantWire")
}

// assistantMessageBody is the inner message object within an assistant record.
type assistantMessageBody struct {
	ID           string             `json:"id"`
	Type         string             `json:"type,omitempty"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []contentBlockWire `json:"content"`
	Usage        agent.Usage        `json:"usage"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`

	Container         json.RawMessage `json:"container,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`

	jsonutil.Overflow
}

var assistantMessageBodyKnown = jsonutil.KnownFields(assistantMessageBody{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *assistantMessageBody) UnmarshalJSON(data []byte) error {
	type Alias assistantMessageBody
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, assistantMessageBodyKnown, "assistantMessageBody")
}

// contentBlockStartWire is the content_block field in a content_block_start streaming event.
type contentBlockStartWire struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// contentBlockWire is a single content block inside an assistant message.
// This is a flat union: fields are populated depending on Type.
//
//   - "text":        Text
//   - "thinking":    Thinking, Signature
//   - "tool_use":    ID, Name, Input
//   - "tool_result": ToolUseID, Content, IsError
type contentBlockWire struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	// tool_result fields (inline MCP tool results).
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ---------- user ----------

// userWire is the wire representation of a user record.
type userWire struct {
	Type            string          `json:"type"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id,omitempty"`
	Timestamp       json.RawMessage `json:"timestamp,omitempty"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	ToolUseResult   json.RawMessage `json:"tool_use_result,omitempty"`
	IsSynthetic     bool            `json:"isSynthetic,omitempty"`
	jsonutil.Overflow
}

var userWireKnown = jsonutil.KnownFields(userWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *userWire) UnmarshalJSON(data []byte) error {
	type Alias userWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, userWireKnown, "userWire")
}

// ---------- result ----------

// resultWire is the wire representation of a result record.
type resultWire struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	DurationMs       int64           `json:"duration_ms"`
	DurationAPIMs    int64           `json:"duration_api_ms"`
	NumTurns         int             `json:"num_turns"`
	Result           string          `json:"result"`
	SessionID        string          `json:"session_id"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            agent.Usage     `json:"usage"`
	UUID             string          `json:"uuid"`
	StructuredOutput *string         `json:"structured_output"`
	Timestamp        json.RawMessage `json:"timestamp,omitempty"`

	FastModeState     json.RawMessage `json:"fast_mode_state,omitempty"`
	ModelUsage        json.RawMessage `json:"modelUsage,omitempty"`
	PermissionDenials json.RawMessage `json:"permission_denials,omitempty"`
	StopReason        json.RawMessage `json:"stop_reason,omitempty"`

	jsonutil.Overflow
}

var resultWireKnown = jsonutil.KnownFields(resultWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *resultWire) UnmarshalJSON(data []byte) error {
	type Alias resultWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, resultWireKnown, "resultWire")
}

// ---------- stream_event ----------

// streamEventWire is the wire representation of a stream_event record.
type streamEventWire struct {
	Type            string          `json:"type"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
	Timestamp       json.RawMessage `json:"timestamp,omitempty"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Event           streamEventData `json:"event"`
	jsonutil.Overflow
}

var streamEventWireKnown = jsonutil.KnownFields(streamEventWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *streamEventWire) UnmarshalJSON(data []byte) error {
	type Alias streamEventWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, streamEventWireKnown, "streamEventWire")
}

// streamEventData is the nested event body inside a stream_event record.
type streamEventData struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	Delta        *streamDeltaWire `json:"delta,omitempty"`
	ContentBlock json.RawMessage  `json:"content_block,omitempty"`
	// message_start carries the full message object; message_delta carries
	// stop_reason and usage in a delta wrapper.
	Message json.RawMessage `json:"message,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

// streamDeltaWire is a delta object inside a stream event.
type streamDeltaWire struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	// message_delta carries stop_reason.
	StopReason string `json:"stop_reason,omitempty"`
}

// ---------- rate_limit_event ----------

// rateLimitEventWire is the wire representation of a rate_limit_event record.
// Emitted when the CLI's rate limit status transitions (e.g. allowed → allowed_warning).
type rateLimitEventWire struct {
	Type          string            `json:"type"`
	UUID          string            `json:"uuid"`
	SessionID     string            `json:"session_id"`
	Timestamp     json.RawMessage   `json:"timestamp,omitempty"`
	RateLimitInfo rateLimitInfoWire `json:"rate_limit_info"`
	jsonutil.Overflow
}

var rateLimitEventWireKnown = jsonutil.KnownFields(rateLimitEventWire{})

// UnmarshalJSON implements json.Unmarshaler.
func (w *rateLimitEventWire) UnmarshalJSON(data []byte) error {
	type Alias rateLimitEventWire
	return jsonutil.UnmarshalRecord(data, (*Alias)(w), &w.Overflow, rateLimitEventWireKnown, "rateLimitEventWire")
}

// rateLimitInfoWire is the nested rate limit info inside a rate_limit_event.
type rateLimitInfoWire struct {
	Status                string          `json:"status"`
	ResetsAt              json.RawMessage `json:"resets_at,omitempty"`
	RateLimitType         json.RawMessage `json:"rate_limit_type,omitempty"`
	Utilization           json.RawMessage `json:"utilization,omitempty"`
	OverageStatus         json.RawMessage `json:"overage_status,omitempty"`
	OverageResetsAt       json.RawMessage `json:"overage_resets_at,omitempty"`
	OverageDisabledReason json.RawMessage `json:"overage_disabled_reason,omitempty"`
}

// ---------- Helper types (no jsonutil.Overflow — not top-level wire objects) ----------

type askInput struct {
	Questions []agent.AskQuestion `json:"questions"`
}

type todoInput struct {
	Todos []agent.TodoItem `json:"todos"`
}

type userTextMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type userBlockMessage struct {
	Role    string             `json:"role"`
	Content []userContentBlock `json:"content"`
}

type userContentBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	Source    *imageSourceWire `json:"source,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	// Nested content and error flag for inline tool_result blocks (MCP tools).
	Content []toolResultContent `json:"content,omitempty"`
	IsError bool                `json:"is_error,omitempty"`
}

type imageSourceWire struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// toolResultWire is the message body format for tool results delivered via
// the top-level parent_tool_use_id path (standard Claude Code tools).
type toolResultWire struct {
	Content []toolResultContent `json:"content"`
	IsError bool                `json:"is_error"`
}

type toolResultContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
