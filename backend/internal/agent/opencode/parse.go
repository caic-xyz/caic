// OpenCode ACP parser. Converts ACP's JSON-RPC session/update notifications
// into normalized agent.Message types.
package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ParseMessage decodes a single line from the OpenCode ACP output into one or
// more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
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
//   - UsageMessage         — usage_update
//   - SystemMessage        — current_mode_update
//   - DiffStatMessage      — caic_diff_stat injection
//   - RawMessage           — unrecognised wire types (preserved verbatim)
func ParseMessage(line []byte) ([]agent.Message, error) {
	var probe messageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines have a "type" field.
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
	case MethodSessionUpdate:
		return parseSessionUpdate(msg.Params, line)

	case MethodSessionRequestPermission:
		// Permission requests are handled by wireFormat (auto-approve).
		// In the stateless parser, emit as RawMessage.
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: msg.Method, Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseSessionUpdate dispatches on the sessionUpdate discriminator.
func parseSessionUpdate(params json.RawMessage, line []byte) ([]agent.Message, error) {
	var sup SessionUpdateParams
	if err := json.Unmarshal(params, &sup); err != nil {
		return nil, fmt.Errorf("session/update params: %w", err)
	}

	var probe updateProbe
	if err := json.Unmarshal(sup.Update, &probe); err != nil {
		return nil, fmt.Errorf("session/update probe: %w", err)
	}

	switch probe.SessionUpdate {
	case UpdateAgentMessageChunk:
		var u AgentMessageChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, fmt.Errorf("agent_message_chunk: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: u.Content.Text}}, nil

	case UpdateAgentThoughtChunk:
		var u AgentThoughtChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, fmt.Errorf("agent_thought_chunk: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: u.Content.Text}}, nil

	case UpdateUserMessageChunk:
		var u UserMessageChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, fmt.Errorf("user_message_chunk: %w", err)
		}
		return []agent.Message{&agent.UserInputMessage{Text: u.Content.Text}}, nil

	case UpdateToolCall:
		return parseToolCall(sup.Update)

	case UpdateToolCallUpdate:
		return parseToolCallUpdate(sup.Update)

	case UpdatePlan:
		return parsePlanUpdate(sup.Update)

	case UpdateUsageUpdate:
		var u UsageUpdateUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, fmt.Errorf("usage_update: %w", err)
		}
		return []agent.Message{&agent.UsageMessage{
			ContextWindow: u.Size,
		}}, nil

	case UpdateCurrentModeUpdate:
		var u CurrentModeUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, fmt.Errorf("current_mode_update: %w", err)
		}
		detail := u.ModeName
		if detail == "" {
			detail = u.ModeID
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "mode_update",
			Detail:      detail,
		}}, nil

	case UpdateSessionInfoUpdate:
		return nil, nil // cosmetic, skip

	case UpdateAvailableCommandsUpdate, UpdateConfigOptionUpdate:
		return nil, nil // internal, skip

	default:
		return []agent.Message{&agent.RawMessage{MessageType: "session/update:" + probe.SessionUpdate, Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseToolCall handles tool_call session updates (initial tool announcement).
func parseToolCall(data json.RawMessage) ([]agent.Message, error) {
	var u ToolCallUpdate
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("tool_call: %w", err)
	}

	// Check for widget tool.
	if agent.WidgetToolNames[u.Title] {
		return []agent.Message{agent.NewWidgetMessage(u.ToolCallID, u.RawInput)}, nil
	}

	return []agent.Message{&agent.ToolUseMessage{
		ToolUseID: u.ToolCallID,
		Name:      normalizeToolName(u.Title, u.Kind),
		Input:     u.RawInput,
	}}, nil
}

// parseToolCallUpdate handles tool_call_update session updates (progress/completion).
func parseToolCallUpdate(data json.RawMessage) ([]agent.Message, error) {
	var u ToolCallUpdateUpdate
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("tool_call_update: %w", err)
	}

	switch u.Status {
	case StatusCompleted:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID}}, nil
	case StatusFailed:
		errMsg := extractToolError(&u)
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID, Error: errMsg}}, nil
	case StatusInProgress:
		// Emit output delta if content is available.
		if delta := extractToolOutputDelta(&u); delta != "" {
			return []agent.Message{&agent.ToolOutputDeltaMessage{
				ToolUseID: u.ToolCallID,
				Delta:     delta,
			}}, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// extractToolError extracts the error message from a failed tool call update.
// It checks rawOutput.error first (structured), then falls back to content text.
func extractToolError(u *ToolCallUpdateUpdate) string {
	if u.RawOutput != nil && u.RawOutput.Error != "" {
		return u.RawOutput.Error
	}
	for i := range u.Content {
		if u.Content[i].Type == "content" && u.Content[i].Content.Text != "" {
			return u.Content[i].Content.Text
		}
	}
	return "tool call failed"
}

// extractToolOutputDelta extracts streaming output from an in-progress tool call.
// It checks rawOutput.output first (structured), then falls back to content text.
func extractToolOutputDelta(u *ToolCallUpdateUpdate) string {
	if u.RawOutput != nil && u.RawOutput.Output != "" {
		return u.RawOutput.Output
	}
	for i := range u.Content {
		if u.Content[i].Type == "content" && u.Content[i].Content.Text != "" {
			return u.Content[i].Content.Text
		}
	}
	return ""
}

// parsePlanUpdate converts a plan update to a TodoMessage.
func parsePlanUpdate(data json.RawMessage) ([]agent.Message, error) {
	var u PlanUpdate
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	todos := make([]agent.TodoItem, len(u.Entries))
	for i, e := range u.Entries {
		todos[i] = agent.TodoItem{
			Content: e.Content,
			Status:  e.Status,
		}
	}
	return []agent.Message{&agent.TodoMessage{
		ToolUseID: "plan",
		Todos:     todos,
	}}, nil
}

// normalizeToolName maps OpenCode tool titles and kinds to caic canonical names.
func normalizeToolName(title, kind string) string {
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
	case KindExecute:
		return "Bash"
	case KindEdit:
		return "Edit"
	case KindRead:
		return "Read"
	case KindSearch:
		return "Grep"
	case KindFetch:
		return "WebFetch"
	}

	// Return original title as-is.
	return title
}
