package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// toolNameMap maps Gemini CLI tool names to normalized (Claude Code) names
// used by the rest of the system.
var toolNameMap = map[string]string{
	"read_file":         "Read",
	"read_many_files":   "Read",
	"write_file":        "Write",
	"replace":           "Edit",
	"run_shell_command": "Bash",
	"grep":              "Grep",
	"grep_search":       "Grep",
	"glob":              "Glob",
	"web_fetch":         "WebFetch",
	"google_web_search": "WebSearch",
	"ask_user":          "AskUserQuestion",
	"write_todos":       "TodoWrite",
	"list_directory":    "ListDirectory",
}

// normalizeToolName maps a Gemini tool name to its normalized form.
// Unknown tools are returned as-is.
func normalizeToolName(name string) string {
	if mapped, ok := toolNameMap[name]; ok {
		return mapped
	}
	return name
}

// parseMessage decodes a single Gemini CLI stream-json line into one or more
// typed agent.Messages.
//
// Emitted agent.Message types:
//   - InitMessage       — type=init
//   - TextMessage       — type=message role=assistant
//   - UserInputMessage  — type=message role=user
//   - ToolUseMessage    — type=tool_use (generic tools)
//   - AskMessage        — type=tool_use name=ask_user
//   - TodoMessage       — type=tool_use name=write_todos
//   - ToolResultMessage — type=tool_result
//   - ResultMessage     — type=result
//   - DiffStatMessage   — caic_diff_stat injection
//   - RawMessage        — unrecognised wire types (preserved verbatim)
func parseMessage(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}
	switch rec.Type {
	case TypeInit:
		r, err := rec.AsInit()
		if err != nil {
			return nil, err
		}
		fw.WarnOverflows("InitRecord", r)
		return []agent.Message{&agent.InitMessage{
			SessionID: r.SessionID,
			Model:     r.Model,
		}}, nil

	case TypeMessage:
		r, err := rec.AsMessage()
		if err != nil {
			return nil, err
		}
		fw.WarnOverflows("MessageRecord", r)
		switch r.Role {
		case "assistant":
			return []agent.Message{&agent.TextMessage{Text: r.Content}}, nil
		case "user":
			return []agent.Message{&agent.UserInputMessage{Text: r.Content}}, nil
		default:
			return []agent.Message{&agent.RawMessage{MessageType: rec.Type, Raw: append([]byte(nil), line...)}}, nil
		}

	case TypeToolUse:
		r, err := rec.AsToolUse()
		if err != nil {
			return nil, err
		}
		fw.WarnOverflows("ToolUseRecord", r)
		return []agent.Message{dispatchToolUse(r.ToolID, normalizeToolName(r.ToolName), r.Parameters)}, nil

	case TypeToolResult:
		r, err := rec.AsToolResult()
		if err != nil {
			return nil, err
		}
		fw.WarnOverflows("ToolResultRecord", r)
		m := &agent.ToolResultMessage{ToolUseID: r.ToolID}
		if r.Status == "error" && r.Error != nil {
			m.Error = r.Error.Message
		}
		return []agent.Message{m}, nil

	case TypeResult:
		r, err := rec.AsResult()
		if err != nil {
			return nil, err
		}
		fw.WarnOverflows("ResultRecord", r)
		msg := &agent.ResultMessage{
			MessageType: "result",
			Subtype:     "result",
			IsError:     r.Status != "success",
		}
		if r.Stats != nil {
			msg.DurationMs = r.Stats.DurationMs
			msg.NumTurns = r.Stats.ToolCalls
			msg.Usage = agent.Usage{
				InputTokens:          r.Stats.InputTokens,
				OutputTokens:         r.Stats.OutputTokens,
				CacheReadInputTokens: r.Stats.Cached,
			}
		}
		return []agent.Message{msg}, nil

	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: rec.Type, Raw: append([]byte(nil), line...)}}, nil
	}
}

// dispatchToolUse creates the appropriate message type based on the normalized
// tool name. AskUserQuestion and TodoWrite get their own semantic types.
func dispatchToolUse(id, name string, input json.RawMessage) agent.Message {
	switch name {
	case "AskUserQuestion":
		var parsed struct {
			Questions []agent.AskQuestion `json:"questions"`
		}
		if json.Unmarshal(input, &parsed) == nil && len(parsed.Questions) > 0 {
			return &agent.AskMessage{ToolUseID: id, Questions: parsed.Questions}
		}
	case "TodoWrite":
		var parsed struct {
			Todos []agent.TodoItem `json:"todos"`
		}
		if json.Unmarshal(input, &parsed) == nil && len(parsed.Todos) > 0 {
			return &agent.TodoMessage{ToolUseID: id, Todos: parsed.Todos}
		}
	}
	return &agent.ToolUseMessage{ToolUseID: id, Name: name, Input: input}
}
