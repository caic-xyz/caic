// Backend-neutral conversion from agent.Message to v1.EventMessage for SSE.
// Every backend (Claude, Gemini, Codex, …) uses this converter to produce the
// event stream served on /api/v1/tasks/{id}/events and /api/v1/tasks/{id}/raw_events.
package server

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// inputTruncateThreshold is the maximum byte length of a tool input JSON before it
// is omitted from the SSE stream. Clients fetch the full input on demand via
// GET /api/v1/tasks/{id}/tool/{toolUseID}.
const inputTruncateThreshold = 4096

// toolTimingTracker computes per-tool-call duration by recording the timestamp
// when each tool_use is seen and computing the delta when the corresponding
// ToolResultMessage arrives.
type toolTimingTracker struct {
	harness agent.Harness
	pending map[string]time.Time
}

func newToolTimingTracker(harness agent.Harness) *toolTimingTracker {
	return &toolTimingTracker{harness: harness, pending: make(map[string]time.Time)}
}

// convertMessage converts an agent.Message into zero or more EventMessages.
func (tt *toolTimingTracker) convertMessage(msg agent.Message, now time.Time) []v1.EventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.InitMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindInit,
			Ts:   ts,
			Init: &v1.EventInit{
				Model:        m.Model,
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
			System: &v1.EventSystem{Subtype: m.Subtype},
		}}
	case *agent.TextMessage:
		if m.Text != "" {
			return []v1.EventMessage{{
				Kind: v1.EventKindText,
				Ts:   ts,
				Text: &v1.EventText{Text: m.Text},
			}}
		}
		return nil
	case *agent.ToolUseMessage:
		tt.pending[m.ToolUseID] = now
		input := m.Input
		truncated := false
		if len(input) > inputTruncateThreshold {
			input = nil
			truncated = true
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindToolUse,
			Ts:   ts,
			ToolUse: &v1.EventToolUse{
				ToolUseID:      m.ToolUseID,
				Name:           m.Name,
				Input:          input,
				PlanContent:    m.PlanContent,
				InputTruncated: truncated,
			},
		}}
	case *agent.AskMessage:
		tt.pending[m.ToolUseID] = now
		return []v1.EventMessage{{
			Kind: v1.EventKindAsk,
			Ts:   ts,
			Ask: &v1.EventAsk{
				ToolUseID: m.ToolUseID,
				Questions: toV1AskQuestions(m.Questions),
			},
		}}
	case *agent.TodoMessage:
		tt.pending[m.ToolUseID] = now
		if todos := toV1TodoItems(m.Todos); len(todos) > 0 {
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
			DiffStat: &v1.EventDiffStat{DiffStat: toV1DiffStat(m.DiffStat)},
		}}
	case *agent.ParseErrorMessage:
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: m.Err, Line: m.Line},
		}}
	case *agent.LogMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindLog,
			Ts:   ts,
			Log:  &v1.EventLog{Line: m.Line},
		}}
	default:
		return nil
	}
}

// toV1AskQuestions converts agent.AskQuestion to v1.AskQuestion.
func toV1AskQuestions(qs []agent.AskQuestion) []v1.AskQuestion {
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

// toV1TodoItems converts agent.TodoItem to v1.TodoItem.
func toV1TodoItems(items []agent.TodoItem) []v1.TodoItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]v1.TodoItem, len(items))
	for i, t := range items {
		out[i] = v1.TodoItem{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm}
	}
	return out
}

// marshalEvent is a convenience wrapper for json.Marshal on EventMessage.
func marshalEvent(ev *v1.EventMessage) ([]byte, error) {
	return json.Marshal(ev)
}

// v1PromptToAgent converts v1.Prompt to agent.Prompt at the server boundary.
func v1PromptToAgent(p v1.Prompt) agent.Prompt {
	var images []agent.ImageData
	if len(p.Images) > 0 {
		images = make([]agent.ImageData, len(p.Images))
		for i, img := range p.Images {
			images[i] = agent.ImageData{MediaType: img.MediaType, Data: img.Data}
		}
	}
	return agent.Prompt{Text: p.Text, Images: images}
}

// toV1Harness converts agent.Harness to v1.Harness at the server boundary.
func toV1Harness(h agent.Harness) v1.Harness {
	return v1.Harness(h)
}

// toAgentHarness converts v1.Harness to agent.Harness at the server boundary.
func toAgentHarness(h v1.Harness) agent.Harness {
	return agent.Harness(h)
}

// toV1SafetyIssues converts []task.SafetyIssue to []v1.SafetyIssue at the
// server boundary.
func toV1SafetyIssues(issues []task.SafetyIssue) []v1.SafetyIssue {
	if len(issues) == 0 {
		return nil
	}
	out := make([]v1.SafetyIssue, len(issues))
	for i, si := range issues {
		out[i] = v1.SafetyIssue{File: si.File, Kind: si.Kind, Detail: si.Detail}
	}
	return out
}

// filterHistoryForReplay removes streaming delta messages that have a
// corresponding final message later in the history. TextDeltaMessage runs
// preceding a TextMessage and ThinkingDeltaMessage runs preceding a
// ThinkingMessage are omitted — the frontend uses only the final message when
// available, so the deltas are pure waste during history replay.
func filterHistoryForReplay(msgs []agent.Message) []agent.Message {
	skip := make([]bool, len(msgs))
	for i, msg := range msgs {
		switch msg.(type) {
		case *agent.TextMessage:
			for j := i - 1; j >= 0; j-- {
				if _, ok := msgs[j].(*agent.TextDeltaMessage); ok {
					skip[j] = true
				} else {
					break
				}
			}
		case *agent.ThinkingMessage:
			for j := i - 1; j >= 0; j-- {
				if _, ok := msgs[j].(*agent.ThinkingDeltaMessage); ok {
					skip[j] = true
				} else {
					break
				}
			}
		}
	}
	out := make([]agent.Message, 0, len(msgs))
	for i, msg := range msgs {
		if !skip[i] {
			out = append(out, msg)
		}
	}
	return out
}

// toV1DiffStat converts agent.DiffStat to v1.DiffStat at the server boundary.
func toV1DiffStat(ds agent.DiffStat) v1.DiffStat {
	if len(ds) == 0 {
		return nil
	}
	out := make(v1.DiffStat, len(ds))
	for i, f := range ds {
		out[i] = v1.DiffFileStat{Path: f.Path, Added: f.Added, Deleted: f.Deleted, Binary: f.Binary}
	}
	return out
}
