// Agent event conversions from backend messages to API v1 SSE DTOs.

package apiconv

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// InputTruncateThreshold is the maximum byte length of a tool input JSON before it
// is omitted from the SSE stream. Clients fetch the full input on demand via
// GET /api/caic/v1/tasks/{id}/tool/{toolUseID}.
const InputTruncateThreshold = 4096

// maxPendingToolTimings bounds timing state while converting unbounded task
// history. When full, the next unseen tool start deterministically resets the
// map, so older results report zero duration rather than retaining timestamps.
const maxPendingToolTimings = 1024

// ToolTimingTracker computes per-tool-call duration while converting agent
// messages to API events. It also projects the immutable message stream into
// the API view: plan content for ExitPlanMode via a resolver taken at
// construction, and fallback result text via a turn text window.
//
// harness and planContent are fixed at construction; the per-stream state
// (pending, window) accumulates across ConvertMessage calls.
type ToolTimingTracker struct {
	harness     harness.Name
	pending     map[string]time.Time
	planContent func(toolUseID string) string
	window      task.ResultTextWindow
}

// NewToolTimingTracker creates a tracker for converting agent messages from a
// harness into API events. planContent resolves the plan text to display on
// an ExitPlanMode event with the given tool use ID; pass nil to disable the
// projection.
func NewToolTimingTracker(h harness.Name, planContent func(toolUseID string) string) *ToolTimingTracker {
	return &ToolTimingTracker{
		harness:     h,
		planContent: planContent,
		pending:     make(map[string]time.Time),
	}
}

// ConvertMessage converts an agent.Message into zero or more EventMessages.
func (tt *ToolTimingTracker) ConvertMessage(msg agent.Message, now time.Time) []v1.EventMessage {
	var ts int64
	if !now.IsZero() {
		ts = now.UnixMilli()
	}
	if _, isResult := msg.(*agent.ResultMessage); !isResult {
		tt.window.Update(msg)
	}
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
		tt.rememberToolStart(m.ToolUseID, now)
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
				PlanContent:    tt.planFor(m),
				InputTruncated: truncated,
				Background:     bg,
			},
		}}
	case *agent.AskMessage:
		tt.rememberToolStart(m.ToolUseID, now)
		return []v1.EventMessage{{
			Kind: v1.EventKindAsk,
			Ts:   ts,
			Ask: &v1.EventAsk{
				ToolUseID: m.ToolUseID,
				Questions: AskQuestions(m.Questions),
			},
		}}
	case *agent.TodoMessage:
		tt.rememberToolStart(m.ToolUseID, now)
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
		nativeDuration, _ := agent.NativeDuration(m)
		duration := nativeDuration.Seconds()
		if started, ok := tt.pending[m.ToolUseID]; ok {
			if duration <= 0 && !now.IsZero() {
				duration = now.Sub(started).Seconds()
			}
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
		// A result is a turn boundary: capture the turn's visible text as the
		// fallback before resetting the window for the next turn.
		result := m.Result
		nativeDuration, _ := agent.NativeDuration(m)
		if result == "" {
			result = tt.window.Value()
		}
		tt.window.Update(m)
		return []v1.EventMessage{{
			Kind: v1.EventKindResult,
			Ts:   ts,
			Result: &v1.EventResult{
				Subtype:      m.Subtype,
				IsError:      m.IsError,
				Result:       result,
				DiffStat:     DiffStat(m.DiffStat),
				TotalCostUSD: m.TotalCostUSD,
				Duration:     nativeDuration.Seconds(),
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
			contentType, formatted := FormatToolOutput(m.Delta)
			if contentType != "" {
				ev.ContentType = contentType
			}
			if formatted != "" {
				ev.Formatted = formatted
			}
			return []v1.EventMessage{{
				Kind:            v1.EventKindToolOutputDelta,
				Ts:              ts,
				ToolOutputDelta: ev,
			}}
		}
		return nil
	case *agent.WidgetMessage:
		tt.rememberToolStart(m.ToolUseID, now)
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
				Status:          v1.EventRateLimitStatus(m.Status),
				ResetsAt:        m.ResetsAt,
				RateLimitType:   m.RateLimitType,
				Utilization:     m.Utilization,
				IsUsingOverage:  m.IsUsingOverage,
				OverageResetsAt: m.OverageResetsAt,
			},
		}}
	default:
		return nil
	}
}

// planFor resolves the plan content projection for a tool use event. The
// resolver returns content only for the visible ExitPlanMode, so superseded
// plans and all other tool uses render without a snapshot.
func (tt *ToolTimingTracker) planFor(m *agent.ToolUseMessage) string {
	if tt.planContent == nil {
		return ""
	}
	return tt.planContent(m.ToolUseID)
}

func (tt *ToolTimingTracker) rememberToolStart(toolUseID string, now time.Time) {
	if now.IsZero() {
		return
	}
	if _, exists := tt.pending[toolUseID]; !exists && len(tt.pending) == maxPendingToolTimings {
		tt.pending = make(map[string]time.Time)
	}
	tt.pending[toolUseID] = now
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

// toolInputProbe extracts run_in_background from a tool's raw JSON input.
type toolInputProbe struct {
	RunInBackground bool `json:"run_in_background"`
}
