// Conversion from internal agent.Message types to dto.EventMessage for SSE.
package server

import (
	"encoding/json"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/server/dto"
)

// toolTimingTracker computes per-tool-call duration by recording the timestamp
// when each tool_use is seen and computing the delta when the corresponding
// UserMessage arrives.
type toolTimingTracker struct {
	pending map[string]time.Time // toolUseID → timestamp when tool_use was seen
}

func newToolTimingTracker() *toolTimingTracker {
	return &toolTimingTracker{pending: make(map[string]time.Time)}
}

// convertMessage converts an agent.Message into zero or more EventMessages.
// A single AssistantMessage can produce multiple events (one per content
// block + one usage event). Returns nil for messages that should be filtered
// (RawMessage, etc.).
func (tt *toolTimingTracker) convertMessage(msg agent.Message, now time.Time) []dto.EventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.SystemInitMessage:
		if m.Subtype == "init" {
			return []dto.EventMessage{{
				Kind: dto.EventKindInit,
				Ts:   ts,
				Init: &dto.EventInit{
					Model:             m.Model,
					ClaudeCodeVersion: m.Version,
					SessionID:         m.SessionID,
					Tools:             m.Tools,
					Cwd:               m.Cwd,
				},
			}}
		}
		return []dto.EventMessage{{
			Kind:   dto.EventKindSystem,
			Ts:     ts,
			System: &dto.EventSystem{Subtype: m.Subtype},
		}}
	case *agent.SystemMessage:
		return []dto.EventMessage{{
			Kind:   dto.EventKindSystem,
			Ts:     ts,
			System: &dto.EventSystem{Subtype: m.Subtype},
		}}
	case *agent.AssistantMessage:
		return tt.convertAssistant(m, ts, now)
	case *agent.UserMessage:
		return tt.convertUser(m, ts, now)
	case *agent.ResultMessage:
		return []dto.EventMessage{{
			Kind: dto.EventKindResult,
			Ts:   ts,
			Result: &dto.EventResult{
				Subtype:       m.Subtype,
				IsError:       m.IsError,
				Result:        m.Result,
				TotalCostUSD:  m.TotalCostUSD,
				DurationMs:    m.DurationMs,
				DurationAPIMs: m.DurationAPIMs,
				NumTurns:      m.NumTurns,
				Usage: dto.EventUsage{
					InputTokens:              m.Usage.InputTokens,
					OutputTokens:             m.Usage.OutputTokens,
					CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
					ServiceTier:              m.Usage.ServiceTier,
				},
			},
		}}
	default:
		// RawMessage (stream_event, tool_progress), MetaMessage, etc. — filtered.
		return nil
	}
}

func (tt *toolTimingTracker) convertAssistant(m *agent.AssistantMessage, ts int64, now time.Time) []dto.EventMessage {
	var events []dto.EventMessage
	for _, block := range m.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, dto.EventMessage{
					Kind: dto.EventKindText,
					Ts:   ts,
					Text: &dto.EventText{Text: block.Text},
				})
			}
		case "tool_use":
			tt.pending[block.ID] = now
			if block.Name == "AskUserQuestion" {
				events = append(events, dto.EventMessage{
					Kind: dto.EventKindAsk,
					Ts:   ts,
					Ask: &dto.EventAsk{
						ToolUseID: block.ID,
						Questions: parseAskInput(block.Input),
					},
				})
			} else {
				events = append(events, dto.EventMessage{
					Kind: dto.EventKindToolUse,
					Ts:   ts,
					ToolUse: &dto.EventToolUse{
						ToolUseID: block.ID,
						Name:      block.Name,
						Input:     block.Input,
					},
				})
			}
		}
	}
	// Emit per-turn usage.
	u := m.Message.Usage
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		events = append(events, dto.EventMessage{
			Kind: dto.EventKindUsage,
			Ts:   ts,
			Usage: &dto.EventUsage{
				InputTokens:              u.InputTokens,
				OutputTokens:             u.OutputTokens,
				CacheCreationInputTokens: u.CacheCreationInputTokens,
				CacheReadInputTokens:     u.CacheReadInputTokens,
				ServiceTier:              u.ServiceTier,
				Model:                    m.Message.Model,
			},
		})
	}
	return events
}

func (tt *toolTimingTracker) convertUser(m *agent.UserMessage, ts int64, now time.Time) []dto.EventMessage {
	// User text input (no parent tool) vs tool result.
	if m.ParentToolUseID == nil {
		text := extractUserInputText(m.Message)
		if text == "" {
			return nil
		}
		return []dto.EventMessage{{
			Kind:      dto.EventKindUserInput,
			Ts:        ts,
			UserInput: &dto.EventUserInput{Text: text},
		}}
	}
	toolUseID := *m.ParentToolUseID
	var durationMs int64
	if started, ok := tt.pending[toolUseID]; ok {
		durationMs = now.Sub(started).Milliseconds()
		delete(tt.pending, toolUseID)
	}
	errText := extractToolError(m.Message)
	return []dto.EventMessage{{
		Kind: dto.EventKindToolResult,
		Ts:   ts,
		ToolResult: &dto.EventToolResult{
			ToolUseID:  toolUseID,
			DurationMs: durationMs,
			Error:      errText,
		},
	}}
}

// parseAskInput extracts typed AskQuestion data from the opaque tool input.
func parseAskInput(raw json.RawMessage) []dto.AskQuestion {
	var input struct {
		Questions []dto.AskQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Questions
	}
	return nil
}

// extractUserInputText extracts the text from a user input message.
// User inputs have the shape {"role":"user","content":"the text"}.
func extractUserInputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &msg) == nil && msg.Role == "user" {
		return msg.Content
	}
	return ""
}

// extractToolError checks if a UserMessage contains an error indicator.
func extractToolError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if json.Unmarshal(raw, &msg) == nil && msg.IsError {
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text != "" {
				return c.Text
			}
		}
	}
	return ""
}
