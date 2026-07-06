// Agent event conversions from backend messages to API v1 SSE DTOs.

package v1conv

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// InputTruncateThreshold is the maximum byte length of a tool input JSON before it
// is omitted from the SSE stream. Clients fetch the full input on demand via
// GET /api/caic/v1/tasks/{id}/tool/{toolUseID}.
const InputTruncateThreshold = 4096

// ToolOutputFormatter analyzes a tool output string and returns its content
// type along with an optional formatted version.
type ToolOutputFormatter func(raw string) (contentType v1.ToolOutputContentType, formatted string)

// ToolTimingTracker computes per-tool-call duration while converting agent
// messages to API events.
type ToolTimingTracker struct {
	harness          harness.Name
	formatToolOutput ToolOutputFormatter
	pending          map[string]time.Time
}

// NewToolTimingTracker creates a tracker for converting agent messages from a
// harness into API events.
func NewToolTimingTracker(h harness.Name, f ToolOutputFormatter) *ToolTimingTracker {
	return &ToolTimingTracker{
		harness:          h,
		formatToolOutput: f,
		pending:          make(map[string]time.Time),
	}
}

// ConvertMessage converts an agent.Message into zero or more EventMessages.
func (tt *ToolTimingTracker) ConvertMessage(msg agent.Message, now time.Time) []v1.EventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.InitMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindInit,
			Ts:   ts,
			Init: &v1.EventInit{
				Model:        m.Model,
				Effort:       m.Effort,
				AgentVersion: m.Version,
				SessionID:    m.SessionID,
				Tools:        m.Tools,
				Cwd:          m.Cwd,
				Harness:      string(tt.harness),
			},
		}}
	case *agent.SystemMessage:
		return []v1.EventMessage{{
			Kind:   v1.EventKindSystem,
			Ts:     ts,
			System: &v1.EventSystem{Subtype: m.Subtype, Detail: m.Detail},
		}}
	case *agent.TextMessage:
		if m.Text != "" {
			// TODO: propagate m.Phase to EventText once EventText has a Phase field.
			return []v1.EventMessage{{
				Kind: v1.EventKindText,
				Ts:   ts,
				Text: &v1.EventText{Text: m.Text},
			}}
		}
		return nil
	case *agent.ToolUseMessage:
		tt.pending[m.ToolUseID] = now
		detail, inputView := ToolUseDisplay(m.Name, m.Input, &m.InputView)
		if m.Detail != "" {
			detail = m.Detail
		}
		input := m.Input
		truncated := false
		if len(input) > InputTruncateThreshold {
			input = nil
			truncated = true
		}
		var bg bool
		if len(m.Input) > 0 {
			var probe toolInputProbe
			if json.Unmarshal(m.Input, &probe) == nil {
				bg = probe.RunInBackground
			}
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindToolUse,
			Ts:   ts,
			ToolUse: &v1.EventToolUse{
				ToolUseID:      m.ToolUseID,
				Name:           m.Name,
				Input:          input,
				Detail:         detail,
				InputView:      inputView,
				PlanContent:    m.PlanContent,
				InputTruncated: truncated,
				Background:     bg,
			},
		}}
	case *agent.AskMessage:
		tt.pending[m.ToolUseID] = now
		return []v1.EventMessage{{
			Kind: v1.EventKindAsk,
			Ts:   ts,
			Ask: &v1.EventAsk{
				ToolUseID: m.ToolUseID,
				Questions: AskQuestions(m.Questions),
			},
		}}
	case *agent.TodoMessage:
		tt.pending[m.ToolUseID] = now
		if todos := TodoItems(m.Todos); len(todos) > 0 {
			return []v1.EventMessage{{
				Kind: v1.EventKindTodo,
				Ts:   ts,
				Todo: &v1.EventTodo{ToolUseID: m.ToolUseID, Todos: todos},
			}}
		}
		return nil
	case *agent.UserInputMessage:
		if m.Text == "" && len(m.Images) == 0 {
			return nil
		}
		var images []v1.ImageData
		for _, img := range m.Images {
			images = append(images, v1.ImageData{MediaType: img.MediaType, Data: img.Data})
		}
		return []v1.EventMessage{{
			Kind:      v1.EventKindUserInput,
			Ts:        ts,
			UserInput: &v1.EventUserInput{Text: m.Text, Images: images},
		}}
	case *agent.ToolResultMessage:
		var duration float64
		if started, ok := tt.pending[m.ToolUseID]; ok {
			duration = now.Sub(started).Seconds()
			delete(tt.pending, m.ToolUseID)
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindToolResult,
			Ts:   ts,
			ToolResult: &v1.EventToolResult{
				ToolUseID: m.ToolUseID,
				Duration:  duration,
				Error:     m.Error,
			},
		}}
	case *agent.UsageMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindUsage,
			Ts:   ts,
			Usage: &v1.EventUsage{
				InputTokens:              m.Usage.InputTokens,
				OutputTokens:             m.Usage.OutputTokens,
				CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
				ReasoningOutputTokens:    m.Usage.ReasoningOutputTokens,
				Model:                    m.Model,
			},
		}}
	case *agent.ResultMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindResult,
			Ts:   ts,
			Result: &v1.EventResult{
				Subtype:      m.Subtype,
				IsError:      m.IsError,
				Result:       m.Result,
				DiffStat:     DiffStat(m.DiffStat),
				TotalCostUSD: m.TotalCostUSD,
				Duration:     float64(m.DurationMs) / 1e3,
				DurationAPI:  float64(m.DurationAPIMs) / 1e3,
				NumTurns:     m.NumTurns,
				Usage: v1.EventUsage{
					InputTokens:              m.Usage.InputTokens,
					OutputTokens:             m.Usage.OutputTokens,
					CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
					ReasoningOutputTokens:    m.Usage.ReasoningOutputTokens,
				},
			},
		}}
	case *agent.TextDeltaMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:      v1.EventKindTextDelta,
				Ts:        ts,
				TextDelta: &v1.EventTextDelta{Text: m.Text},
			}}
		}
		return nil
	case *agent.ThinkingMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:     v1.EventKindThinking,
				Ts:       ts,
				Thinking: &v1.EventThinking{Text: m.Text},
			}}
		}
		return nil
	case *agent.ThinkingDeltaMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind:          v1.EventKindThinkingDelta,
				Ts:            ts,
				ThinkingDelta: &v1.EventThinkingDelta{Text: m.Text},
			}}
		}
		return nil
	case *agent.SubagentStartMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindSubagentStart,
			Ts:   ts,
			SubagentStart: &v1.EventSubagentStart{
				TaskID:      m.TaskID,
				Description: m.Description,
			},
		}}
	case *agent.SubagentEndMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindSubagentEnd,
			Ts:   ts,
			SubagentEnd: &v1.EventSubagentEnd{
				TaskID: m.TaskID,
				Status: m.Status,
			},
		}}
	case *agent.DiffStatMessage:
		return []v1.EventMessage{{
			Kind:     v1.EventKindDiffStat,
			Ts:       ts,
			DiffStat: &v1.EventDiffStat{DiffStat: DiffStat(m.DiffStat)},
		}}
	case *agent.ParseErrorMessage:
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: m.Err, Line: m.Line},
		}}
	case *agent.ExitMessage:
		if m.ExitCode == 0 {
			return nil
		}
		err := m.Error
		if err == "" {
			err = fmt.Sprintf("agent subprocess exited with code %d", m.ExitCode)
		}
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: err},
		}}
	case *agent.LogMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindLog,
			Ts:   ts,
			Log:  &v1.EventLog{Line: m.Line},
		}}
	case *agent.ToolOutputDeltaMessage:
		if m.Delta != "" {
			ev := &v1.EventToolOutputDelta{
				ToolUseID: m.ToolUseID,
				Delta:     m.Delta,
			}
			if tt.formatToolOutput != nil {
				contentType, formatted := tt.formatToolOutput(m.Delta)
				if contentType != "" {
					ev.ContentType = contentType
				}
				if formatted != "" {
					ev.Formatted = formatted
				}
			}
			return []v1.EventMessage{{
				Kind:            v1.EventKindToolOutputDelta,
				Ts:              ts,
				ToolOutputDelta: ev,
			}}
		}
		return nil
	case *agent.WidgetMessage:
		tt.pending[m.ToolUseID] = now
		return []v1.EventMessage{{
			Kind: v1.EventKindWidget,
			Ts:   ts,
			Widget: &v1.EventWidget{
				ToolUseID: m.ToolUseID,
				Title:     m.Title,
				HTML:      m.HTML,
			},
		}}
	case *agent.WidgetDeltaMessage:
		if m.Delta != "" {
			return []v1.EventMessage{{
				Kind: v1.EventKindWidgetDelta,
				Ts:   ts,
				WidgetDelta: &v1.EventWidgetDelta{
					ToolUseID: m.ToolUseID,
					Delta:     m.Delta,
				},
			}}
		}
		return nil
	case *agent.RateLimitMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindRateLimit,
			Ts:   ts,
			RateLimit: &v1.EventRateLimit{
				Status:          m.Status,
				ResetsAt:        epochSecondsToTime(m.ResetsAt),
				RateLimitType:   m.RateLimitType,
				Utilization:     m.Utilization,
				IsUsingOverage:  m.IsUsingOverage,
				OverageResetsAt: epochSecondsToTime(m.OverageResetsAt),
			},
		}}
	default:
		return nil
	}
}

// AskQuestions converts agent ask questions to API DTOs.
func AskQuestions(qs []agent.AskQuestion) []v1.AskQuestion {
	if len(qs) == 0 {
		return nil
	}
	out := make([]v1.AskQuestion, len(qs))
	for i, q := range qs {
		opts := make([]v1.AskOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = v1.AskOption{Label: o.Label, Description: o.Description}
		}
		out[i] = v1.AskQuestion{
			Question:    q.Question,
			Header:      q.Header,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		}
	}
	return out
}

// TodoItems converts agent todo items to API DTOs.
func TodoItems(items []agent.TodoItem) []v1.TodoItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]v1.TodoItem, len(items))
	for i, t := range items {
		out[i] = v1.TodoItem{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm}
	}
	return out
}

// MarshalEvent marshals an EventMessage.
func MarshalEvent(ev *v1.EventMessage) ([]byte, error) {
	return json.Marshal(ev)
}

func epochSecondsToTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	sec := int64(seconds)
	nsec := int64((seconds - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// toolInputProbe extracts run_in_background from a tool's raw JSON input.
type toolInputProbe struct {
	RunInBackground bool `json:"run_in_background"`
}
