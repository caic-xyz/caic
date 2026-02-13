package gemini

import (
	"encoding/json"
	"fmt"
)

// Record type constants.
const (
	TypeInit       = "init"
	TypeMessage    = "message"
	TypeToolUse    = "tool_use"
	TypeToolResult = "tool_result"
	TypeResult     = "result"
)

// Record is a single line from a Gemini CLI stream-json session.
// Use the typed accessor methods to get the concrete record after checking Type.
type Record struct {
	// Type discriminates the record kind.
	Type string `json:"type"`

	raw json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Record) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("Record: %w", err)
	}
	r.Type = probe.Type
	r.raw = append(r.raw[:0], data...)
	return nil
}

// Raw returns the original JSON bytes for this record.
func (r *Record) Raw() json.RawMessage { return r.raw }

// AsInit decodes the record as an InitRecord.
func (r *Record) AsInit() (*InitRecord, error) {
	var v InitRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsMessage decodes the record as a MessageRecord.
func (r *Record) AsMessage() (*MessageRecord, error) {
	var v MessageRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsToolUse decodes the record as a ToolUseRecord.
func (r *Record) AsToolUse() (*ToolUseRecord, error) {
	var v ToolUseRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsToolResult decodes the record as a ToolResultRecord.
func (r *Record) AsToolResult() (*ToolResultRecord, error) {
	var v ToolResultRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsResult decodes the record as a ResultRecord.
func (r *Record) AsResult() (*ResultRecord, error) {
	var v ResultRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// InitRecord is emitted at session start.
//
// Example:
//
//	{"type":"init","timestamp":"2026-02-13T19:00:05.416Z","session_id":"730077db-...","model":"auto-gemini-3"}
type InitRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`

	Overflow
}

var initRecordKnown = makeSet("type", "timestamp", "session_id", "model")

// UnmarshalJSON implements json.Unmarshaler.
func (r *InitRecord) UnmarshalJSON(data []byte) error {
	type Alias InitRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("InitRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("InitRecord: %w", err)
	}
	r.Extra = collectUnknown(raw, initRecordKnown)
	warnUnknown("InitRecord", r.Extra)
	return nil
}

// MessageRecord is a user or assistant text message.
// Delta messages (delta=true) are streaming fragments.
//
// Example:
//
//	{"type":"message","timestamp":"...","role":"user","content":"Say hello"}
//	{"type":"message","timestamp":"...","role":"assistant","content":"Hello.","delta":true}
type MessageRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`    // "user" or "assistant"
	Content   string `json:"content"` // Text content.
	Delta     bool   `json:"delta,omitempty"`

	Overflow
}

var messageRecordKnown = makeSet("type", "timestamp", "role", "content", "delta")

// UnmarshalJSON implements json.Unmarshaler.
func (r *MessageRecord) UnmarshalJSON(data []byte) error {
	type Alias MessageRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("MessageRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("MessageRecord: %w", err)
	}
	r.Extra = collectUnknown(raw, messageRecordKnown)
	warnUnknown("MessageRecord("+r.Role+")", r.Extra)
	return nil
}

// ToolUseRecord is emitted when the assistant invokes a tool.
//
// Example:
//
//	{"type":"tool_use","timestamp":"...","tool_name":"read_file","tool_id":"read_file-...",
//	 "parameters":{"file_path":"/etc/hostname"}}
type ToolUseRecord struct {
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	ToolName   string          `json:"tool_name"`
	ToolID     string          `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters"`

	Overflow
}

var toolUseRecordKnown = makeSet("type", "timestamp", "tool_name", "tool_id", "parameters")

// UnmarshalJSON implements json.Unmarshaler.
func (r *ToolUseRecord) UnmarshalJSON(data []byte) error {
	type Alias ToolUseRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ToolUseRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ToolUseRecord: %w", err)
	}
	r.Extra = collectUnknown(raw, toolUseRecordKnown)
	warnUnknown("ToolUseRecord("+r.ToolName+")", r.Extra)
	return nil
}

// ToolResultRecord is emitted after a tool execution completes.
//
// Example (success):
//
//	{"type":"tool_result","timestamp":"...","tool_id":"run_shell_command-...","status":"success","output":"md-caic-w0"}
//
// Example (error):
//
//	{"type":"tool_result","timestamp":"...","tool_id":"read_file-...","status":"error",
//	 "output":"Path not in workspace: ...","error":{"type":"invalid_tool_params","message":"Path not in workspace: ..."}}
type ToolResultRecord struct {
	Type      string           `json:"type"`
	Timestamp string           `json:"timestamp"`
	ToolID    string           `json:"tool_id"`
	Status    string           `json:"status"` // "success" or "error"
	Output    string           `json:"output"`
	Error     *ToolResultError `json:"error,omitempty"`

	Overflow
}

var toolResultRecordKnown = makeSet("type", "timestamp", "tool_id", "status", "output", "error")

// UnmarshalJSON implements json.Unmarshaler.
func (r *ToolResultRecord) UnmarshalJSON(data []byte) error {
	type Alias ToolResultRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ToolResultRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ToolResultRecord: %w", err)
	}
	r.Extra = collectUnknown(raw, toolResultRecordKnown)
	warnUnknown("ToolResultRecord", r.Extra)
	return nil
}

// ToolResultError describes a tool execution error.
type ToolResultError struct {
	Type    string `json:"type"`
	Message string `json:"message"`

	Overflow
}

var toolResultErrorKnown = makeSet("type", "message")

// UnmarshalJSON implements json.Unmarshaler.
func (e *ToolResultError) UnmarshalJSON(data []byte) error {
	type Alias ToolResultError
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ToolResultError: %w", err)
	}
	alias := (*Alias)(e)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ToolResultError: %w", err)
	}
	e.Extra = collectUnknown(raw, toolResultErrorKnown)
	warnUnknown("ToolResultError", e.Extra)
	return nil
}

// ResultRecord is the terminal event for a session.
//
// Example:
//
//	{"type":"result","timestamp":"...","status":"success",
//	 "stats":{"total_tokens":12359,"input_tokens":11744,"output_tokens":47,"cached":0,"input":11744,
//	          "duration_ms":5322,"tool_calls":0}}
type ResultRecord struct {
	Type      string       `json:"type"`
	Timestamp string       `json:"timestamp"`
	Status    string       `json:"status"` // "success" or "error"
	Stats     *ResultStats `json:"stats"`

	Overflow
}

var resultRecordKnown = makeSet("type", "timestamp", "status", "stats")

// UnmarshalJSON implements json.Unmarshaler.
func (r *ResultRecord) UnmarshalJSON(data []byte) error {
	type Alias ResultRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ResultRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ResultRecord: %w", err)
	}
	r.Extra = collectUnknown(raw, resultRecordKnown)
	warnUnknown("ResultRecord", r.Extra)
	return nil
}

// ResultStats contains token usage and timing for the session.
type ResultStats struct {
	TotalTokens  int   `json:"total_tokens"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	Cached       int   `json:"cached"`
	Input        int   `json:"input"` // Non-cached input tokens.
	DurationMs   int64 `json:"duration_ms"`
	ToolCalls    int   `json:"tool_calls"`

	Overflow
}

var resultStatsKnown = makeSet("total_tokens", "input_tokens", "output_tokens", "cached", "input", "duration_ms", "tool_calls")

// UnmarshalJSON implements json.Unmarshaler.
func (s *ResultStats) UnmarshalJSON(data []byte) error {
	type Alias ResultStats
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ResultStats: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ResultStats: %w", err)
	}
	s.Extra = collectUnknown(raw, resultStatsKnown)
	warnUnknown("ResultStats", s.Extra)
	return nil
}
