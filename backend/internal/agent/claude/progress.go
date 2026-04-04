package claude

import (
	"encoding/json"

	"fmt"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// ProgressRecord reports progress during tool execution.
type ProgressRecord struct {
	MessageFields

	Data            json.RawMessage `json:"data"`
	ParentToolUseID string          `json:"parentToolUseID,omitempty"`
	ToolUseID       string          `json:"toolUseID,omitempty"`

	jsonutil.Overflow
}

var progressRecordKnown = jsonutil.KnownFields(ProgressRecord{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ProgressRecord) UnmarshalJSON(data []byte) error {
	type Alias ProgressRecord
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ProgressRecord: %w", err)
	}
	alias := (*Alias)(p)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ProgressRecord: %w", err)
	}
	p.Extra = jsonutil.CollectUnknown(raw, progressRecordKnown)
	return nil
}

// ProgressData returns the typed progress data.
// Returns the concrete type based on the "type" field in the data JSON.
func (p *ProgressRecord) ProgressData() (*ProgressPayload, error) {
	var v ProgressPayload
	if err := json.Unmarshal(p.Data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ProgressPayload is the data field inside a progress record.
// The Type field discriminates variants: hook_progress, bash_progress,
// agent_progress, query_update, search_results_received, waiting_for_task.
type ProgressPayload struct {
	Type string `json:"type"`

	// hook_progress fields.
	HookEvent string `json:"hookEvent,omitempty"`
	HookName  string `json:"hookName,omitempty"`
	Command   string `json:"command,omitempty"`

	// bash_progress fields.
	Output             string  `json:"output,omitempty"`
	FullOutput         string  `json:"fullOutput,omitempty"`
	TotalLines         int     `json:"totalLines,omitempty"`
	ElapsedTimeSeconds float64 `json:"elapsedTimeSeconds,omitempty"`
	TimeoutMs          int     `json:"timeoutMs,omitempty"`

	// agent_progress fields.
	AgentID            string          `json:"agentId,omitempty"`
	Message            string          `json:"message,omitempty"`
	Prompt             string          `json:"prompt,omitempty"`
	NormalizedMessages json.RawMessage `json:"normalizedMessages,omitempty"`

	// query_update / search_results_received fields.
	Query       string `json:"query,omitempty"`
	ResultCount int    `json:"resultCount,omitempty"`

	// waiting_for_task fields.
	TaskDescription string `json:"taskDescription,omitempty"`
	TaskType        string `json:"taskType,omitempty"`

	jsonutil.Overflow
}

var progressPayloadKnown = jsonutil.KnownFields(ProgressPayload{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ProgressPayload) UnmarshalJSON(data []byte) error {
	type Alias ProgressPayload
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ProgressPayload: %w", err)
	}
	alias := (*Alias)(p)
	if err := json.Unmarshal(data, alias); err != nil {
		return fmt.Errorf("ProgressPayload: %w", err)
	}
	p.Extra = jsonutil.CollectUnknown(raw, progressPayloadKnown)
	return nil
}
