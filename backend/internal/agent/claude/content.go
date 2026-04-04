package claude

import (
	"encoding/json"

	"fmt"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// ContentBlock is a single block within a message's content array.
// The Type field discriminates between variants.
type ContentBlock struct {
	Type string `json:"type"`

	// Text block (type="text").
	Text string `json:"text,omitempty"`

	// Thinking block (type="thinking").
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Tool use block (type="tool_use").
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// Tool result block (type="tool_result").
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`

	jsonutil.Overflow
}

var contentBlockKnown = jsonutil.KnownFields(ContentBlock{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *ContentBlock) UnmarshalJSON(data []byte) error {
	type Alias ContentBlock
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ContentBlock: %w", err)
	}
	// Unmarshal known fields via alias to avoid recursion.
	alias := (*Alias)(c)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ContentBlock: %w", err)
	}
	c.Extra = jsonutil.CollectUnknown(raw, contentBlockKnown)
	return nil
}

// Usage tracks token consumption for an API call.
type Usage struct {
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens"`
	ServiceTier              string          `json:"service_tier"`
	InferenceGeo             string          `json:"inference_geo,omitempty"`
	Iterations               json.RawMessage `json:"iterations,omitempty"` // int or array

	ServerToolUse *ServerToolUse `json:"server_tool_use,omitempty"`
	CacheCreation *CacheCreation `json:"cache_creation,omitempty"`

	jsonutil.Overflow
}

var usageKnown = jsonutil.KnownFields(Usage{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type Alias Usage
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Usage: %w", err)
	}
	alias := (*Alias)(u)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("Usage: %w", err)
	}
	u.Extra = jsonutil.CollectUnknown(raw, usageKnown)
	return nil
}

// ServerToolUse tracks server-side tool use counts.
type ServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
	WebFetchRequests  int `json:"web_fetch_requests"`

	jsonutil.Overflow
}

var serverToolUseKnown = jsonutil.KnownFields(ServerToolUse{})

// UnmarshalJSON implements json.Unmarshaler.
func (s *ServerToolUse) UnmarshalJSON(data []byte) error {
	type Alias ServerToolUse
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ServerToolUse: %w", err)
	}
	alias := (*Alias)(s)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ServerToolUse: %w", err)
	}
	s.Extra = jsonutil.CollectUnknown(raw, serverToolUseKnown)
	return nil
}

// CacheCreation breaks down cache creation by time bucket.
type CacheCreation struct {
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`

	jsonutil.Overflow
}

var cacheCreationKnown = jsonutil.KnownFields(CacheCreation{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *CacheCreation) UnmarshalJSON(data []byte) error {
	type Alias CacheCreation
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("CacheCreation: %w", err)
	}
	alias := (*Alias)(c)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("CacheCreation: %w", err)
	}
	c.Extra = jsonutil.CollectUnknown(raw, cacheCreationKnown)
	return nil
}

// APIMessage is the Anthropic API message object embedded in assistant records.
type APIMessage struct {
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	Model             string          `json:"model"`
	Role              string          `json:"role"`
	Content           []ContentBlock  `json:"content"`
	StopReason        *string         `json:"stop_reason"`
	StopSequence      *string         `json:"stop_sequence"`
	Usage             *Usage          `json:"usage,omitempty"`
	Container         json.RawMessage `json:"container,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`

	jsonutil.Overflow
}

var apiMessageKnown = jsonutil.KnownFields(APIMessage{})

// UnmarshalJSON implements json.Unmarshaler.
func (m *APIMessage) UnmarshalJSON(data []byte) error {
	type Alias APIMessage
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("APIMessage: %w", err)
	}
	alias := (*Alias)(m)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("APIMessage: %w", err)
	}
	m.Extra = jsonutil.CollectUnknown(raw, apiMessageKnown)
	return nil
}

// UserMessage is the message payload inside a user record.
// Content can be a plain string or an array of content blocks (tool results).
type UserMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock

	jsonutil.Overflow
}

var userMessageKnown = jsonutil.KnownFields(UserMessage{})

// UnmarshalJSON implements json.Unmarshaler.
func (m *UserMessage) UnmarshalJSON(data []byte) error {
	type Alias UserMessage
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("UserMessage: %w", err)
	}
	alias := (*Alias)(m)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("UserMessage: %w", err)
	}
	m.Extra = jsonutil.CollectUnknown(raw, userMessageKnown)
	return nil
}

// ContentText returns the content as a plain string.
// Returns ("", false) if content is an array.
func (m *UserMessage) ContentText() (string, bool) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, true
	}
	return "", false
}

// ContentBlocks returns the content as a slice of ContentBlock.
// Returns (nil, false) if content is a plain string.
func (m *UserMessage) ContentBlocks() ([]ContentBlock, bool) {
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks, true
	}
	return nil, false
}

// Todo is a task-tracking entry.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`

	jsonutil.Overflow
}

var todoKnown = jsonutil.KnownFields(Todo{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *Todo) UnmarshalJSON(data []byte) error {
	type Alias Todo
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Todo: %w", err)
	}
	alias := (*Alias)(t)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("Todo: %w", err)
	}
	t.Extra = jsonutil.CollectUnknown(raw, todoKnown)
	return nil
}

// ThinkingMetadata controls extended thinking parameters.
type ThinkingMetadata struct {
	MaxThinkingTokens int             `json:"maxThinkingTokens"`
	Disabled          bool            `json:"disabled,omitempty"`
	Level             string          `json:"level,omitempty"`
	Triggers          json.RawMessage `json:"triggers,omitempty"`

	jsonutil.Overflow
}

var thinkingMetadataKnown = jsonutil.KnownFields(ThinkingMetadata{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *ThinkingMetadata) UnmarshalJSON(data []byte) error {
	type Alias ThinkingMetadata
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ThinkingMetadata: %w", err)
	}
	alias := (*Alias)(t)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ThinkingMetadata: %w", err)
	}
	t.Extra = jsonutil.CollectUnknown(raw, thinkingMetadataKnown)
	return nil
}

// ToolUseResult holds summary data from a completed subagent tool invocation.
type ToolUseResult struct {
	Status            string          `json:"status"`
	Prompt            string          `json:"prompt"`
	AgentID           string          `json:"agentId"`
	Content           json.RawMessage `json:"content,omitempty"`
	TotalDurationMs   int64           `json:"totalDurationMs"`
	TotalTokens       int             `json:"totalTokens"`
	TotalToolUseCount int             `json:"totalToolUseCount"`
	Usage             *Usage          `json:"usage,omitempty"`

	jsonutil.Overflow
}

var toolUseResultKnown = jsonutil.KnownFields(ToolUseResult{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *ToolUseResult) UnmarshalJSON(data []byte) error {
	type Alias ToolUseResult
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ToolUseResult: %w", err)
	}
	alias := (*Alias)(t)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ToolUseResult: %w", err)
	}
	t.Extra = jsonutil.CollectUnknown(raw, toolUseResultKnown)
	return nil
}
