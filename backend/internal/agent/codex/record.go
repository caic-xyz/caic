package codex

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC notification method constants for codex app-server.
const (
	MethodThreadStarted = "thread/started"
	MethodTurnStarted   = "turn/started"
	MethodTurnCompleted = "turn/completed"
	MethodItemStarted   = "item/started"
	MethodItemCompleted = "item/completed"
	MethodItemUpdated   = "item/updated"
	MethodItemDelta     = "item/agentMessage/delta"
)

// Item type constants for the inner item object.
const (
	ItemTypeAgentMessage     = "agent_message"
	ItemTypeReasoning        = "reasoning"
	ItemTypeCommandExecution = "command_execution"
	ItemTypeFileChange       = "file_change"
	ItemTypeMCPToolCall      = "mcp_tool_call"
	ItemTypeWebSearch        = "web_search"
	ItemTypeTodoList         = "todo_list"
	ItemTypeError            = "error"
)

// JSONRPCMessage is the JSON-RPC 2.0 envelope for codex app-server messages.
// Notifications have Method set and ID nil. Responses have ID set.
type JSONRPCMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *JSONRPCError    `json:"error,omitempty"`
}

// IsResponse returns true if this is a response (has an ID).
func (m *JSONRPCMessage) IsResponse() bool { return m.ID != nil }

// JSONRPCError is a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ThreadStartedParams holds the params for thread/started notifications.
type ThreadStartedParams struct {
	Thread ThreadInfo `json:"thread"`

	Overflow
}

var threadStartedParamsKnown = makeSet("thread")

// UnmarshalJSON implements json.Unmarshaler.
func (p *ThreadStartedParams) UnmarshalJSON(data []byte) error {
	type Alias ThreadStartedParams
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ThreadStartedParams: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(p)); err != nil {
		return fmt.Errorf("ThreadStartedParams: %w", err)
	}
	p.Extra = collectUnknown(raw, threadStartedParamsKnown)
	warnUnknown("ThreadStartedParams", p.Extra)
	return nil
}

// ThreadInfo describes a thread in thread/started params.
type ThreadInfo struct {
	ID string `json:"id"`

	Overflow
}

var threadInfoKnown = makeSet("id")

// UnmarshalJSON implements json.Unmarshaler.
func (t *ThreadInfo) UnmarshalJSON(data []byte) error {
	type Alias ThreadInfo
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ThreadInfo: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(t)); err != nil {
		return fmt.Errorf("ThreadInfo: %w", err)
	}
	t.Extra = collectUnknown(raw, threadInfoKnown)
	warnUnknown("ThreadInfo", t.Extra)
	return nil
}

// TurnCompletedParams holds the params for turn/completed notifications.
type TurnCompletedParams struct {
	Turn TurnCompletedInfo `json:"turn"`

	Overflow
}

var turnCompletedParamsKnown = makeSet("turn")

// UnmarshalJSON implements json.Unmarshaler.
func (p *TurnCompletedParams) UnmarshalJSON(data []byte) error {
	type Alias TurnCompletedParams
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("TurnCompletedParams: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(p)); err != nil {
		return fmt.Errorf("TurnCompletedParams: %w", err)
	}
	p.Extra = collectUnknown(raw, turnCompletedParamsKnown)
	warnUnknown("TurnCompletedParams", p.Extra)
	return nil
}

// TurnCompletedInfo describes a completed turn.
type TurnCompletedInfo struct {
	Status string    `json:"status"` // "completed" or "failed"
	Usage  TurnUsage `json:"usage"`
	Error  string    `json:"error,omitempty"`

	Overflow
}

var turnCompletedInfoKnown = makeSet("status", "usage", "error")

// UnmarshalJSON implements json.Unmarshaler.
func (t *TurnCompletedInfo) UnmarshalJSON(data []byte) error {
	type Alias TurnCompletedInfo
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("TurnCompletedInfo: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(t)); err != nil {
		return fmt.Errorf("TurnCompletedInfo: %w", err)
	}
	t.Extra = collectUnknown(raw, turnCompletedInfoKnown)
	warnUnknown("TurnCompletedInfo", t.Extra)
	return nil
}

// TurnUsage contains token counts for a single turn.
type TurnUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`

	Overflow
}

var turnUsageKnown = makeSet("input_tokens", "cached_input_tokens", "output_tokens")

// UnmarshalJSON implements json.Unmarshaler.
func (u *TurnUsage) UnmarshalJSON(data []byte) error {
	type Alias TurnUsage
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("TurnUsage: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(u)); err != nil {
		return fmt.Errorf("TurnUsage: %w", err)
	}
	u.Extra = collectUnknown(raw, turnUsageKnown)
	warnUnknown("TurnUsage", u.Extra)
	return nil
}

// ItemParams holds the params for item/* notifications.
type ItemParams struct {
	Item ItemData `json:"item"`

	Overflow
}

var itemParamsKnown = makeSet("item")

// UnmarshalJSON implements json.Unmarshaler.
func (p *ItemParams) UnmarshalJSON(data []byte) error {
	type Alias ItemParams
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ItemParams: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(p)); err != nil {
		return fmt.Errorf("ItemParams: %w", err)
	}
	p.Extra = collectUnknown(raw, itemParamsKnown)
	warnUnknown("ItemParams", p.Extra)
	return nil
}

// ItemDeltaParams holds the params for item/agentMessage/delta notifications.
type ItemDeltaParams struct {
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`

	Overflow
}

var itemDeltaParamsKnown = makeSet("item_id", "delta")

// UnmarshalJSON implements json.Unmarshaler.
func (p *ItemDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias ItemDeltaParams
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ItemDeltaParams: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(p)); err != nil {
		return fmt.Errorf("ItemDeltaParams: %w", err)
	}
	p.Extra = collectUnknown(raw, itemDeltaParamsKnown)
	warnUnknown("ItemDeltaParams", p.Extra)
	return nil
}

// ItemData is the inner item object within item event params.
type ItemData struct {
	ID     string `json:"id"`
	Type   string `json:"type"`   // agent_message, reasoning, command_execution, file_change, mcp_tool_call, web_search, todo_list, error
	Status string `json:"status"` // in_progress, completed, failed

	// agent_message / reasoning fields.
	Text string `json:"text,omitempty"`

	// command_execution fields.
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`

	// file_change fields.
	Changes []FileChange `json:"changes,omitempty"`

	// mcp_tool_call fields.
	Server    string          `json:"server,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`

	// web_search fields.
	Query string `json:"query,omitempty"`

	// todo_list fields.
	Items []TodoItem `json:"items,omitempty"`

	// error fields (when type=error).
	Message string `json:"message,omitempty"`

	Overflow
}

var itemDataKnown = makeSet(
	"id", "type", "status", "text",
	"command", "aggregated_output", "exit_code",
	"changes",
	"server", "tool", "arguments", "result", "error",
	"query",
	"items",
	"message",
)

// UnmarshalJSON implements json.Unmarshaler.
func (d *ItemData) UnmarshalJSON(data []byte) error {
	type Alias ItemData
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ItemData: %w", err)
	}
	if err := json.Unmarshal(data, (*Alias)(d)); err != nil {
		return fmt.Errorf("ItemData: %w", err)
	}
	d.Extra = collectUnknown(raw, itemDataKnown)
	warnUnknown("ItemData("+d.Type+")", d.Extra)
	return nil
}

// FileChange describes a single file change within a file_change item.
type FileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // add, update, delete
}

// TodoItem is a single entry in a todo_list item.
type TodoItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}
