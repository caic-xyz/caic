package codex

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	cx "github.com/maruel/genai/providers/codex"
)

// notificationKnownFields caches the known field sets for output wire types,
// built on first use. Uses sync.Map: few writes (once per type), many reads.
var notificationKnownFields sync.Map

// unmarshalNotification unmarshals data into v and warns via fw for any
// unknown JSON fields. The name identifies the type for logging.
func unmarshalNotification(data []byte, v any, name string, fw *jsonutil.FieldWarner) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	val, ok := notificationKnownFields.Load(name)
	if !ok {
		val, _ = notificationKnownFields.LoadOrStore(name, jsonutil.KnownFields(reflect.ValueOf(v).Elem().Interface()))
	}
	known := val.(map[string]struct{})
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) == nil {
		fw.Warn(name, jsonutil.CollectUnknown(raw, known))
	}
	return nil
}

// parseMessage decodes a single line from the codex app-server output into one
// or more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
//
// Emitted agent.Message types:
//   - InitMessage          — thread/started
//   - TextMessage          — item/completed agentMessage or plan
//   - TextDeltaMessage     — item/agentMessage/delta
//   - ThinkingMessage      — item/completed reasoning
//   - ThinkingDeltaMessage — item/reasoning/summaryTextDelta
//   - ToolUseMessage       — item/started commandExecution, fileChange, mcpToolCall, dynamicToolCall, collabAgentToolCall
//   - ToolResultMessage    — item/completed commandExecution, fileChange, mcpToolCall, dynamicToolCall, collabAgentToolCall
//   - ToolOutputDeltaMessage — commandExecution/outputDelta, mcpToolCall/progress
//   - SystemMessage        — thread/status/changed, model/rerouted, item/completed contextCompaction
//   - ResultMessage        — turn/completed, error notification
//   - DiffStatMessage      — caic_diff_stat injection
//   - RawMessage           — unrecognised wire types (preserved verbatim)
func parseMessage(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	// Fast probe: check for "type" (caic-injected) vs "method"/"id" (JSON-RPC).
	var probe cx.MessageProbe
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
	var msg cx.JSONRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	switch msg.Method {
	case cx.MethodThreadStarted:
		var p cx.ThreadStartedNotification
		if err := unmarshalNotification(msg.Params, &p, "ThreadStartedNotification", fw); err != nil {
			return nil, fmt.Errorf("thread/started params: %w", err)
		}
		return []agent.Message{&agent.InitMessage{
			SessionID: p.Thread.ID,
			Cwd:       p.Thread.CWD,
			Version:   p.Thread.CLIVersion,
		}}, nil

	case cx.MethodTurnStarted:
		return nil, nil

	case cx.MethodTurnCompleted:
		var p cx.TurnCompletedNotification
		if err := unmarshalNotification(msg.Params, &p, "TurnCompletedNotification", fw); err != nil {
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

	case cx.MethodItemStarted:
		return parseItemStarted(&msg, fw)

	case cx.MethodItemCompleted:
		return parseItemCompleted(&msg, fw)

	case cx.MethodItemUpdated:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil

	case cx.MethodItemDelta:
		var p cx.AgentMessageDeltaNotification
		if err := unmarshalNotification(msg.Params, &p, "AgentMessageDeltaNotification", fw); err != nil {
			return nil, fmt.Errorf("item/agentMessage/delta params: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: p.Delta}}, nil

	case cx.MethodErrorNotification:
		var p cx.ErrorNotification
		if err := unmarshalNotification(msg.Params, &p, "ErrorNotification", fw); err != nil {
			return nil, fmt.Errorf("error notification params: %w", err)
		}
		if p.WillRetry || p.Error == nil {
			return nil, nil
		}
		return []agent.Message{&agent.ResultMessage{
			MessageType: "result",
			Subtype:     "result",
			IsError:     true,
			Result:      p.Error.Message,
		}}, nil

	case cx.MethodReasoningSummaryTextDelta:
		var p cx.ReasoningSummaryTextDeltaNotification
		if err := unmarshalNotification(msg.Params, &p, "ReasoningSummaryTextDeltaNotification", fw); err != nil {
			return nil, fmt.Errorf("item/reasoning/summaryTextDelta params: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: p.Delta}}, nil

	case cx.MethodCommandOutputDelta:
		var p cx.CommandExecutionOutputDeltaNotification
		if err := unmarshalNotification(msg.Params, &p, "CommandExecutionOutputDeltaNotification", fw); err != nil {
			return nil, fmt.Errorf("commandExecution/outputDelta params: %w", err)
		}
		return []agent.Message{&agent.ToolOutputDeltaMessage{ToolUseID: p.ItemID, Delta: p.Delta}}, nil

	case cx.MethodMcpToolCallProgress:
		var p cx.McpToolCallProgressNotification
		if err := unmarshalNotification(msg.Params, &p, "McpToolCallProgressNotification", fw); err != nil {
			return nil, fmt.Errorf("mcpToolCall/progress params: %w", err)
		}
		return []agent.Message{&agent.ToolOutputDeltaMessage{ToolUseID: p.ItemID, Delta: p.Message}}, nil

	case cx.MethodThreadStatusChanged:
		var p cx.ThreadStatusChangedNotification
		if err := unmarshalNotification(msg.Params, &p, "ThreadStatusChangedNotification", fw); err != nil {
			return nil, fmt.Errorf("thread/status/changed params: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     p.Status.Type,
		}}, nil

	case cx.MethodModelRerouted:
		var p cx.ModelReroutedNotification
		if err := unmarshalNotification(msg.Params, &p, "ModelReroutedNotification", fw); err != nil {
			return nil, fmt.Errorf("model/rerouted params: %w", err)
		}
		detail := p.FromModel + " → " + p.ToModel
		if p.Reason != "" {
			detail += " (" + p.Reason + ")"
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "model_rerouted",
			Detail:      detail,
			Model:       p.ToModel,
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseItemStarted handles item/started notifications.
func parseItemStarted(msg *cx.JSONRPCMessage, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var p cx.ItemNotification
	if err := unmarshalNotification(msg.Params, &p, "ItemNotification", fw); err != nil {
		return nil, fmt.Errorf("item/started params: %w", err)
	}
	var h cx.ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/started header: %w", err)
	}
	switch h.Type {
	case cx.ItemTypeCommandExecution:
		var item cx.CommandExecutionItem
		if err := unmarshalNotification(p.Item, &item, "CommandExecutionItem", fw); err != nil {
			return nil, fmt.Errorf("item/started commandExecution: %w", err)
		}
		input, _ := json.Marshal(map[string]string{"command": item.Command, "cwd": item.Cwd})
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      "Bash",
			Input:     input,
		}}, nil

	case cx.ItemTypeFileChange:
		var item cx.FileChangeItem
		if err := unmarshalNotification(p.Item, &item, "FileChangeItem", fw); err != nil {
			return nil, fmt.Errorf("item/started fileChange: %w", err)
		}
		input, _ := json.Marshal(item.Changes)
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      toolNameForChanges(item.Changes),
			Input:     input,
		}}, nil

	case cx.ItemTypeMCPToolCall:
		var item cx.McpToolCallItem
		if err := unmarshalNotification(p.Item, &item, "McpToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/started mcpToolCall: %w", err)
		}
		if agent.WidgetToolNames[item.Tool] {
			return []agent.Message{agent.NewWidgetMessage(item.ID, item.Arguments)}, nil
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      item.Tool,
			Input:     item.Arguments,
		}}, nil

	case cx.ItemTypeDynamicToolCall:
		var item cx.DynamicToolCallItem
		if err := unmarshalNotification(p.Item, &item, "DynamicToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/started dynamicToolCall: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      item.Tool,
			Input:     item.Arguments,
		}}, nil

	case cx.ItemTypeCollabAgentToolCall:
		var item cx.CollabAgentToolCallItem
		if err := unmarshalNotification(p.Item, &item, "CollabAgentToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/started collabAgentToolCall: %w", err)
		}
		toolName := item.Tool
		if toolName == "" {
			toolName = "collabAgent"
		}
		input, _ := json.Marshal(map[string]string{"prompt": item.Prompt})
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      toolName,
			Input:     input,
		}}, nil

	case cx.ItemTypeImageGeneration:
		var item cx.ImageGenerationItem
		if err := unmarshalNotification(p.Item, &item, "ImageGenerationItem", fw); err != nil {
			return nil, fmt.Errorf("item/started imageGeneration: %w", err)
		}
		input, _ := json.Marshal(map[string]string{"revisedPrompt": item.RevisedPrompt})
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      "ImageGeneration",
			Input:     input,
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}

// parseItemCompleted handles item/completed notifications.
func parseItemCompleted(msg *cx.JSONRPCMessage, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var p cx.ItemNotification
	if err := unmarshalNotification(msg.Params, &p, "ItemNotification", fw); err != nil {
		return nil, fmt.Errorf("item/completed params: %w", err)
	}
	var h cx.ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/completed header: %w", err)
	}
	switch h.Type {
	case cx.ItemTypeAgentMessage:
		var item cx.AgentMessageItem
		if err := unmarshalNotification(p.Item, &item, "AgentMessageItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed agentMessage: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text, Phase: item.Phase}}, nil

	case cx.ItemTypeReasoning:
		var item cx.ReasoningItem
		if err := unmarshalNotification(p.Item, &item, "ReasoningItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed reasoning: %w", err)
		}
		text := strings.Join(item.Summary, "\n")
		return []agent.Message{&agent.ThinkingMessage{Text: text}}, nil

	case cx.ItemTypePlan:
		var item cx.PlanItem
		if err := unmarshalNotification(p.Item, &item, "PlanItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed plan: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text}}, nil

	case cx.ItemTypeCommandExecution:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: h.ID}}, nil

	case cx.ItemTypeFileChange:
		var item cx.FileChangeItem
		if err := unmarshalNotification(p.Item, &item, "FileChangeItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed fileChange: %w", err)
		}
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: item.ID}}, nil

	case cx.ItemTypeMCPToolCall:
		var item cx.McpToolCallItem
		if err := unmarshalNotification(p.Item, &item, "McpToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed mcpToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Error != nil {
			m.Error = item.Error.Message
		}
		return []agent.Message{m}, nil

	case cx.ItemTypeDynamicToolCall:
		var item cx.DynamicToolCallItem
		if err := unmarshalNotification(p.Item, &item, "DynamicToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed dynamicToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Success != nil && !*item.Success {
			m.Error = "tool call failed"
		}
		return []agent.Message{m}, nil

	case cx.ItemTypeCollabAgentToolCall:
		var item cx.CollabAgentToolCallItem
		if err := unmarshalNotification(p.Item, &item, "CollabAgentToolCallItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed collabAgentToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Status == "failed" {
			m.Error = "collab agent tool call failed"
		}
		return []agent.Message{m}, nil

	case cx.ItemTypeContextCompaction:
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "context_compaction",
		}}, nil

	case cx.ItemTypeWebSearch:
		var item cx.WebSearchItem
		if err := unmarshalNotification(p.Item, &item, "WebSearchItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed webSearch: %w", err)
		}
		input, _ := json.Marshal(map[string]string{"query": item.Query})
		return []agent.Message{
			&agent.ToolUseMessage{ToolUseID: item.ID, Name: "WebSearch", Input: input},
			&agent.ToolResultMessage{ToolUseID: item.ID},
		}, nil

	case cx.ItemTypeImageGeneration:
		var item cx.ImageGenerationItem
		if err := unmarshalNotification(p.Item, &item, "ImageGenerationItem", fw); err != nil {
			return nil, fmt.Errorf("item/completed imageGeneration: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Status == "failed" {
			m.Error = "image generation failed"
		}
		return []agent.Message{m}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}

// toolNameForChanges returns "Write" if any change has Kind.Type == "add", else "Edit".
func toolNameForChanges(changes []cx.FileUpdateChange) string {
	for _, c := range changes {
		if c.Kind.Type == "add" {
			return "Write"
		}
	}
	return "Edit"
}
