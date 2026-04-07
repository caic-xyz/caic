// OpenCode ACP parser. Converts ACP's JSON-RPC session/update notifications
// into normalized agent.Message types.
package opencode

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	oc "github.com/maruel/genai/providers/opencode"
)

// notificationKnownFields caches the known field sets for output wire types,
// built on first use. Uses sync.Map: few writes (once per type), many reads.
var notificationKnownFields sync.Map

// unmarshalNotification unmarshals data into v and logs a warning for any
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

// parseMessage decodes a single line from the OpenCode ACP output into one or
// more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
//
// Emitted agent.Message types:
//   - InitMessage          — caic_init injection
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
func parseMessage(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var probe oc.MessageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines have a "type" field.
	if probe.Type != "" {
		switch probe.Type {
		case "caic_init":
			var ci caicInit
			if err := json.Unmarshal(line, &ci); err != nil {
				return nil, err
			}
			return []agent.Message{&agent.InitMessage{
				SessionID: ci.SessionID,
				Model:     ci.Model,
				Version:   ci.Version,
			}}, nil
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
	var msg oc.JSONRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	switch msg.Method {
	case oc.MethodSessionUpdate:
		return parseSessionUpdate(msg.Params, line, fw)

	case oc.MethodSessionRequestPermission:
		// Permission requests are handled by wireFormat (auto-approve).
		// In the stateless parser, emit as RawMessage.
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseSessionUpdate dispatches on the sessionUpdate discriminator.
func parseSessionUpdate(params json.RawMessage, line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var sup oc.SessionUpdateParams
	if err := json.Unmarshal(params, &sup); err != nil {
		return nil, fmt.Errorf("session/update params: %w", err)
	}

	var probe oc.UpdateProbe
	if err := json.Unmarshal(sup.Update, &probe); err != nil {
		return nil, fmt.Errorf("session/update probe: %w", err)
	}

	switch probe.SessionUpdate {
	case oc.UpdateAgentMessageChunk:
		var u oc.AgentMessageChunkUpdate
		if err := unmarshalNotification(sup.Update, &u, "AgentMessageChunkUpdate", fw); err != nil {
			return nil, fmt.Errorf("agent_message_chunk: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: u.Content.Text}}, nil

	case oc.UpdateAgentThoughtChunk:
		var u oc.AgentThoughtChunkUpdate
		if err := unmarshalNotification(sup.Update, &u, "AgentThoughtChunkUpdate", fw); err != nil {
			return nil, fmt.Errorf("agent_thought_chunk: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: u.Content.Text}}, nil

	case oc.UpdateUserMessageChunk:
		var u oc.UserMessageChunkUpdate
		if err := unmarshalNotification(sup.Update, &u, "UserMessageChunkUpdate", fw); err != nil {
			return nil, fmt.Errorf("user_message_chunk: %w", err)
		}
		return []agent.Message{&agent.UserInputMessage{Text: u.Content.Text}}, nil

	case oc.UpdateToolCall:
		return parseToolCall(sup.Update, fw)

	case oc.UpdateToolCallUpdate:
		return parseToolCallUpdate(sup.Update, fw)

	case oc.UpdatePlan:
		return parsePlanUpdate(sup.Update, fw)

	case oc.UpdateUsageUpdate:
		var u oc.UsageUpdateUpdate
		if err := unmarshalNotification(sup.Update, &u, "UsageUpdateUpdate", fw); err != nil {
			return nil, fmt.Errorf("usage_update: %w", err)
		}
		return []agent.Message{&agent.UsageMessage{
			ContextWindow: u.Size,
		}}, nil

	case oc.UpdateCurrentModeUpdate:
		var u oc.CurrentModeUpdate
		if err := unmarshalNotification(sup.Update, &u, "CurrentModeUpdate", fw); err != nil {
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

	case oc.UpdateSessionInfoUpdate:
		return nil, nil // cosmetic, skip

	case oc.UpdateAvailableCommandsUpdate, oc.UpdateConfigOptionUpdate:
		return nil, nil // internal, skip

	default:
		return []agent.Message{&agent.RawMessage{MessageType: "session/update:" + string(probe.SessionUpdate), Raw: append([]byte(nil), line...)}}, nil
	}
}

// parseToolCall handles tool_call session updates (initial tool announcement).
func parseToolCall(data json.RawMessage, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var u oc.ToolCallUpdate
	if err := unmarshalNotification(data, &u, "ToolCallUpdate", fw); err != nil {
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
func parseToolCallUpdate(data json.RawMessage, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var u oc.ToolCallUpdateUpdate
	if err := unmarshalNotification(data, &u, "ToolCallUpdateUpdate", fw); err != nil {
		return nil, fmt.Errorf("tool_call_update: %w", err)
	}

	switch u.Status {
	case oc.StatusCompleted:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID}}, nil
	case oc.StatusFailed:
		errMsg := extractToolError(&u)
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID, Error: errMsg}}, nil
	case oc.StatusInProgress:
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
func extractToolError(u *oc.ToolCallUpdateUpdate) string {
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
func extractToolOutputDelta(u *oc.ToolCallUpdateUpdate) string {
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
func parsePlanUpdate(data json.RawMessage, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var u oc.PlanUpdate
	if err := unmarshalNotification(data, &u, "PlanUpdate", fw); err != nil {
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

// normalizeToolName maps OpenCode tool titles and kinds to caic canonical names.
func normalizeToolName(title string, kind oc.ToolKind) string {
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
	case oc.KindExecute:
		return "Bash"
	case oc.KindEdit:
		return "Edit"
	case oc.KindRead:
		return "Read"
	case oc.KindSearch:
		return "Grep"
	case oc.KindFetch:
		return "WebFetch"
	case oc.KindDelete, oc.KindMove, oc.KindThink, oc.KindSwitchMode, oc.KindOther:
		// No mapping; fall through to passthrough.
	}

	// Return original title as-is.
	return title
}
