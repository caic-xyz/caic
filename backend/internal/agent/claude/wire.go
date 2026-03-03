// Wire types for the Claude Code NDJSON streaming protocol.

package claude

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/agent"
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
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	Cwd       string   `json:"cwd"`
	SessionID string   `json:"session_id"`
	Tools     []string `json:"tools"`
	Model     string   `json:"model"`
	Version   string   `json:"claude_code_version"`
	UUID      string   `json:"uuid"`
	Overflow
}

var initWireKnown = makeSet(
	"type", "subtype", "cwd", "session_id", "tools",
	"model", "claude_code_version", "uuid",
	"agents", "apiKeySource", "fast_mode_state", "mcp_servers",
	"output_style", "permissionMode", "plugins", "skills", "slash_commands",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *initWire) UnmarshalJSON(data []byte) error {
	type Alias initWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, initWireKnown, "initWire")
}

// ---------- system (non-init) ----------

// systemWire is the wire representation of a non-init system record.
type systemWire struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	UUID      string `json:"uuid"`
	Overflow
}

var systemWireKnown = makeSet(
	"type", "subtype", "session_id", "uuid",
	"description", "task_id", "task_type", "tool_use_id",
	"last_tool_name", "permissionMode", "status", "usage",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *systemWire) UnmarshalJSON(data []byte) error {
	type Alias systemWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, systemWireKnown, "systemWire")
}

// ---------- assistant ----------

// assistantWire is the wire representation of an assistant record.
type assistantWire struct {
	Type            string               `json:"type"`
	SessionID       string               `json:"session_id"`
	UUID            string               `json:"uuid"`
	Message         assistantMessageBody `json:"message"`
	ParentToolUseID string               `json:"parent_tool_use_id"`
	Error           string               `json:"error"`
	Overflow
}

var assistantWireKnown = makeSet(
	"type", "session_id", "uuid", "message",
	"parent_tool_use_id", "error",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *assistantWire) UnmarshalJSON(data []byte) error {
	type Alias assistantWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, assistantWireKnown, "assistantWire")
}

// assistantMessageBody is the inner message object within an assistant record.
type assistantMessageBody struct {
	ID           string             `json:"id"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []contentBlockWire `json:"content"`
	Usage        agent.Usage        `json:"usage"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Overflow
}

var assistantMessageBodyKnown = makeSet(
	"id", "role", "model", "content", "usage",
	"stop_reason", "stop_sequence", "type",
	"container", "context_management",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *assistantMessageBody) UnmarshalJSON(data []byte) error {
	type Alias assistantMessageBody
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, assistantMessageBodyKnown, "assistantMessageBody")
}

// contentBlockWire is a single content block inside an assistant message.
type contentBlockWire struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ---------- user ----------

// userWire is the wire representation of a user record.
type userWire struct {
	Type            string          `json:"type"`
	UUID            string          `json:"uuid"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Overflow
}

var userWireKnown = makeSet("type", "uuid", "session_id", "message", "parent_tool_use_id", "tool_use_result")

// UnmarshalJSON implements json.Unmarshaler.
func (w *userWire) UnmarshalJSON(data []byte) error {
	type Alias userWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, userWireKnown, "userWire")
}

// ---------- result ----------

// resultWire is the wire representation of a result record.
type resultWire struct {
	Type             string      `json:"type"`
	Subtype          string      `json:"subtype"`
	IsError          bool        `json:"is_error"`
	DurationMs       int64       `json:"duration_ms"`
	DurationAPIMs    int64       `json:"duration_api_ms"`
	NumTurns         int         `json:"num_turns"`
	Result           string      `json:"result"`
	SessionID        string      `json:"session_id"`
	TotalCostUSD     float64     `json:"total_cost_usd"`
	Usage            agent.Usage `json:"usage"`
	UUID             string      `json:"uuid"`
	StructuredOutput *string     `json:"structured_output"`
	Overflow
}

var resultWireKnown = makeSet(
	"type", "subtype", "is_error", "duration_ms", "duration_api_ms",
	"num_turns", "result", "session_id", "total_cost_usd", "usage",
	"uuid", "structured_output",
	"fast_mode_state", "modelUsage", "permission_denials", "stop_reason",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *resultWire) UnmarshalJSON(data []byte) error {
	type Alias resultWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, resultWireKnown, "resultWire")
}

// ---------- stream_event ----------

// streamEventWire is the wire representation of a stream_event record.
type streamEventWire struct {
	Type            string          `json:"type"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Event           streamEventData `json:"event"`
	Overflow
}

var streamEventWireKnown = makeSet(
	"type", "uuid", "session_id", "parent_tool_use_id", "event",
)

// UnmarshalJSON implements json.Unmarshaler.
func (w *streamEventWire) UnmarshalJSON(data []byte) error {
	type Alias streamEventWire
	return unmarshalRecord(data, (*Alias)(w), &w.Overflow, streamEventWireKnown, "streamEventWire")
}

// streamEventData is the nested event body inside a stream_event record.
type streamEventData struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	Delta        *streamDeltaWire `json:"delta,omitempty"`
	ContentBlock json.RawMessage  `json:"content_block,omitempty"`
}

// streamDeltaWire is a delta object inside a stream event.
type streamDeltaWire struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
}

// ---------- Helper types (no Overflow — not top-level wire objects) ----------

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
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *imageSourceWire `json:"source,omitempty"`
}

type imageSourceWire struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type toolResultWire struct {
	Content []toolResultContent `json:"content"`
	IsError bool                `json:"is_error"`
}

type toolResultContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
