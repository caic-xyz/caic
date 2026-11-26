// Agent event conversions to API v1 SSE DTOs, closing interrupted tool calls and excluding private delivery controls and skill machinery.

package apiconv

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// InputTruncateThreshold is the maximum byte length of a tool input JSON before it
// is omitted from the SSE stream. Clients fetch the full input on demand via
// GET /api/caic/v1/tasks/{id}/tool/{toolUseID}.
const InputTruncateThreshold = 4096

// maxPendingToolTimings bounds open-tool state while converting unbounded task
// history. When full, the next unseen tool start deterministically resets the
// map, so older results report zero duration and an interruption no longer
// closes the forgotten calls.
const maxPendingToolTimings = 1024

// ToolTimingTracker computes per-tool-call duration while converting agent
// messages to API events, closes tool calls left open by an agent exit or task
// settlement, and supplies fallback result text from a turn text window. A
// turn that a TurnStartedMessage opens and a TurnEndedMessage closes without a
// result ends with a result event, as every other turn does.
//
// The harness is fixed at construction; the per-stream state
// (pending, window, openTurn) accumulates across ConvertMessage calls.
type ToolTimingTracker struct {
	taskID           string
	harness          harness.Name
	cost             task.CostTracker
	pricer           usage.ModelPricer
	pending          map[agent.ToolKey]pendingTool
	window           task.ResultTextWindow
	skillReads       agent.SkillReadTracker // Confirms inferred skill loads.
	skillTools       agent.SkillToolTracker // Hides reported skill machinery after accounting.
	presentedSession string                 // Root session start already projected by native init or restoration.
	openTurn         bool                   // A root TurnStartedMessage opened a turn that no result has ended.
}

// NewToolTimingTracker creates a tracker for converting agent messages from a
// harness into API events.
func NewToolTimingTracker(taskID string, h harness.Name, model string, pricer usage.ModelPricer) *ToolTimingTracker {
	return &ToolTimingTracker{
		taskID:  taskID,
		harness: h,
		cost:    task.CostTracker{Harness: h, Model: model},
		pricer:  pricer,
		pending: make(map[agent.ToolKey]pendingTool),
	}
}

// ConvertMessage converts an agent.Message into zero or more EventMessages.
// The physical record ID and zero-based parsed message index address its images.
func (tt *ToolTimingTracker) ConvertMessage(recordID uint64, messageIndex int, msg agent.Message, now time.Time) []v1.EventMessage {
	hidden := tt.skillTools.Observe(msg)
	// A root exit ends every open call: the agent process that would report
	// their results is gone. Close them before the exit's own events.
	var events []v1.EventMessage
	if exit, ok := msg.(*agent.ExitMessage); ok && !exit.Source.IsChild() {
		events = tt.InterruptOpenTools(now)
	}
	closed := len(events)
	// Fold timing and skill confirmation even when the generic tool is hidden.
	events = append(events, tt.convertMessage(recordID, messageIndex, msg, now)...)
	if hidden {
		// A hidden call's result is filtered too, so it is never closed.
		if m, ok := msg.(*agent.ToolUseMessage); ok {
			delete(tt.pending, agent.ToolKeyForSource(m.Provenance(), m.ToolUseID))
		}
		filtered := events[:closed]
		for i := closed; i < len(events); i++ {
			switch events[i].Kind {
			case v1.EventKindToolUse, v1.EventKindToolResult, v1.EventKindToolOutputDelta:
				continue
			default:
				filtered = append(filtered, events[i])
			}
		}
		events = filtered
	}
	for i := closed; i < len(events); i++ {
		events[i].Source = v1.EventMessageSource(*msg.Provenance())
	}
	return events
}

// InterruptOpenTools closes every open generic tool call with an interrupted
// result under the source of its tool use. Call it when no result can follow,
// such as when the host settles the task. Results are ordered by start time
// then key so the projection stays deterministic.
func (tt *ToolTimingTracker) InterruptOpenTools(now time.Time) []v1.EventMessage {
	var keys []agent.ToolKey
	for key := range tt.pending {
		if tt.pending[key].interruptible {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b agent.ToolKey) int {
		if c := tt.pending[a].started.Compare(tt.pending[b].started); c != 0 {
			return c
		}
		return strings.Compare(a.String(), b.String())
	})
	var ts int64
	if !now.IsZero() {
		ts = now.UnixMilli()
	}
	events := make([]v1.EventMessage, 0, len(keys))
	for _, key := range keys {
		open := tt.pending[key]
		delete(tt.pending, key)
		events = append(events, v1.EventMessage{
			Kind:   v1.EventKindToolResult,
			Ts:     ts,
			Source: v1.EventMessageSource(open.source),
			ToolResult: &v1.EventToolResult{
				ToolUseID:   key.ToolUseID,
				Duration:    open.elapsed(now),
				Interrupted: true,
			},
		})
	}
	return events
}

// convertMessage projects event payloads; ConvertMessage attaches their common origin.
func (tt *ToolTimingTracker) convertMessage(recordID uint64, messageIndex int, msg agent.Message, now time.Time) []v1.EventMessage {
	turnCost, hasCost := tt.cost.Observe(msg, now, tt.pricer)
	var ts int64
	if !now.IsZero() {
		ts = now.UnixMilli()
	}
	switch msg.(type) {
	case *agent.ResultMessage, *agent.TurnEndedMessage:
		// The turn's end reads the window before resetting it.
	default:
		tt.window.Update(msg)
	}
	countable, confirmed := tt.skillReads.Confirm(msg)
	// Identity-bearing session controls restore a missing native start using
	// the established init event shape. Configuration-only controls stay hidden.
	if m, ok := msg.(*agent.MetaSessionMessage); ok {
		if m.SessionID == "" || (!m.Source.IsChild() && m.SessionID == tt.presentedSession) {
			return nil
		}
		msg = &agent.InitMessage{
			MessageProvenance: m.MessageProvenance,
			SessionID:         m.SessionID, ReportedModel: m.ReportedModel,
			ReportedEffort: m.ReportedEffort, Version: m.AgentVersion,
		}
	}
	switch m := msg.(type) {
	case *agent.SessionMetadataMessage:
		return nil
	case *agent.InitMessage:
		if !m.Source.IsChild() {
			tt.presentedSession = m.SessionID
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindInit,
			Ts:   ts,
			Init: &v1.EventInit{
				ReportedModel:  m.ReportedModel,
				ReportedEffort: m.ReportedEffort,
				AgentVersion:   m.Version,
				SessionID:      m.SessionID,
				Tools:          m.Tools,
				Cwd:            m.Cwd,
				Harness:        string(tt.harness),
			},
		}}
	case *agent.SystemMessage:
		if !m.Source.IsChild() && m.Subtype == agent.SystemSubtypeContextCleared {
			tt.presentedSession = ""
		}
		subtype, ok := systemSubtype(m.Subtype)
		if !ok {
			return nil
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindSystem,
			Ts:   ts,
			System: &v1.EventSystem{
				Subtype:             subtype,
				Detail:              m.Detail,
				ContextTokensBefore: m.ContextTokensBefore,
				ContextTokensAfter:  m.ContextTokensAfter,
			},
		}}
	case *agent.TextMessage:
		if m.Text != "" || m.Source.HasIdentity() {
			return []v1.EventMessage{{
				Kind: v1.EventKindText,
				Ts:   ts,
				Text: &v1.EventText{Text: m.Text, Phase: m.Phase},
			}}
		}
		return nil
	case *agent.ToolUseMessage:
		tt.rememberToolStart(m.Provenance(), m.ToolUseID, now, true)
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
				InputTruncated: truncated,
				Background:     bg,
			},
		}}
	case *agent.AskMessage:
		tt.rememberToolStart(m.Provenance(), m.ToolUseID, now, false)
		return []v1.EventMessage{{
			Kind: v1.EventKindAsk,
			Ts:   ts,
			Ask: &v1.EventAsk{
				ToolUseID: m.ToolUseID,
				Questions: AskQuestions(m.Questions),
			},
		}}
	case *agent.TodoMessage:
		tt.rememberToolStart(m.Provenance(), m.ToolUseID, now, false)
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
		var images []v1.InputImage
		for imageIndex := range m.Images {
			url := fmt.Sprintf("/api/caic/v1/tasks/%s/attachments/%d/%d/%d", tt.taskID, recordID, messageIndex, imageIndex)
			images = append(images, InputImage(url))
		}
		return []v1.EventMessage{{
			Kind:      v1.EventKindUserInput,
			Ts:        ts,
			UserInput: &v1.Prompt{Text: m.Text, Images: images},
		}}
	case *agent.ToolResultMessage:
		nativeDuration, _ := agent.NativeDuration(m)
		duration := nativeDuration.Seconds()
		key := agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)
		if open, ok := tt.pending[key]; ok {
			if duration <= 0 {
				duration = open.elapsed(now)
			}
			delete(tt.pending, key)
		}
		events := []v1.EventMessage{{
			Kind: v1.EventKindToolResult,
			Ts:   ts,
			ToolResult: &v1.EventToolResult{
				ToolUseID: m.ToolUseID,
				Duration:  duration,
				Error:     m.Error,
			},
		}}
		for _, c := range confirmed {
			if load, ok := c.(*agent.SkillLoadMessage); ok {
				events = append(events, skillLoadEvent(load, ts))
			}
		}
		return events
	case *agent.SkillLoadMessage:
		if !countable {
			return nil
		}
		return []v1.EventMessage{skillLoadEvent(m, ts)}
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
				ReportedModel:            m.ReportedModel,
				ContextUsed:              m.ContextUsed,
				ContextWindow:            m.ContextWindow,
			},
		}}
	case *agent.TurnStartedMessage:
		if !m.Source.IsChild() {
			tt.openTurn = true
		}
		return nil
	case *agent.TurnEndedMessage:
		var result string
		if len(m.PendingToolUseIDs) == 0 {
			result = tt.window.Value()
		}
		tt.window.Update(m)
		if !tt.openTurn || m.Source.IsChild() {
			return nil
		}
		tt.openTurn = false
		return []v1.EventMessage{{
			Kind:   v1.EventKindResult,
			Ts:     ts,
			Result: &v1.EventResult{PendingToolUseIDs: m.PendingToolUseIDs, Result: result},
		}}
	case *agent.ResultMessage:
		// A result is a turn boundary: capture the turn's visible text as the
		// fallback before resetting the window for the next turn.
		if !m.Source.IsChild() {
			tt.openTurn = false
		}
		result := m.Result
		nativeDuration, _ := agent.NativeDuration(m)
		if result == "" && !m.Source.IsChild() && len(m.PendingToolUseIDs) == 0 {
			result = tt.window.Value()
		}
		tt.window.Update(m)
		var cost v1.EventCost
		if hasCost {
			cost = v1.EventCost{USD: turnCost.USD, Source: v1.EventCostSource(turnCost.Source)}
		}
		return []v1.EventMessage{{
			Kind: v1.EventKindResult,
			Ts:   ts,
			Result: &v1.EventResult{
				PendingToolUseIDs: m.PendingToolUseIDs,
				Cost:              cost,
				Subtype:           m.Subtype,
				IsError:           m.IsError,
				Result:            result,
				DiffStat:          DiffStat(m.DiffStat),
				TotalCostUSD:      m.TotalCostUSD,
				Duration:          nativeDuration.Seconds(),
				DurationAPI:       float64(m.DurationAPIMs) / 1e3,
				NumTurns:          m.NumTurns,
				Usage: v1.EventUsage{
					InputTokens:              m.Usage.InputTokens,
					OutputTokens:             m.Usage.OutputTokens,
					CacheCreationInputTokens: m.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     m.Usage.CacheReadInputTokens,
					ReasoningOutputTokens:    m.Usage.ReasoningOutputTokens,
				},
			},
		}}
	case *agent.TurnCommitSnapshotMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindCommitSnapshot,
			Ts:   ts,
			CommitSnapshot: &v1.EventCommitSnapshot{
				RepositoryCommits: repositoryCommits(m.RepositoryCommits),
				Baseline:          m.Baseline,
				ChangeStat:        changeStat(m.ChangeStat),
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
		if m.Text != "" || m.Source.HasIdentity() {
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
	case *agent.NativeSubagentMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindNativeSubagent,
			Ts:   ts,
			NativeSubagent: &v1.EventNativeSubagent{
				RunID:      m.Subagent.RunID,
				ID:         m.Subagent.ID,
				ToolUseID:  m.Subagent.ToolUseID,
				Scope:      v1.EventNativeSubagentScope(m.Subagent.Scope),
				GroupID:    m.Subagent.GroupID,
				Label:      m.Subagent.Label,
				Kind:       m.Subagent.Kind,
				Prompt:     m.Subagent.Prompt,
				Status:     v1.EventNativeSubagentStatus(m.Subagent.Status),
				Result:     m.Subagent.Result,
				Background: m.Subagent.Background,
			},
		}}
	case *agent.BackgroundCommandMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindBackgroundCommand,
			Ts:   ts,
			BackgroundCommand: &v1.EventBackgroundCommand{
				ID:         m.Command.ID,
				Label:      m.Command.Label,
				Status:     string(m.Command.Status),
				Result:     m.Command.Result,
				ExitCode:   m.Command.ExitCode,
				OutputRef:  m.Command.OutputRef,
				ToolUseID:  m.Command.ToolUseID,
				Watch:      m.Command.Watch,
				WakesAgent: m.Command.WakesAgent,
			},
		}}
	case *agent.BackgroundRetiredMessage:
		return []v1.EventMessage{{
			Kind:              v1.EventKindBackgroundRetired,
			Ts:                ts,
			BackgroundRetired: &v1.EventBackgroundRetired{ID: m.ID},
		}}
	case *agent.DiffStatMessage:
		return []v1.EventMessage{{
			Kind:     v1.EventKindDiffStat,
			Ts:       ts,
			DiffStat: &v1.EventDiffStat{DiffStat: DiffStat(m.DiffStat)},
		}}
	case *agent.ExitMessage:
		if !m.Source.IsChild() {
			tt.presentedSession = ""
		}
		if m.ExitCode == 0 {
			return nil
		}
		return []v1.EventMessage{{
			Kind:  v1.EventKindError,
			Ts:    ts,
			Error: &v1.EventError{Err: m.ExitError()},
		}}
	case *agent.LogMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindLog,
			Ts:   ts,
			Log:  &v1.EventLog{Line: m.Line},
		}}
	case *agent.SyncActivityMessage:
		return []v1.EventMessage{{
			Kind: v1.EventKindSyncActivity,
			Ts:   ts,
			SyncActivity: &v1.EventSyncActivity{
				SyncID:     m.SyncID,
				Phase:      v1.EventSyncActivityPhase(m.Phase),
				Target:     m.Target,
				Branch:     m.Branch,
				Output:     m.Output,
				Truncated:  m.Truncated,
				Status:     v1.EventSyncActivityStatus(m.Status),
				Error:      m.Error,
				DurationMS: m.DurationMS,
			},
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
		tt.rememberToolStart(m.Provenance(), m.ToolUseID, now, true)
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

func (tt *ToolTimingTracker) rememberToolStart(source *logv3.MessageSource, toolUseID string, now time.Time, interruptible bool) {
	key := agent.ToolKeyForSource(source, toolUseID)
	if key.ToolUseID == "" {
		return
	}
	if _, exists := tt.pending[key]; !exists && len(tt.pending) == maxPendingToolTimings {
		tt.pending = make(map[agent.ToolKey]pendingTool)
	}
	tt.pending[key] = pendingTool{source: *source, started: now, interruptible: interruptible}
}

func skillLoadEvent(m *agent.SkillLoadMessage, ts int64) v1.EventMessage {
	return v1.EventMessage{
		Kind:      v1.EventKindSkillLoad,
		Ts:        ts,
		SkillLoad: &v1.EventSkillLoad{Skill: m.Skill, Path: m.Path},
	}
}

func repositoryCommits(commits []logv3.RepositoryCommit) []v1.EventRepositoryCommit {
	if len(commits) == 0 {
		return nil
	}
	result := make([]v1.EventRepositoryCommit, len(commits))
	for i, commit := range commits {
		result[i] = v1.EventRepositoryCommit{
			RepositoryPath: commit.RepositoryPath,
			BranchName:     commit.BranchName,
			CommitHash:     commit.CommitHash,
		}
	}
	return result
}

func changeStat(stat *logv3.ChangeStat) *v1.EventChangeStat {
	if stat == nil {
		return nil
	}
	return &v1.EventChangeStat{
		Files:        stat.Files,
		LinesAdded:   stat.LinesAdded,
		LinesDeleted: stat.LinesDeleted,
		BinaryFiles:  stat.BinaryFiles,
	}
}

// pendingTool is a tool call whose result has not been converted yet.
type pendingTool struct {
	source  logv3.MessageSource
	started time.Time // Zero when the producer time is unknown.
	// interruptible marks a generic tool call. Questions stay answerable by a
	// later prompt and plan updates complete on their own, so neither is
	// closed by an interruption.
	interruptible bool
}

// elapsed returns the call's duration in seconds at now, or 0 when unknown.
func (p *pendingTool) elapsed(now time.Time) float64 {
	if p.started.IsZero() || now.IsZero() {
		return 0
	}
	return now.Sub(p.started).Seconds()
}

// AskQuestions converts agent ask questions to API DTOs.
func AskQuestions(qs []logv3.AskQuestion) []v1.AskQuestion {
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

// toolInputProbe extracts run_in_background from a tool's raw JSON input.
type toolInputProbe struct {
	RunInBackground bool `json:"run_in_background"`
}

// systemSubtype maps a presented agent system subtype to its API value. Private
// input-delivery controls have none.
func systemSubtype(s string) (v1.EventSystemSubtype, bool) {
	switch s {
	case agent.SystemSubtypeAPIError:
		return v1.EventSystemSubtypeAPIError, true
	case agent.SystemSubtypeCompactBoundary:
		return v1.EventSystemSubtypeCompactBoundary, true
	case agent.SystemSubtypeCompactError:
		return v1.EventSystemSubtypeCompactError, true
	case agent.SystemSubtypeCompactStart:
		return v1.EventSystemSubtypeCompactStart, true
	case agent.SystemSubtypeContextCleared:
		return v1.EventSystemSubtypeContextCleared, true
	case agent.SystemSubtypeError:
		return v1.EventSystemSubtypeError, true
	case agent.SystemSubtypeInputDeliveryFailed:
		return v1.EventSystemSubtypeInputDeliveryFailed, true
	case agent.SystemSubtypeModelRerouted:
		return v1.EventSystemSubtypeModelRerouted, true
	case agent.SystemSubtypeNotice:
		return v1.EventSystemSubtypeNotice, true
	default:
		return "", false
	}
}
