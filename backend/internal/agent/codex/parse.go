package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ParseMessage decodes a single line from the codex app-server output into one
// or more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
func ParseMessage(line []byte) ([]agent.Message, error) {
	// Fast probe: check for "type" (caic-injected) vs "method"/"id" (JSON-RPC).
	var probe messageProbe
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
			return []agent.Message{&m}, nil
		default:
			return []agent.Message{&agent.RawMessage{MessageType: probe.Type, Raw: append([]byte(nil), line...)}}, nil
		}
	}

	// JSON-RPC response (has "id").
	if probe.ID != nil {
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil
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
		return []agent.Message{&agent.InitMessage{
			SessionID: p.Thread.ID,
			Cwd:       p.Thread.CWD,
			Version:   p.Thread.CLIVersion,
		}}, nil

	case MethodTurnStarted:
		return nil, nil

	case MethodTurnCompleted:
		var p TurnCompletedParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("turn/completed params: %w", err)
		}
		switch p.Turn.Status {
		case "failed", "interrupted":
			errMsg := ""
			if p.Turn.Error != nil {
				errMsg = p.Turn.Error.Message
			}
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "result",
				IsError:     true,
				Result:      errMsg,
			}}, nil
		default: // "completed", "inProgress"
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "result",
			}}, nil
		}

	case MethodItemStarted:
		return parseItemStarted(&msg)

	case MethodItemCompleted:
		return parseItemCompleted(&msg)

	case MethodItemUpdated:
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}}, nil

	case MethodItemDelta:
		var p ItemDeltaParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("item/agentMessage/delta params: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: p.Delta}}, nil

	case MethodReasoningSummaryTextDelta:
		var p ReasoningSummaryTextDeltaParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("item/reasoning/summaryTextDelta params: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: p.Delta}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseItemStarted handles item/started notifications.
func parseItemStarted(msg *JSONRPCMessage) ([]agent.Message, error) {
	var p ItemParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/started params: %w", err)
	}
	var h ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/started header: %w", err)
	}
	switch h.Type {
	case ItemTypeCommandExecution:
		var item CommandExecutionItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started commandExecution: %w", err)
		}
		input, _ := json.Marshal(map[string]string{"command": item.Command})
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      "Bash",
			Input:     input,
		}}, nil

	case ItemTypeMCPToolCall:
		var item McpToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started mcpToolCall: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      item.Tool,
			Input:     item.Arguments,
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}

// parseItemCompleted handles item/completed notifications.
func parseItemCompleted(msg *JSONRPCMessage) ([]agent.Message, error) {
	var p ItemParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/completed params: %w", err)
	}
	var h ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/completed header: %w", err)
	}
	switch h.Type {
	case ItemTypeAgentMessage:
		var item AgentMessageItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed agentMessage: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text}}, nil

	case ItemTypeReasoning:
		var item ReasoningItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed reasoning: %w", err)
		}
		text := strings.Join(item.Summary, "\n")
		return []agent.Message{&agent.ThinkingMessage{Text: text}}, nil

	case ItemTypePlan:
		var item PlanItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed plan: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text}}, nil

	case ItemTypeCommandExecution:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: h.ID}}, nil

	case ItemTypeFileChange:
		var item FileChangeItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed fileChange: %w", err)
		}
		toolName := "Edit"
		for _, c := range item.Changes {
			if c.Kind.Type == "add" {
				toolName = "Write"
				break
			}
		}
		input, _ := json.Marshal(item.Changes)
		return []agent.Message{
			&agent.ToolUseMessage{ToolUseID: item.ID, Name: toolName, Input: input},
			&agent.ToolResultMessage{ToolUseID: item.ID},
		}, nil

	case ItemTypeMCPToolCall:
		var item McpToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed mcpToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Error != nil {
			m.Error = item.Error.Message
		}
		return []agent.Message{m}, nil

	case ItemTypeWebSearch:
		var item WebSearchItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed webSearch: %w", err)
		}
		input, _ := json.Marshal(map[string]string{"query": item.Query})
		return []agent.Message{
			&agent.ToolUseMessage{ToolUseID: item.ID, Name: "WebSearch", Input: input},
			&agent.ToolResultMessage{ToolUseID: item.ID},
		}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}
