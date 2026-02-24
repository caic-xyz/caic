// Backend-neutral conversion from agent.Message to v1.EventMessage for SSE.
// Every backend (Claude, Gemini, Codex, …) uses this converter to produce the
// generic event stream served on /api/v1/tasks/{id}/events. The harness-
// specific raw streams (e.g. eventconv.go for Claude) are separate.
package server

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// genericToolTimingTracker mirrors toolTimingTracker but emits
// EventMessage events with a harness field on init.
type genericToolTimingTracker struct {
	harness agent.Harness
	pending map[string]time.Time
}

func newGenericToolTimingTracker(harness agent.Harness) *genericToolTimingTracker {
	return &genericToolTimingTracker{harness: harness, pending: make(map[string]time.Time)}
}

// convertMessage converts an agent.Message into zero or more EventMessages.
func (gt *genericToolTimingTracker) convertMessage(msg agent.Message, now time.Time) []v1.EventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.SystemInitMessage:
		if m.Subtype == "init" {
			return []v1.EventMessage{{
				Kind: v1.EventKindInit,
				Ts:   ts,
				Init: &v1.EventInit{
					Model:        m.Model,
					AgentVersion: m.Version,
					SessionID:    m.SessionID,
					Tools:        m.Tools,
					Cwd:          m.Cwd,
					Harness:      string(gt.harness),
				},
			}}
		}
		return []v1.EventMessage{{
			Kind:   v1.EventKindSystem,
			Ts:     ts,
			System: &v1.EventSystem{Subtype: m.Subtype},
		}}
	case *agent.SystemMessage:
		return []v1.EventMessage{{
			Kind:   v1.EventKindSystem,
			Ts:     ts,
			System: &v1.EventSystem{Subtype: m.Subtype},
		}}
	case *agent.AssistantMessage:
		return gt.convertAssistant(m, ts, now)
	case *agent.UserMessage:
		return gt.convertUser(m, ts, now)
	case *agent.ResultMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindResult,
			Ts:   ts,
			Result: &v1.EventResult{
				Subtype:      m.Subtype,
				IsError:      m.IsError,
				Result:       m.Result,
				DiffStat:     toV1DiffStat(m.DiffStat),
				TotalCostUSD: m.TotalCostUSD,
				Duration:     float64(m.DurationMs) / 1e3,
				DurationAPI:  float64(m.DurationAPIMs) / 1e3,
				NumTurns:     m.NumTurns,
				Usage: v1.EventUsage{
					InputTokens:              m.Usage.InputTokens,
					OutputTokens:             m.Usage.OutputTokens,
					CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
					ServiceTier:              m.Usage.ServiceTier,
				},
			},
		}}
	case *agent.StreamEvent:
		if m.Event.Type == "content_block_delta" && m.Event.Delta != nil && m.Event.Delta.Type == "text_delta" && m.Event.Delta.Text != "" {
			return []v1.EventMessage{{
				Kind:      v1.EventKindTextDelta,
				Ts:        ts,
				TextDelta: &v1.EventTextDelta{Text: m.Event.Delta.Text},
			}}
		}
		return nil
	case *agent.DiffStatMessage:
		return []v1.EventMessage{{
			Kind:     v1.EventKindDiffStat,
			Ts:       ts,
			DiffStat: &v1.EventDiffStat{DiffStat: toV1DiffStat(m.DiffStat)},
		}}
	case *agent.ParseErrorMessage:
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: m.Err, Line: m.Line},
		}}
	default:
		return nil
	}
}

func (gt *genericToolTimingTracker) convertAssistant(m *agent.AssistantMessage, ts int64, now time.Time) []v1.EventMessage {
	var events []v1.EventMessage
	for _, block := range m.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, v1.EventMessage{
					Kind: v1.EventKindText,
					Ts:   ts,
					Text: &v1.EventText{Text: block.Text},
				})
			}
		case "tool_use":
			gt.pending[block.ID] = now
			switch block.Name {
			case "AskUserQuestion":
				events = append(events, v1.EventMessage{
					Kind: v1.EventKindAsk,
					Ts:   ts,
					Ask: &v1.EventAsk{
						ToolUseID: block.ID,
						Questions: parseAskInput(block.Input),
					},
				})
			case "TodoWrite":
				if todo := parseTodoInput(block.ID, block.Input); todo != nil {
					events = append(events, v1.EventMessage{
						Kind: v1.EventKindTodo,
						Ts:   ts,
						Todo: todo,
					})
				}
			default:
				events = append(events, v1.EventMessage{
					Kind: v1.EventKindToolUse,
					Ts:   ts,
					ToolUse: &v1.EventToolUse{
						ToolUseID: block.ID,
						Name:      block.Name,
						Input:     block.Input,
					},
				})
			}
		}
	}
	u := m.Message.Usage
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		events = append(events, v1.EventMessage{
			Kind: v1.EventKindUsage,
			Ts:   ts,
			Usage: &v1.EventUsage{
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

func (gt *genericToolTimingTracker) convertUser(m *agent.UserMessage, ts int64, now time.Time) []v1.EventMessage {
	if m.ParentToolUseID == nil {
		ui := extractUserInput(m.Message)
		if ui.Text == "" && len(ui.Images) == 0 {
			return nil
		}
		return []v1.EventMessage{{
			Kind:      v1.EventKindUserInput,
			Ts:        ts,
			UserInput: &v1.EventUserInput{Text: ui.Text, Images: ui.Images},
		}}
	}
	toolUseID := *m.ParentToolUseID
	var duration float64
	if started, ok := gt.pending[toolUseID]; ok {
		duration = now.Sub(started).Seconds()
		delete(gt.pending, toolUseID)
	}
	errText := extractToolError(m.Message)
	return []v1.EventMessage{{
		Kind: v1.EventKindToolResult,
		Ts:   ts,
		ToolResult: &v1.EventToolResult{
			ToolUseID: toolUseID,
			Duration:  duration,
			Error:     errText,
		},
	}}
}

// marshalEvent is a convenience wrapper for json.Marshal on EventMessage.
func marshalEvent(ev *v1.EventMessage) ([]byte, error) {
	return json.Marshal(ev)
}
