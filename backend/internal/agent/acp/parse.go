// ACP session-update parsing converts native content, tools, plans, and controls into normalized agent messages.

package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// parseMessage decodes a single line from the ACP output into one or
// more typed agent.Messages.
//
// The line is one of:
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
//
// Emitted agent.Message types:
//   - TextDeltaMessage     — agent_message_chunk
//   - ThinkingDeltaMessage — agent_thought_chunk
//   - ToolUseMessage       — tool_call
//   - ToolResultMessage    — tool_call_update (completed/failed)
//   - ToolOutputDeltaMessage — tool_call_update (in_progress with output)
//   - TodoMessage          — plan update
//   - UserInputMessage     — user_message_chunk
//   - SystemMessage        — current_mode_update as a mode notice
//   - RawMessage           — unrecognised wire types (preserved verbatim)
func parseMessage(line []byte, probe *agent.JSONRPCEnvelope) ([]agent.Message, *acp.ToolCallUpdateUpdate, error) {
	// JSON-RPC response (has "id").
	if probe.ID != nil && probe.Method == "" {
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil, nil
	}

	// JSON-RPC notification — dispatch on method.
	msg := acp.JSONRPCMessage{Method: acp.Method(probe.Method), Params: probe.Params}

	switch msg.Method {
	case acp.MethodSessionUpdate:
		msgs, toolCall, err := parseSessionUpdate(msg.Params, line)
		return msgs, toolCall, err

	case acp.MethodSessionRequestPermission:
		// Permission requests are handled by wireFormat (auto-approve).
		// In the stateless parser, emit as RawMessage.
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil, nil
	}
}

// parseSessionUpdate dispatches on the sessionUpdate discriminator.
func parseSessionUpdate(params json.RawMessage, line []byte) ([]agent.Message, *acp.ToolCallUpdateUpdate, error) {
	update, err := agent.JSONField(params, "update")
	if err != nil {
		return nil, nil, fmt.Errorf("session/update params: %w", err)
	}
	if update == nil {
		return nil, nil, errors.New("session/update params: missing update")
	}
	typ, err := agent.JSONString(update, "sessionUpdate")
	if err != nil {
		return nil, nil, fmt.Errorf("session/update probe: %w", err)
	}

	switch acp.UpdateType(typ) {
	case acp.UpdateAgentMessageChunk:
		var u acp.AgentMessageChunkUpdate
		if err := json.Unmarshal(update, &u); err != nil {
			return nil, nil, fmt.Errorf("agent_message_chunk: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: u.Content.Text}}, nil, nil

	case acp.UpdateAgentThoughtChunk:
		var u acp.AgentThoughtChunkUpdate
		if err := json.Unmarshal(update, &u); err != nil {
			return nil, nil, fmt.Errorf("agent_thought_chunk: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: u.Content.Text}}, nil, nil

	case acp.UpdateUserMessageChunk:
		var u acp.UserMessageChunkUpdate
		if err := json.Unmarshal(update, &u); err != nil {
			return nil, nil, fmt.Errorf("user_message_chunk: %w", err)
		}
		return []agent.Message{&agent.UserInputMessage{Text: u.Content.Text}}, nil, nil

	case acp.UpdateToolCall, acp.UpdateToolCallUpdate:
		// The announcement and the update carry the same fields, so decode once
		// and hand the same value to the conversion and to native correlation.
		var u acp.ToolCallUpdateUpdate
		if err := json.Unmarshal(update, &u); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", acp.UpdateType(typ), err)
		}
		var msgs []agent.Message
		if acp.UpdateType(typ) == acp.UpdateToolCall {
			msgs = parseToolCall(&u)
		} else {
			msgs = parseToolCallUpdate(&u)
		}
		return msgs, &u, nil

	case acp.UpdatePlan:
		msgs, err := parsePlanUpdate(update)
		return msgs, nil, err

	case acp.UpdateCurrentModeUpdate:
		var u acp.CurrentModeUpdate
		if err := json.Unmarshal(update, &u); err != nil {
			return nil, nil, fmt.Errorf("current_mode_update: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     agent.SystemSubtypeNotice,
			Detail:      "Mode: " + u.CurrentModeID,
		}}, nil, nil

	case acp.UpdateSessionInfoUpdate:
		return nil, nil, nil // cosmetic, skip

	case acp.UpdateAvailableCommandsUpdate:
		return nil, nil, nil // internal, skip

	default:
		return []agent.Message{&agent.RawMessage{MessageType: "session/update:" + string(acp.UpdateType(typ)), Raw: append([]byte(nil), line...)}}, nil, nil
	}
}

// parseToolCall handles tool_call session updates (initial tool announcement),
// rendering the update its caller decoded.
func parseToolCall(u *acp.ToolCallUpdateUpdate) []agent.Message {
	// Check for widget tool.
	if _, ok := agent.WidgetToolNames[u.Title]; ok {
		return []agent.Message{agent.NewWidgetMessage(u.ToolCallID, u.RawInput)}
	}

	use := &agent.ToolUseMessage{
		ToolUseID: u.ToolCallID,
		Name:      normalizeToolName(u.Title, u.Kind),
		Input:     u.RawInput,
	}
	addEditInputView(use)
	return []agent.Message{use}
}

// parseToolCallUpdate handles tool_call_update session updates (progress/completion),
// rendering the update its caller decoded.
//
// When the update transitions to in_progress, it also emits a ToolUseMessage
// with the real tool input. This is necessary because the initial tool_call
// notification has an empty rawInput ({}); the actual arguments only arrive
// in the tool_call_update.
func parseToolCallUpdate(u *acp.ToolCallUpdateUpdate) []agent.Message {
	switch u.Status {
	case acp.StatusCompleted:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID}}
	case acp.StatusFailed:
		errMsg := ToolError(u)
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID, Error: errMsg}}
	case acp.StatusInProgress:
		var msgs []agent.Message
		// Emit a ToolUseMessage with the real input when available.
		if len(u.RawInput) > 2 {
			name := normalizeToolName(u.Title, u.Kind)
			use := &agent.ToolUseMessage{
				ToolUseID: u.ToolCallID,
				Name:      name,
				Input:     u.RawInput,
			}
			addEditInputView(use)
			msgs = append(msgs, use)
		}
		// Also emit output delta if content is available.
		if delta := ToolOutput(u); delta != "" {
			msgs = append(msgs, &agent.ToolOutputDeltaMessage{
				ToolUseID: u.ToolCallID,
				Delta:     delta,
			})
		}
		return msgs
	default:
		return nil
	}
}

func addEditInputView(use *agent.ToolUseMessage) {
	if use == nil || !strings.EqualFold(use.Name, "Edit") {
		return
	}
	p, replacements, ok := parseEditInput(use.Input)
	if !ok {
		return
	}
	use.Detail = path.Base(p)
	use.InputView = agent.FileChangesInputViewFromReplacements(p, replacements)
}

func parseEditInput(raw json.RawMessage) (string, []agent.TextReplacement, bool) {
	var input acp.EditInput
	if len(raw) == 0 || json.Unmarshal(raw, &input) != nil || input.FilePath == "" || input.OldString == "" {
		return "", nil, false
	}
	return input.FilePath, []agent.TextReplacement{{
		OldText: input.OldString,
		NewText: input.NewString,
	}}, true
}

type toolCallRawOutput struct {
	Error  string `json:"error"`
	Output string `json:"output"`
}

// ToolError returns the native structured error or textual failure content.
func ToolError(u *acp.ToolCallUpdateUpdate) string {
	raw := decodeToolCallRawOutput(u.RawOutput)
	if raw.Error != "" {
		return raw.Error
	}
	for i := range u.Content {
		if u.Content[i].Type == "content" && u.Content[i].Content.Text != "" {
			return u.Content[i].Content.Text
		}
	}
	return "tool call failed"
}

// ToolOutput returns native structured output or textual tool content.
func ToolOutput(u *acp.ToolCallUpdateUpdate) string {
	raw := decodeToolCallRawOutput(u.RawOutput)
	if raw.Output != "" {
		return raw.Output
	}
	for i := range u.Content {
		if u.Content[i].Type == "content" && u.Content[i].Content.Text != "" {
			return u.Content[i].Content.Text
		}
	}
	return ""
}

func decodeToolCallRawOutput(data json.RawMessage) toolCallRawOutput {
	var raw toolCallRawOutput
	_ = json.Unmarshal(data, &raw)
	return raw
}

// parsePlanUpdate converts a plan update to a TodoMessage.
func parsePlanUpdate(data json.RawMessage) ([]agent.Message, error) {
	var u acp.PlanUpdate
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	todos := make([]agent.TodoItem, len(u.Entries))
	for i, e := range u.Entries {
		todos[i] = agent.TodoItem{
			Content: e.Content,
			Status:  string(e.Status),
		}
	}
	return []agent.Message{&agent.TodoMessage{
		ToolUseID: "plan",
		Todos:     todos,
	}}, nil
}

// normalizeToolName maps native tool titles and kinds to caic canonical names.
func normalizeToolName(title string, kind acp.ToolKind) string {
	// Normalize to lowercase for matching.
	lower := strings.ToLower(title)

	// Direct name mappings.
	switch lower {
	case "bash", "shell", "terminal":
		return "Bash"
	case "edit", "replace":
		return "Edit"
	case "write", "write_file":
		return "Write"
	case "read", "read_file":
		return "Read"
	case "glob", "find_files":
		return "Glob"
	case "grep", "search", "grep_search":
		return "Grep"
	case "list", "list_directory", "ls":
		return "ListDirectory"
	case "webfetch", "web_fetch":
		return "WebFetch"
	case "websearch", "web_search", "google_web_search":
		return "WebSearch"
	case "todowrite", "todo_write":
		return "TodoWrite"
	case "task":
		return "Agent"
	case "patch":
		return "Edit"
	}

	// Fall back to kind-based mapping.
	switch kind {
	case acp.KindExecute:
		return "Bash"
	case acp.KindEdit:
		return "Edit"
	case acp.KindRead:
		return "Read"
	case acp.KindSearch:
		return "Grep"
	case acp.KindFetch:
		return "WebFetch"
	case acp.KindDelete, acp.KindMove, acp.KindThink, acp.KindSwitchMode, acp.KindOther:
		// No mapping; fall through to passthrough.
	}

	// Return original title as-is.
	return title
}
