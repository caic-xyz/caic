// Parser for flat fake-agent messages preserving content, tool, and usage ownership in task-log envelopes.
// Each message is a JSON object whose "type" field maps directly to an
// agent.Message type.

package smoketest

import (
	"encoding/json"
	"fmt"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// envelope is the minimal probe to determine the message type.
type envelope struct {
	Type string `json:"type"`
}

// parseMessage decodes a single flat NDJSON line into agent.Messages.
func parseMessage(line []byte) ([]agent.Message, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case "init":
		var m agent.InitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "skill_load":
		var m agent.SkillLoadMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "system":
		var m agent.SystemMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "text":
		var m agent.TextMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "text_delta":
		var m textDelta
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.TextDeltaMessage{Source: m.Source, Text: m.Text}}, nil
	case "thinking":
		var m agent.ThinkingMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "thinking_delta":
		var m textDelta
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Source: m.Source, Text: m.Text}}, nil
	case "tool_use":
		var m agent.ToolUseMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "tool_output_delta":
		var m toolOutputDelta
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ToolOutputDeltaMessage{MessageProvenance: m.MessageProvenance, ToolUseID: m.ToolUseID, Delta: m.Delta}}, nil
	case "usage":
		var m agent.UsageMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "tool_result":
		var m agent.ToolResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "ask":
		var m askMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.AskMessage{
			ToolUseID: m.ID,
			Questions: m.Questions,
		}}, nil
	case "widget":
		var m widgetMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.WidgetMessage{
			ToolUseID: m.ID,
			Title:     m.Title,
			HTML:      m.HTML,
		}}, nil
	case "widget_delta":
		var m widgetDeltaMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.WidgetDeltaMessage{
			ToolUseID: m.ID,
			Delta:     m.Delta,
		}}, nil
	case "result":
		var m agent.ResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "native_subagent":
		var m agent.NativeSubagentMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "background_command":
		var m agent.BackgroundCommandMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "rate_limit":
		var m agent.RateLimitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	default:
		return []agent.Message{&agent.RawMessage{
			MessageType: env.Type,
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
}

// Wire types: thin wrappers matching the flat NDJSON the Python script emits.

type textDelta struct {
	Source agent.MessageSource `json:"source,omitzero"`
	Text   string              `json:"text"`
}

type toolOutputDelta struct {
	agent.MessageProvenance

	ToolUseID string `json:"tool_use_id"`
	Delta     string `json:"delta"`
}

type askMsg struct {
	ID        string           `json:"id"`
	Questions []v3.AskQuestion `json:"questions"`
}

type widgetMsg struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

type widgetDeltaMsg struct {
	ID    string `json:"id"`
	Delta string `json:"delta"`
}
