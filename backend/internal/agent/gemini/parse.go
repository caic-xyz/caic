package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/maruel/caic/backend/internal/agent"
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

// ParseMessage decodes a single Gemini CLI stream-json line into a typed
// agent.Message.
func ParseMessage(line []byte) (agent.Message, error) {
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
		return &agent.SystemInitMessage{
			MessageType: "system",
			Subtype:     "init",
			SessionID:   r.SessionID,
			Model:       r.Model,
		}, nil

	case TypeMessage:
		r, err := rec.AsMessage()
		if err != nil {
			return nil, err
		}
		switch r.Role {
		case "assistant":
			return &agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Role: "assistant",
					Content: []agent.ContentBlock{{
						Type: "text",
						Text: r.Content,
					}},
				},
			}, nil
		case "user":
			raw, err := json.Marshal(r.Content)
			if err != nil {
				return nil, fmt.Errorf("marshal user content: %w", err)
			}
			return &agent.UserMessage{
				MessageType: "user",
				Message:     raw,
			}, nil
		default:
			return &agent.RawMessage{MessageType: rec.Type, Raw: append([]byte(nil), line...)}, nil
		}

	case TypeToolUse:
		r, err := rec.AsToolUse()
		if err != nil {
			return nil, err
		}
		return &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{
				Role: "assistant",
				Content: []agent.ContentBlock{{
					Type:  "tool_use",
					ID:    r.ToolID,
					Name:  normalizeToolName(r.ToolName),
					Input: r.Parameters,
				}},
			},
		}, nil

	case TypeToolResult:
		r, err := rec.AsToolResult()
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(r.Output)
		if err != nil {
			return nil, fmt.Errorf("marshal tool result: %w", err)
		}
		return &agent.UserMessage{
			MessageType:     "user",
			Message:         raw,
			ParentToolUseID: &r.ToolID,
		}, nil

	case TypeResult:
		r, err := rec.AsResult()
		if err != nil {
			return nil, err
		}
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
		return msg, nil

	default:
		return &agent.RawMessage{MessageType: rec.Type, Raw: append([]byte(nil), line...)}, nil
	}
}
