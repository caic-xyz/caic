//go:build e2e

// Flat NDJSON parser for the fake agent. Each line is a JSON object whose
// "type" field maps directly to an agent.Message type.

package fake

import (
	"encoding/json"
	"fmt"

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
		return []agent.Message{&agent.TextDeltaMessage{Text: m.Text}}, nil
	case "tool_use":
		var m toolUse
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: m.ID,
			Name:      m.Name,
			Input:     m.Input,
		}}, nil
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
		var m resultMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ResultMessage{
			MessageType:  m.Type,
			Subtype:      m.Subtype,
			Result:       m.Result,
			NumTurns:     m.NumTurns,
			TotalCostUSD: m.TotalCostUSD,
			DurationMs:   m.DurationMs,
		}}, nil
	default:
		return []agent.Message{&agent.RawMessage{
			MessageType: env.Type,
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
}

// Wire types — thin wrappers matching the flat NDJSON the Python script emits.

type textDelta struct {
	Text string `json:"text"`
}

type toolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type askMsg struct {
	ID        string             `json:"id"`
	Questions []agent.AskQuestion `json:"questions"`
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

type resultMsg struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	Result       string  `json:"result"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMs   int64   `json:"duration_ms"`
}
