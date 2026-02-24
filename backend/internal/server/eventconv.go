// Conversion from internal agent.Message types to v1.ClaudeEventMessage for
// the Claude Code raw SSE stream (/api/v1/tasks/{id}/raw_events). Each backend
// has its own raw converter; this is the Claude Code one. See genericconv.go
// for the backend-neutral converter that all backends share.
package server

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
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
func (tt *toolTimingTracker) convertMessage(msg agent.Message, now time.Time) []v1.ClaudeEventMessage {
	ts := now.UnixMilli()
	switch m := msg.(type) {
	case *agent.SystemInitMessage:
		if m.Subtype == "init" {
			return []v1.ClaudeEventMessage{{
				Kind: v1.ClaudeEventKindInit,
				Ts:   ts,
				Init: &v1.ClaudeEventInit{
					Model:        m.Model,
					AgentVersion: m.Version,
					SessionID:    m.SessionID,
					Tools:        m.Tools,
					Cwd:          m.Cwd,
				},
			}}
		}
		return []v1.ClaudeEventMessage{{
			Kind:   v1.ClaudeEventKindSystem,
			Ts:     ts,
			System: &v1.ClaudeEventSystem{Subtype: m.Subtype},
		}}
	case *agent.SystemMessage:
		return []v1.ClaudeEventMessage{{
			Kind:   v1.ClaudeEventKindSystem,
			Ts:     ts,
			System: &v1.ClaudeEventSystem{Subtype: m.Subtype},
		}}
	case *agent.AssistantMessage:
		return tt.convertAssistant(m, ts, now)
	case *agent.UserMessage:
		return tt.convertUser(m, ts, now)
	case *agent.ResultMessage:
		return []v1.ClaudeEventMessage{{
			Kind: v1.ClaudeEventKindResult,
			Ts:   ts,
			Result: &v1.ClaudeEventResult{
				Subtype:      m.Subtype,
				IsError:      m.IsError,
				Result:       m.Result,
				DiffStat:     toV1DiffStat(m.DiffStat),
				TotalCostUSD: m.TotalCostUSD,
				Duration:     float64(m.DurationMs) / 1e3,
				DurationAPI:  float64(m.DurationAPIMs) / 1e3,
				NumTurns:     m.NumTurns,
				Usage: v1.ClaudeEventUsage{
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
			return []v1.ClaudeEventMessage{{
				Kind:      v1.ClaudeEventKindTextDelta,
				Ts:        ts,
				TextDelta: &v1.ClaudeEventTextDelta{Text: m.Event.Delta.Text},
			}}
		}
		return nil
	case *agent.DiffStatMessage:
		return []v1.ClaudeEventMessage{{
			Kind:     v1.ClaudeEventKindDiffStat,
			Ts:       ts,
			DiffStat: &v1.ClaudeEventDiffStat{DiffStat: toV1DiffStat(m.DiffStat)},
		}}
	case *agent.ParseErrorMessage:
		return []v1.ClaudeEventMessage{{
			Kind:  v1.ClaudeEventKindError,
			Ts:    ts,
			Error: &v1.ClaudeEventError{Err: m.Err, Line: m.Line},
		}}
	default:
		// RawMessage (tool_progress), MetaMessage, etc. — filtered.
		return nil
	}
}

func (tt *toolTimingTracker) convertAssistant(m *agent.AssistantMessage, ts int64, now time.Time) []v1.ClaudeEventMessage {
	var events []v1.ClaudeEventMessage
	for _, block := range m.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, v1.ClaudeEventMessage{
					Kind: v1.ClaudeEventKindText,
					Ts:   ts,
					Text: &v1.ClaudeEventText{Text: block.Text},
				})
			}
		case "tool_use":
			tt.pending[block.ID] = now
			switch block.Name {
			case "AskUserQuestion":
				events = append(events, v1.ClaudeEventMessage{
					Kind: v1.ClaudeEventKindAsk,
					Ts:   ts,
					Ask: &v1.ClaudeEventAsk{
						ToolUseID: block.ID,
						Questions: parseClaudeAskInput(block.Input),
					},
				})
			case "TodoWrite":
				if todo := parseClaudeTodoInput(block.ID, block.Input); todo != nil {
					events = append(events, v1.ClaudeEventMessage{
						Kind: v1.ClaudeEventKindTodo,
						Ts:   ts,
						Todo: todo,
					})
				}
			default:
				events = append(events, v1.ClaudeEventMessage{
					Kind: v1.ClaudeEventKindToolUse,
					Ts:   ts,
					ToolUse: &v1.ClaudeEventToolUse{
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
		events = append(events, v1.ClaudeEventMessage{
			Kind: v1.ClaudeEventKindUsage,
			Ts:   ts,
			Usage: &v1.ClaudeEventUsage{
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

func (tt *toolTimingTracker) convertUser(m *agent.UserMessage, ts int64, now time.Time) []v1.ClaudeEventMessage {
	// User text input (no parent tool) vs tool result.
	//
	// NOTE: Claude Code only emits UserMessage with ParentToolUseID for
	// background/async tools (e.g. Task subagent). Synchronous built-in tools
	// (Read, Edit, Grep, Glob, Bash, Write, etc.) execute internally and their
	// results are fed back to the API without emitting a "user" message on the
	// stream-json output. The frontend must infer completion for these tools
	// from subsequent events rather than waiting for an explicit toolResult.
	if m.ParentToolUseID == nil {
		ui := extractUserInput(m.Message)
		if ui.Text == "" && len(ui.Images) == 0 {
			return nil
		}
		return []v1.ClaudeEventMessage{{
			Kind:      v1.ClaudeEventKindUserInput,
			Ts:        ts,
			UserInput: &v1.ClaudeEventUserInput{Text: ui.Text, Images: ui.Images},
		}}
	}
	toolUseID := *m.ParentToolUseID
	var duration float64
	if started, ok := tt.pending[toolUseID]; ok {
		duration = now.Sub(started).Seconds()
		delete(tt.pending, toolUseID)
	}
	errText := extractToolError(m.Message)
	return []v1.ClaudeEventMessage{{
		Kind: v1.ClaudeEventKindToolResult,
		Ts:   ts,
		ToolResult: &v1.ClaudeEventToolResult{
			ToolUseID: toolUseID,
			Duration:  duration,
			Error:     errText,
		},
	}}
}

// parseTodoInput extracts typed TodoItem data from a TodoWrite tool input
// for the generic event stream.
func parseTodoInput(toolUseID string, raw json.RawMessage) *v1.EventTodo {
	var input struct {
		Todos []v1.TodoItem `json:"todos"`
	}
	if json.Unmarshal(raw, &input) != nil || len(input.Todos) == 0 {
		return nil
	}
	return &v1.EventTodo{ToolUseID: toolUseID, Todos: input.Todos}
}

// parseClaudeTodoInput extracts typed ClaudeTodoItem data from a TodoWrite
// tool input for the Claude raw stream.
func parseClaudeTodoInput(toolUseID string, raw json.RawMessage) *v1.ClaudeEventTodo {
	var input struct {
		Todos []v1.ClaudeTodoItem `json:"todos"`
	}
	if json.Unmarshal(raw, &input) != nil || len(input.Todos) == 0 {
		return nil
	}
	return &v1.ClaudeEventTodo{ToolUseID: toolUseID, Todos: input.Todos}
}

// parseAskInput extracts typed AskQuestion data from the opaque tool input
// for the generic event stream.
func parseAskInput(raw json.RawMessage) []v1.AskQuestion {
	var input struct {
		Questions []v1.AskQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Questions
	}
	return nil
}

// parseClaudeAskInput extracts typed ClaudeAskQuestion data from the opaque
// tool input for the Claude raw stream.
func parseClaudeAskInput(raw json.RawMessage) []v1.ClaudeAskQuestion {
	var input struct {
		Questions []v1.ClaudeAskQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Questions
	}
	return nil
}

// userInput holds the text and optional images extracted from a synthetic
// UserMessage. See syntheticUserInput in task.go for the two shapes:
//   - {"role":"user","content":"text"}           (text only)
//   - {"role":"user","content":[...blocks...]}   (images + optional text)
type userInput struct {
	Text   string
	Images []v1.ImageData
}

// extractUserInput extracts text and images from a user input message.
func extractUserInput(raw json.RawMessage) userInput {
	if len(raw) == 0 {
		return userInput{}
	}
	// Try text-only shape first (most common).
	var textMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &textMsg) == nil && textMsg.Role == "user" && textMsg.Content != "" {
		return userInput{Text: textMsg.Content}
	}
	// Try content-block array shape (images).
	var blockMsg struct {
		Role    string `json:"role"`
		Content []struct {
			Type   string `json:"type"`
			Text   string `json:"text,omitempty"`
			Source *struct {
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source,omitempty"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &blockMsg) == nil && blockMsg.Role == "user" {
		var ui userInput
		for _, b := range blockMsg.Content {
			switch b.Type {
			case "text":
				ui.Text = b.Text
			case "image":
				if b.Source != nil {
					ui.Images = append(ui.Images, v1.ImageData{
						MediaType: b.Source.MediaType,
						Data:      b.Source.Data,
					})
				}
			}
		}
		return ui
	}
	return userInput{}
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
