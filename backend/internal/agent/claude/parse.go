// Claude Code stream-json parser. Converts Claude's wire format into
// backend-neutral agent.Message types.
package claude

import (
	"encoding/json"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// parseEnvelope is a local alias for typeProbe used by ParseMessage.
type parseEnvelope = typeProbe

// ParseMessage decodes a single Claude Code NDJSON line into one or more
// typed agent.Messages. A single "assistant" line may contain multiple
// content blocks (text + tool_use + usage), each producing a separate message.
func ParseMessage(line []byte) ([]agent.Message, error) {
	var env parseEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case "system":
		return parseSystem(line, env.Subtype)
	case "assistant":
		return parseAssistant(line)
	case "user":
		return parseUser(line)
	case "result":
		var w resultWire
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ResultMessage{
			MessageType:   w.Type,
			Subtype:       w.Subtype,
			IsError:       w.IsError,
			DurationMs:    w.DurationMs,
			DurationAPIMs: w.DurationAPIMs,
			NumTurns:      w.NumTurns,
			Result:        w.Result,
			SessionID:     w.SessionID,
			TotalCostUSD:  w.TotalCostUSD,
			Usage:         w.Usage,
			UUID:          w.UUID,
		}}, nil
	case "stream_event":
		return parseStreamEvent(line)
	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	default:
		return []agent.Message{&agent.RawMessage{MessageType: env.Type, Raw: append([]byte(nil), line...)}}, nil
	}
}

func parseSystem(line []byte, subtype string) ([]agent.Message, error) {
	if subtype == "init" {
		var w initWire
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.InitMessage{
			SessionID: w.SessionID,
			Cwd:       w.Cwd,
			Tools:     w.Tools,
			Model:     w.Model,
			Version:   w.Version,
		}}, nil
	}
	var w systemWire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	return []agent.Message{&agent.SystemMessage{
		MessageType: w.Type,
		Subtype:     w.Subtype,
		SessionID:   w.SessionID,
		UUID:        w.UUID,
	}}, nil
}

func parseAssistant(line []byte) ([]agent.Message, error) {
	var w assistantWire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	var msgs []agent.Message
	for i := range w.Message.Content {
		b := &w.Message.Content[i]
		switch b.Type {
		case "text":
			if b.Text != "" {
				msgs = append(msgs, &agent.TextMessage{Text: b.Text})
			}
		case "tool_use":
			msgs = append(msgs, parseToolUseBlock(b)...)
		}
	}
	u := w.Message.Usage
	if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0 {
		msgs = append(msgs, &agent.UsageMessage{
			Usage: u,
			Model: w.Message.Model,
		})
	}
	if len(msgs) == 0 {
		// Preserve as raw if nothing was extracted (e.g. empty content).
		msgs = append(msgs, &agent.RawMessage{MessageType: "assistant", Raw: append([]byte(nil), line...)})
	}
	return msgs, nil
}

func parseToolUseBlock(b *contentBlockWire) []agent.Message {
	switch b.Name {
	case "AskUserQuestion":
		var input askInput
		if json.Unmarshal(b.Input, &input) == nil && len(input.Questions) > 0 {
			return []agent.Message{&agent.AskMessage{
				ToolUseID: b.ID,
				Questions: input.Questions,
			}}
		}
		// Fall through to generic ToolUseMessage if parse fails.
	case "TodoWrite":
		var input todoInput
		if json.Unmarshal(b.Input, &input) == nil && len(input.Todos) > 0 {
			return []agent.Message{&agent.TodoMessage{
				ToolUseID: b.ID,
				Todos:     input.Todos,
			}}
		}
	}
	return []agent.Message{&agent.ToolUseMessage{
		ToolUseID: b.ID,
		Name:      b.Name,
		Input:     b.Input,
	}}
}

func parseUser(line []byte) ([]agent.Message, error) {
	var w userWire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	if w.ParentToolUseID == nil {
		return []agent.Message{extractUserInput(w.Message)}, nil
	}
	return []agent.Message{extractToolResult(*w.ParentToolUseID, w.Message)}, nil
}

func extractUserInput(raw json.RawMessage) *agent.UserInputMessage {
	if len(raw) == 0 {
		return &agent.UserInputMessage{}
	}
	var textMsg userTextMessage
	if json.Unmarshal(raw, &textMsg) == nil && textMsg.Role == "user" && textMsg.Content != "" {
		return &agent.UserInputMessage{Text: textMsg.Content}
	}
	var blockMsg userBlockMessage
	if json.Unmarshal(raw, &blockMsg) == nil && blockMsg.Role == "user" {
		ui := &agent.UserInputMessage{}
		for _, b := range blockMsg.Content {
			switch b.Type {
			case "text":
				ui.Text = b.Text
			case "image":
				if b.Source != nil {
					ui.Images = append(ui.Images, agent.ImageData{
						MediaType: b.Source.MediaType,
						Data:      b.Source.Data,
					})
				}
			}
		}
		return ui
	}
	return &agent.UserInputMessage{}
}

func extractToolResult(toolUseID string, raw json.RawMessage) *agent.ToolResultMessage {
	m := &agent.ToolResultMessage{ToolUseID: toolUseID}
	if len(raw) == 0 {
		return m
	}
	var msg toolResultWire
	if json.Unmarshal(raw, &msg) == nil && msg.IsError {
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text != "" {
				m.Error = c.Text
				return m
			}
		}
	}
	return m
}

func parseStreamEvent(line []byte) ([]agent.Message, error) {
	var w streamEventWire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	if w.Event.Type == "content_block_delta" && w.Event.Delta != nil && w.Event.Delta.Type == "text_delta" && w.Event.Delta.Text != "" {
		return []agent.Message{&agent.TextDeltaMessage{Text: w.Event.Delta.Text}}, nil
	}
	return []agent.Message{&agent.RawMessage{MessageType: "stream_event", Raw: append([]byte(nil), line...)}}, nil
}
