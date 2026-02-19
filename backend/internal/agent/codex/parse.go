package codex

import (
	"encoding/json"
	"fmt"

	"github.com/maruel/caic/backend/internal/agent"
)

// ParseMessage decodes a single line from the codex app-server output into a
// typed agent.Message.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
func ParseMessage(line []byte) (agent.Message, error) {
	// Fast probe: check for "type" (caic-injected) vs "method"/"id" (JSON-RPC).
	var probe struct {
		Type   string           `json:"type,omitempty"`
		Method string           `json:"method,omitempty"`
		ID     *json.RawMessage `json:"id,omitempty"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines have a "type" field (not "jsonrpc").
	if probe.Type != "" {
		switch probe.Type {
		case "caic_diff_stat":
			var m agent.DiffStatMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return &m, nil
		default:
			return &agent.RawMessage{MessageType: probe.Type, Raw: append([]byte(nil), line...)}, nil
		}
	}

	// JSON-RPC response (has "id").
	if probe.ID != nil {
		return &agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}, nil
	}

	// JSON-RPC notification — dispatch on method.
	var msg JSONRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	switch msg.Method {
	case MethodThreadStarted:
		var p ThreadStartedParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("thread/started params: %w", err)
		}
		return &agent.SystemInitMessage{
			MessageType: "system",
			Subtype:     "init",
			SessionID:   p.Thread.ID,
		}, nil

	case MethodTurnStarted:
		return &agent.SystemMessage{
			MessageType: "system",
			Subtype:     "turn_started",
		}, nil

	case MethodTurnCompleted:
		var p TurnCompletedParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("turn/completed params: %w", err)
		}
		if p.Turn.Status == "failed" {
			return &agent.ResultMessage{
				MessageType: "result",
				Subtype:     "result",
				IsError:     true,
				Result:      p.Turn.Error,
			}, nil
		}
		return &agent.ResultMessage{
			MessageType: "result",
			Subtype:     "result",
			Usage: agent.Usage{
				InputTokens:          p.Turn.Usage.InputTokens,
				OutputTokens:         p.Turn.Usage.OutputTokens,
				CacheReadInputTokens: p.Turn.Usage.CachedInputTokens,
			},
		}, nil

	case MethodItemStarted:
		return parseItemStarted(&msg)

	case MethodItemCompleted:
		return parseItemCompleted(&msg)

	case MethodItemUpdated:
		return &agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}, nil

	case MethodItemDelta:
		var p ItemDeltaParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("item/agentMessage/delta params: %w", err)
		}
		return &agent.StreamEvent{
			MessageType: "stream_event",
			Event: agent.StreamEventData{
				Type: "content_block_delta",
				Delta: &agent.StreamDelta{
					Type: "text_delta",
					Text: p.Delta,
				},
			},
		}, nil

	default:
		return &agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}, nil
	}
}

// parseItemStarted handles item/started notifications.
func parseItemStarted(msg *JSONRPCMessage) (agent.Message, error) {
	var p ItemParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/started params: %w", err)
	}
	switch p.Item.Type {
	case ItemTypeCommandExecution:
		input, _ := json.Marshal(map[string]string{"command": p.Item.Command})
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    p.Item.ID,
					Name:  "Bash",
					Input: input,
				}},
			},
		}, nil

	case ItemTypeMCPToolCall:
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    p.Item.ID,
					Name:  p.Item.Tool,
					Input: p.Item.Arguments,
				}},
			},
		}, nil

	default:
		return &agent.RawMessage{MessageType: msg.Method, Raw: append(msg.Params[:0:0], msg.Params...)}, nil
	}
}

// parseItemCompleted handles item/completed notifications.
func parseItemCompleted(msg *JSONRPCMessage) (agent.Message, error) {
	var p ItemParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/completed params: %w", err)
	}
	switch p.Item.Type {
	case ItemTypeAgentMessage:
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type: "text",
					Text: p.Item.Text,
				}},
			},
		}, nil

	case ItemTypeReasoning:
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type: "text",
					Text: p.Item.Text,
				}},
			},
		}, nil

	case ItemTypeCommandExecution:
		raw, _ := json.Marshal(p.Item.AggregatedOutput)
		return &agent.UserMessage{
			MessageType:     "user",
			Message:         raw,
			ParentToolUseID: &p.Item.ID,
		}, nil

	case ItemTypeFileChange:
		toolName := "Edit"
		for _, c := range p.Item.Changes {
			if c.Kind == "add" {
				toolName = "Write"
				break
			}
		}
		input, _ := json.Marshal(p.Item.Changes)
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    p.Item.ID,
					Name:  toolName,
					Input: input,
				}},
			},
		}, nil

	case ItemTypeMCPToolCall:
		content := p.Item.Result
		if p.Item.Error != "" {
			content = p.Item.Error
		}
		raw, _ := json.Marshal(content)
		return &agent.UserMessage{
			MessageType:     "user",
			Message:         raw,
			ParentToolUseID: &p.Item.ID,
		}, nil

	case ItemTypeWebSearch:
		input, _ := json.Marshal(map[string]string{"query": p.Item.Query})
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    p.Item.ID,
					Name:  "WebSearch",
					Input: input,
				}},
			},
		}, nil

	case ItemTypeTodoList:
		input, _ := json.Marshal(p.Item.Items)
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    p.Item.ID,
					Name:  "TodoWrite",
					Input: input,
				}},
			},
		}, nil

	case ItemTypeError:
		return &agent.RawMessage{MessageType: "error", Raw: append(msg.Params[:0:0], msg.Params...)}, nil

	default:
		return &agent.RawMessage{MessageType: msg.Method, Raw: append(msg.Params[:0:0], msg.Params...)}, nil
	}
}
