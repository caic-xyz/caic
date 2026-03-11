package kilo

import (
	"encoding/json"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// toolNameMap maps kilo lowercase tool names to normalized (PascalCase) names
// used by the rest of the system.
var toolNameMap = map[string]string{
	"bash":       "Bash",
	"read":       "Read",
	"write":      "Write",
	"edit":       "Edit",
	"glob":       "Glob",
	"grep":       "Grep",
	"web_fetch":  "WebFetch",
	"web_search": "WebSearch",
	"todo_write": "TodoWrite",
	"ask_user":   "AskUserQuestion",
	"agent":      "Agent",
}

// normalizeToolName maps a kilo tool name to its normalized form.
// Unknown tools are returned as-is.
func normalizeToolName(name string) string {
	if mapped, ok := toolNameMap[name]; ok {
		return mapped
	}
	return name
}

// ParseMessage decodes a single kilo bridge NDJSON line into one or more
// typed agent.Messages.
//
// Emitted agent.Message types:
//   - InitMessage       — type=system subtype=init
//   - SystemMessage     — type=system (other subtypes), message.part.updated part.type=step_start
//   - TextMessage       — message.part.updated part.type=text
//   - TextDeltaMessage  — message.part.delta
//   - ThinkingMessage   — message.part.updated part.type=reasoning
//   - ToolUseMessage    — message.part.updated part.type=tool state.status=running (generic tools)
//   - AskMessage        — ask_user tool (state.status=running)
//   - TodoMessage       — todo_write tool (state.status=running)
//   - ToolResultMessage — message.part.updated part.type=tool state.status=completed|error
//   - ResultMessage     — message.part.updated part.type=step_finish, session.error
//   - DiffStatMessage   — caic_diff_stat injection
//   - RawMessage        — unrecognised wire types (preserved verbatim)
func ParseMessage(line []byte) ([]agent.Message, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}
	switch rec.Type {
	case TypeSystem:
		return parseSystem(&rec)
	case TypePartUpdated:
		return parsePartUpdated(&rec)
	case TypePartDelta:
		return parsePartDelta(&rec)
	case TypeSessionError:
		return parseSessionError(&rec)
	case TypeTurnClose:
		return parseTurnClose(&rec)
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

// parseSystem handles type=system records (init or generic).
func parseSystem(rec *Record) ([]agent.Message, error) {
	r, err := rec.AsInit()
	if err != nil {
		return nil, err
	}
	if r.Subtype == "init" {
		return []agent.Message{&agent.InitMessage{
			SessionID: r.SessionID,
			Model:     r.Model,
		}}, nil
	}
	return []agent.Message{&agent.SystemMessage{
		MessageType: "system",
		Subtype:     r.Subtype,
	}}, nil
}

// parsePartUpdated dispatches on part type within a message.part.updated event.
func parsePartUpdated(rec *Record) ([]agent.Message, error) {
	r, err := rec.AsPartUpdated()
	if err != nil {
		return nil, err
	}
	part := &r.Properties.Part

	switch part.Type {
	case PartTypeText:
		return []agent.Message{&agent.TextMessage{Text: part.Text}}, nil

	case PartTypeTool:
		return parseToolPart(part)

	case PartTypeStepFinish:
		return parseStepFinish(part)

	case PartTypeReasoning:
		return []agent.Message{&agent.ThinkingMessage{Text: part.Text}}, nil

	case PartTypeStepStart:
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "step_start",
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{
			MessageType: TypePartUpdated,
			Raw:         append([]byte(nil), rec.Raw()...),
		}}, nil
	}
}

// parseToolPart handles tool parts. Running → ToolUseMessage/AskMessage/TodoMessage,
// completed/error → ToolResultMessage.
func parseToolPart(part *Part) ([]agent.Message, error) {
	if part.State == nil {
		return []agent.Message{&agent.RawMessage{MessageType: TypePartUpdated}}, nil
	}

	switch part.State.Status {
	case "running":
		name := normalizeToolName(part.Tool)
		return []agent.Message{dispatchToolUse(part.CallID, name, part.State.Input)}, nil

	case "completed", "error":
		m := &agent.ToolResultMessage{ToolUseID: part.CallID}
		if part.State.Status == "error" {
			m.Error = part.State.Error
		}
		return []agent.Message{m}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: TypePartUpdated}}, nil
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

// parseStepFinish converts a step-finish part into a ResultMessage with usage/cost.
func parseStepFinish(part *Part) ([]agent.Message, error) {
	msg := &agent.ResultMessage{
		MessageType:  "result",
		Subtype:      "result",
		TotalCostUSD: part.Cost,
	}
	if part.Tokens != nil {
		msg.Usage = agent.Usage{
			InputTokens:           part.Tokens.Input,
			OutputTokens:          part.Tokens.Output,
			ReasoningOutputTokens: part.Tokens.Reasoning,
		}
		if part.Tokens.Cache != nil {
			msg.Usage.CacheReadInputTokens = part.Tokens.Cache.Read
			msg.Usage.CacheCreationInputTokens = part.Tokens.Cache.Write
		}
	}
	return []agent.Message{msg}, nil
}

// parsePartDelta converts a message.part.delta event into a TextDeltaMessage.
func parsePartDelta(rec *Record) ([]agent.Message, error) {
	r, err := rec.AsPartDelta()
	if err != nil {
		return nil, err
	}
	if r.Properties.Delta != "" {
		return []agent.Message{&agent.TextDeltaMessage{Text: r.Properties.Delta}}, nil
	}
	return []agent.Message{&agent.RawMessage{
		MessageType: TypePartDelta,
		Raw:         append([]byte(nil), rec.Raw()...),
	}}, nil
}

// parseSessionError converts a session.error event into a ResultMessage with
// the actual error message from kilo (e.g. "Model not found: ...").
func parseSessionError(rec *Record) ([]agent.Message, error) {
	r, err := rec.AsSessionError()
	if err != nil {
		return nil, err
	}
	msg := r.Properties.Error.Data.Message
	if msg == "" {
		msg = r.Properties.Error.Name
	}
	return []agent.Message{&agent.ResultMessage{
		MessageType: "result",
		Subtype:     "result",
		IsError:     true,
		Result:      msg,
	}}, nil
}

// parseTurnClose converts a session.turn.close event. Error details come from
// the preceding session.error event, so error turns are passed through as raw.
func parseTurnClose(rec *Record) ([]agent.Message, error) {
	_, err := rec.AsTurnClose()
	if err != nil {
		return nil, err
	}
	return []agent.Message{&agent.RawMessage{
		MessageType: TypeTurnClose,
		Raw:         append([]byte(nil), rec.Raw()...),
	}}, nil
}
