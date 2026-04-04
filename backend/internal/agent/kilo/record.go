package kilo

import (
	"encoding/json"

	"fmt"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// SSE event type constants.
const (
	TypeSystem       = "system"               // Synthetic init from bridge.
	TypePartUpdated  = "message.part.updated" // Part state change.
	TypePartDelta    = "message.part.delta"   // Streaming text fragment.
	TypeTurnClose    = "session.turn.close"   // Turn completed/errored.
	TypeSessionError = "session.error"        // Session-level error (e.g. model not found).
)

// Part type constants within message.part.updated events.
const (
	PartTypeText       = "text"
	PartTypeTool       = "tool"
	PartTypeStepStart  = "step-start"
	PartTypeStepFinish = "step-finish"
	PartTypeReasoning  = "reasoning"
)

// Record is a single NDJSON line from the kilo bridge.
// Use the typed accessor methods to get the concrete record after checking Type.
type Record struct {
	// Type discriminates the record kind.
	Type string `json:"type"`

	raw json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Record) UnmarshalJSON(data []byte) error {
	var probe typeProbe
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

// AsPartUpdated decodes the record as a PartUpdatedRecord.
func (r *Record) AsPartUpdated() (*PartUpdatedRecord, error) {
	var v PartUpdatedRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsPartDelta decodes the record as a PartDeltaRecord.
func (r *Record) AsPartDelta() (*PartDeltaRecord, error) {
	var v PartDeltaRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// AsTurnClose decodes the record as a TurnCloseRecord.
func (r *Record) AsTurnClose() (*TurnCloseRecord, error) {
	var v TurnCloseRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// InitRecord is the synthetic system/init event emitted by bridge.py.
//
// Example:
//
//	{"type":"system","subtype":"init","session_id":"ses_abc","model":"anthropic/claude-sonnet-4-20250514"}
type InitRecord struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`

	jsonutil.Overflow
}

var initRecordKnown = jsonutil.KnownFields(InitRecord{})

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
	r.Extra = jsonutil.CollectUnknown(raw, initRecordKnown)
	return nil
}

// PartUpdatedRecord wraps a message.part.updated SSE event.
//
// Example:
//
//	{"type":"message.part.updated","properties":{"part":{"id":"prt_1","type":"text","text":"Hello"}}}
type PartUpdatedRecord struct {
	Type       string                `json:"type"`
	Properties PartUpdatedProperties `json:"properties"`

	jsonutil.Overflow
}

var partUpdatedRecordKnown = jsonutil.KnownFields(PartUpdatedRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *PartUpdatedRecord) UnmarshalJSON(data []byte) error {
	type Alias PartUpdatedRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("PartUpdatedRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("PartUpdatedRecord: %w", err)
	}
	r.Extra = jsonutil.CollectUnknown(raw, partUpdatedRecordKnown)
	return nil
}

// PartUpdatedProperties contains the part within a message.part.updated event.
type PartUpdatedProperties struct {
	Part Part `json:"part"`
}

// Part is a single part within a kilo message. The Type field discriminates
// between text, tool, step-start, step-finish, and reasoning parts.
type Part struct {
	ID        string      `json:"id"`
	SessionID string      `json:"sessionID"`
	MessageID string      `json:"messageID"`
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	Time      PartTime    `json:"time,omitempty"`
	CallID    string      `json:"callID,omitempty"`
	Tool      string      `json:"tool,omitempty"`
	State     *ToolState  `json:"state,omitempty"`
	Cost      float64     `json:"cost,omitempty"`
	Tokens    *StepTokens `json:"tokens,omitempty"`

	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Reason   json.RawMessage `json:"reason,omitempty"`
	Mime     json.RawMessage `json:"mime,omitempty"`
	URL      json.RawMessage `json:"url,omitempty"`
	Filename json.RawMessage `json:"filename,omitempty"`
	Hash     json.RawMessage `json:"hash,omitempty"`
	Files    json.RawMessage `json:"files,omitempty"`
	Attempt  json.RawMessage `json:"attempt,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
	Auto     json.RawMessage `json:"auto,omitempty"`

	jsonutil.Overflow
}

var partKnown = jsonutil.KnownFields(Part{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *Part) UnmarshalJSON(data []byte) error {
	type Alias Part
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Part: %w", err)
	}
	alias := (*Alias)(p)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("Part: %w", err)
	}
	p.Extra = jsonutil.CollectUnknown(raw, partKnown)
	return nil
}

// PartTime holds optional start/end timestamps for a part.
type PartTime struct {
	Start *int64 `json:"start,omitempty"`
	End   *int64 `json:"end,omitempty"`
}

// ToolState describes the state of a tool part.
type ToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	Title    string          `json:"title,omitempty"`
	Time     json.RawMessage `json:"time,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`

	jsonutil.Overflow
}

var toolStateKnown = jsonutil.KnownFields(ToolState{})

// UnmarshalJSON implements json.Unmarshaler.
func (s *ToolState) UnmarshalJSON(data []byte) error {
	type Alias ToolState
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ToolState: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ToolState: %w", err)
	}
	s.Extra = jsonutil.CollectUnknown(raw, toolStateKnown)
	return nil
}

// StepTokens holds token usage for a step-finish part.
type StepTokens struct {
	Total     int         `json:"total"`
	Input     int         `json:"input"`
	Output    int         `json:"output"`
	Reasoning int         `json:"reasoning"`
	Cache     *TokenCache `json:"cache,omitempty"`

	jsonutil.Overflow
}

var stepTokensKnown = jsonutil.KnownFields(StepTokens{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *StepTokens) UnmarshalJSON(data []byte) error {
	type Alias StepTokens
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("StepTokens: %w", err)
	}
	alias := (*Alias)(t)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("StepTokens: %w", err)
	}
	t.Extra = jsonutil.CollectUnknown(raw, stepTokensKnown)
	return nil
}

// TokenCache holds cache read/write token counts.
type TokenCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`

	jsonutil.Overflow
}

var tokenCacheKnown = jsonutil.KnownFields(TokenCache{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *TokenCache) UnmarshalJSON(data []byte) error {
	type Alias TokenCache
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("TokenCache: %w", err)
	}
	alias := (*Alias)(c)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("TokenCache: %w", err)
	}
	c.Extra = jsonutil.CollectUnknown(raw, tokenCacheKnown)
	return nil
}

// PartDeltaRecord wraps a message.part.delta SSE event (streaming text).
//
// Example:
//
//	{"type":"message.part.delta","properties":{"sessionID":"ses_abc","messageID":"msg_1","partID":"prt_1","field":"text","delta":"Hello"}}
type PartDeltaRecord struct {
	Type       string              `json:"type"`
	Properties PartDeltaProperties `json:"properties"`

	jsonutil.Overflow
}

var partDeltaRecordKnown = jsonutil.KnownFields(PartDeltaRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *PartDeltaRecord) UnmarshalJSON(data []byte) error {
	type Alias PartDeltaRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("PartDeltaRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("PartDeltaRecord: %w", err)
	}
	r.Extra = jsonutil.CollectUnknown(raw, partDeltaRecordKnown)
	return nil
}

// PartDeltaProperties contains the delta text within a message.part.delta event.
type PartDeltaProperties struct {
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	PartID    string `json:"partID"`
	Field     string `json:"field"`
	Delta     string `json:"delta"`
}

// AsSessionError decodes the record as a SessionErrorRecord.
func (r *Record) AsSessionError() (*SessionErrorRecord, error) {
	var v SessionErrorRecord
	if err := json.Unmarshal(r.raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SessionErrorRecord wraps a session.error SSE event.
//
// Example:
//
//	{"type":"session.error","properties":{"sessionID":"ses_abc","error":{"name":"UnknownError","data":{"message":"Model not found: ..."}}}}
type SessionErrorRecord struct {
	Type       string                 `json:"type"`
	Properties SessionErrorProperties `json:"properties"`
}

// SessionErrorProperties contains the error details.
type SessionErrorProperties struct {
	SessionID string       `json:"sessionID"`
	Error     SessionError `json:"error"`
}

// SessionError is a kilo error object with a name and optional data.message.
// Known error names: ProviderAuthError, ProviderModelNotFoundError,
// UnknownError, MessageOutputLengthError, MessageAbortedError,
// StructuredOutputError, ContextOverflowError, APIError.
type SessionError struct {
	Name string           `json:"name"`
	Data SessionErrorData `json:"data"`
}

// SessionErrorData holds the error message.
type SessionErrorData struct {
	Message string `json:"message"`
}

// TurnCloseRecord wraps a session.turn.close SSE event.
//
// Example:
//
//	{"type":"session.turn.close","properties":{"sessionID":"ses_abc","reason":"completed"}}
type TurnCloseRecord struct {
	Type       string              `json:"type"`
	Properties TurnCloseProperties `json:"properties"`

	jsonutil.Overflow
}

var turnCloseRecordKnown = jsonutil.KnownFields(TurnCloseRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *TurnCloseRecord) UnmarshalJSON(data []byte) error {
	type Alias TurnCloseRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("TurnCloseRecord: %w", err)
	}
	alias := (*Alias)(r)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("TurnCloseRecord: %w", err)
	}
	r.Extra = jsonutil.CollectUnknown(raw, turnCloseRecordKnown)
	return nil
}

// TurnCloseProperties contains the reason a turn was closed.
type TurnCloseProperties struct {
	SessionID string `json:"sessionID"`
	Reason    string `json:"reason"` // "completed", "error", "interrupted"
}
