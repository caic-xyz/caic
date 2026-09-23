// OpenCode ACP parser. Converts ACP's JSON-RPC session/update notifications into normalized agent.Message types.

package opencode

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// parseMessage decodes a single line from the OpenCode ACP output into one or
// more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
//
// Emitted agent.Message types:
//   - InitMessage          — caic_session or caic_init injection
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
func parseMessage(line []byte) ([]agent.Message, *opencode.ToolCallUpdateUpdate, error) {
	var probe opencode.MessageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines have a "type" field.
	if probe.Type != "" {
		switch probe.Type {
		case "caic_session":
			m, err := agent.DecodeV1MetaSessionMessage(line)
			if err != nil {
				return nil, nil, err
			}
			return []agent.Message{&agent.InitMessage{
				SessionID:      m.SessionID,
				ReportedModel:  m.ReportedModel,
				ReportedEffort: m.ReportedEffort,
				Version:        m.AgentVersion,
			}}, nil, nil
		case "caic_init":
			var ci CaicInit
			if err := json.Unmarshal(line, &ci); err != nil {
				return nil, nil, err
			}
			return []agent.Message{&agent.InitMessage{
				SessionID:     ci.SessionID,
				ReportedModel: ci.ReportedModel,
				Version:       ci.Version,
			}}, nil, nil
		case "caic_diff_stat":
			var m agent.DiffStatMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, nil, err
			}
			return []agent.Message{&m}, nil, nil
		case "caic_exit":
			var m agent.ExitMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, nil, err
			}
			return []agent.Message{&m}, nil, nil
		default:
			return []agent.Message{&agent.RawMessage{MessageType: probe.Type, Raw: append([]byte(nil), line...)}}, nil, nil
		}
	}

	// JSON-RPC response (has "id").
	if probe.ID != nil {
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil, nil
	}

	// JSON-RPC notification — dispatch on method.
	var msg opencode.JSONRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, nil, fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	switch msg.Method {
	case opencode.MethodSessionUpdate:
		msgs, toolCall, err := parseSessionUpdate(msg.Params, line)
		return msgs, toolCall, err

	case opencode.MethodSessionRequestPermission:
		// Permission requests are handled by wireFormat (auto-approve).
		// In the stateless parser, emit as RawMessage.
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil, nil
	}
}

// parseSessionUpdate dispatches on the sessionUpdate discriminator.
func parseSessionUpdate(params json.RawMessage, line []byte) ([]agent.Message, *opencode.ToolCallUpdateUpdate, error) {
	var sup opencode.SessionUpdateParams
	if err := json.Unmarshal(params, &sup); err != nil {
		return nil, nil, fmt.Errorf("session/update params: %w", err)
	}

	var probe opencode.UpdateProbe
	if err := json.Unmarshal(sup.Update, &probe); err != nil {
		return nil, nil, fmt.Errorf("session/update probe: %w", err)
	}

	switch probe.SessionUpdate {
	case opencode.UpdateAgentMessageChunk:
		var u opencode.AgentMessageChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("agent_message_chunk: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: u.Content.Text}}, nil, nil

	case opencode.UpdateAgentThoughtChunk:
		var u opencode.AgentThoughtChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("agent_thought_chunk: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: u.Content.Text}}, nil, nil

	case opencode.UpdateUserMessageChunk:
		var u opencode.UserMessageChunkUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("user_message_chunk: %w", err)
		}
		return []agent.Message{&agent.UserInputMessage{Text: u.Content.Text}}, nil, nil

	case opencode.UpdateToolCall, opencode.UpdateToolCallUpdate:
		// The announcement and the update carry the same fields, so decode once
		// and hand the same value to the conversion and to native correlation.
		var u opencode.ToolCallUpdateUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", probe.SessionUpdate, err)
		}
		var msgs []agent.Message
		if probe.SessionUpdate == opencode.UpdateToolCall {
			msgs = parseToolCall(&u)
		} else {
			msgs = parseToolCallUpdate(&u)
		}
		return msgs, &u, nil

	case opencode.UpdatePlan:
		msgs, err := parsePlanUpdate(sup.Update)
		return msgs, nil, err

	case opencode.UpdateUsageUpdate:
		var u opencode.UsageUpdateUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("usage_update: %w", err)
		}
		return []agent.Message{&agent.UsageMessage{
			ContextWindow: u.Size,
		}}, nil, nil

	case opencode.UpdateCurrentModeUpdate:
		var u opencode.CurrentModeUpdate
		if err := json.Unmarshal(sup.Update, &u); err != nil {
			return nil, nil, fmt.Errorf("current_mode_update: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "mode_update",
			Detail:      u.CurrentModeID,
		}}, nil, nil

	case opencode.UpdateSessionInfoUpdate:
		return nil, nil, nil // cosmetic, skip

	case opencode.UpdateAvailableCommandsUpdate, opencode.UpdateConfigOptionUpdate:
		return nil, nil, nil // internal, skip

	default:
		return []agent.Message{&agent.RawMessage{MessageType: "session/update:" + string(probe.SessionUpdate), Raw: append([]byte(nil), line...)}}, nil, nil
	}
}

// parseToolCall handles tool_call session updates (initial tool announcement),
// rendering the update its caller decoded.
func parseToolCall(u *opencode.ToolCallUpdateUpdate) []agent.Message {
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
func parseToolCallUpdate(u *opencode.ToolCallUpdateUpdate) []agent.Message {
	switch u.Status {
	case opencode.StatusCompleted:
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID}}
	case opencode.StatusFailed:
		errMsg := extractToolError(u)
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: u.ToolCallID, Error: errMsg}}
	case opencode.StatusInProgress:
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
			// The arguments arrive only here, so this is the one place a
			// skill read can be recovered without counting it twice.
			msgs = append(msgs, inferredSkillReads(name, u.RawInput, u.ToolCallID)...)
		}
		// Also emit output delta if content is available.
		if delta := extractToolOutputDelta(u); delta != "" {
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

// inferredSkillReads reports the skills an OpenCode tool call opens as files.
// OpenCode has no Skill tool; it scans ~/.claude, ~/.agents, and its own
// config directories, then the model opens the file it wants.
func inferredSkillReads(name string, input json.RawMessage, sourceID string) []agent.Message {
	switch name {
	case "Read":
		var in struct {
			FilePath string `json:"filePath"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil
		}
		return agent.InferredSkillReadFromPath(in.FilePath, sourceID)
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil
		}
		return agent.InferredSkillReadsFromCommand(in.Command, sourceID)
	}
	return nil
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
	var input opencode.EditInput
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

// extractToolError extracts the error message from a failed tool call update.
// It checks rawOutput.error first (structured), then falls back to content text.
func extractToolError(u *opencode.ToolCallUpdateUpdate) string {
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

// extractToolOutputDelta extracts streaming output from an in-progress tool call.
// It checks rawOutput.output first (structured), then falls back to content text.
func extractToolOutputDelta(u *opencode.ToolCallUpdateUpdate) string {
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
	var u opencode.PlanUpdate
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

// normalizeToolName maps OpenCode tool titles and kinds to caic canonical names.
func normalizeToolName(title string, kind opencode.ToolKind) string {
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
	case opencode.KindExecute:
		return "Bash"
	case opencode.KindEdit:
		return "Edit"
	case opencode.KindRead:
		return "Read"
	case opencode.KindSearch:
		return "Grep"
	case opencode.KindFetch:
		return "WebFetch"
	case opencode.KindDelete, opencode.KindMove, opencode.KindThink, opencode.KindSwitchMode, opencode.KindOther:
		// No mapping; fall through to passthrough.
	}

	// Return original title as-is.
	return title
}
