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
	Gemini Harness = "gemini"
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

// Message is the interface for all Claude Code streaming JSON messages.
type Message interface {
	// Type returns the message type string.
	Type() string
}

// SystemInitMessage is emitted at session start (type=system, subtype=init).
type SystemInitMessage struct {
	MessageType string   `json:"type"`
	Subtype     string   `json:"subtype"`
	Cwd         string   `json:"cwd"`
	SessionID   string   `json:"session_id"`
	Tools       []string `json:"tools"`
	Model       string   `json:"model"`
	Version     string   `json:"claude_code_version"`
	UUID        string   `json:"uuid"`
}

// Type implements Message.
func (m *SystemInitMessage) Type() string { return "system" }

// SystemMessage is a generic system message (status, compact_boundary, etc.).
type SystemMessage struct {
	MessageType string `json:"type"`
	Subtype     string `json:"subtype"`
	SessionID   string `json:"session_id"`
	UUID        string `json:"uuid"`
}

// Type implements Message.
func (m *SystemMessage) Type() string { return "system" }

// AssistantMessage contains model responses (text or tool_use blocks).
type AssistantMessage struct {
	MessageType     string     `json:"type"`
	Message         APIMessage `json:"message"`
	ParentToolUseID *string    `json:"parent_tool_use_id"`
	SessionID       string     `json:"session_id"`
	UUID            string     `json:"uuid"`
}

// Type implements Message.
func (m *AssistantMessage) Type() string { return "assistant" }

// APIMessage is the Anthropic API message structure.
type APIMessage struct {
	Model        string         `json:"model"`
	ID           string         `json:"id"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// ContentBlock is a single block in a message's content array.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Usage tracks per-API-call token consumption as reported by the Anthropic API.
//
// The three input token fields are disjoint; total input context for one call
// equals InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// InputTokens is only the small non-cached, non-cache-creation portion
// (typically single-digit). The bulk of the input context lands in cache
// fields.
//
// In ResultMessage these values are per-query (one API round-trip).
// Task.liveUsage sums them across all queries for cumulative totals.
type Usage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	ServiceTier              string `json:"service_tier"`
}

// UserMessage contains tool results fed back by Claude Code.
type UserMessage struct {
	MessageType     string          `json:"type"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	SessionID       string          `json:"session_id"`
	UUID            string          `json:"uuid"`
}

// Type implements Message.
func (m *UserMessage) Type() string { return "user" }

// ResultMessage is the terminal message for a query.
type ResultMessage struct {
	MessageType   string          `json:"type"`
	Subtype       string          `json:"subtype"`
	IsError       bool            `json:"is_error"`
	DurationMs    int64           `json:"duration_ms"`
	DurationAPIMs int64           `json:"duration_api_ms"`
	NumTurns      int             `json:"num_turns"`
	Result        string          `json:"result"`
	StopReason    *string         `json:"stop_reason"`
	SessionID     string          `json:"session_id"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	Usage         Usage           `json:"usage"`
	ModelUsage    json.RawMessage `json:"modelUsage"`
	UUID          string          `json:"uuid"`
	DiffStat      DiffStat        `json:"diff_stat,omitzero"` // Set by caic after running container diff.
}

// Type implements Message.
func (m *ResultMessage) Type() string { return "result" }

// RawMessage is a pass-through for message types we don't need to inspect
// (stream_event, tool_progress, etc.).
type RawMessage struct {
	MessageType string
	Raw         []byte
}

// Type implements Message.
func (m *RawMessage) Type() string { return m.MessageType }

// MetaMessage is written as the first line of a JSONL log file. It captures
// task-level metadata so logs can be reloaded on restart.
type MetaMessage struct {
	MessageType string    `json:"type"`
	Version     int       `json:"version"`
	Prompt      string    `json:"prompt"`
	Repo        string    `json:"repo"`
	Branch      string    `json:"branch"`
	Harness     Harness   `json:"harness"`
	Model       string    `json:"model,omitempty"`
	StartedAt   time.Time `json:"started_at"`
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
	if m.Repo == "" {
		return errors.New("missing repo")
	}
	if m.Branch == "" {
		return errors.New("missing branch")
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
	CostUSD                  float64  `json:"cost_usd,omitempty"`
	DurationMs               int64    `json:"duration_ms,omitempty"`
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

// MarshalMessage serializes a Message to JSON. For RawMessage, returns the
// original bytes to preserve unknown fields. For typed messages, uses
// json.Marshal.
func MarshalMessage(m Message) ([]byte, error) {
	if rm, ok := m.(*RawMessage); ok {
		return rm.Raw, nil
	}
	return json.Marshal(m)
}
