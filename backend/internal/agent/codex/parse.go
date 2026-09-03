// Parses Codex CLI wire messages into canonical agent events.

package codex

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/maruel/genai/base"
	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// parseMessage decodes a single line from the codex app-server output into one
// or more typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (e.g. caic_diff_stat).
//   - A JSON-RPC 2.0 notification (has "method", no "id").
//   - A JSON-RPC 2.0 response (has "id").
//
// Emitted agent.Message types:
//   - InitMessage          — thread/started or caic_session injection
//   - TextMessage          — item/completed agentMessage or plan
//   - TextDeltaMessage     — item/agentMessage/delta
//   - ThinkingMessage      — item/completed reasoning
//   - ThinkingDeltaMessage — item/reasoning/summaryTextDelta
//   - ToolUseMessage       — item/started commandExecution, fileChange, mcpToolCall, dynamicToolCall, collabAgentToolCall
//   - ToolResultMessage    — item/completed commandExecution, fileChange, mcpToolCall, dynamicToolCall, collabAgentToolCall
//   - ToolOutputDeltaMessage — commandExecution/outputDelta, mcpToolCall/progress
//   - SystemMessage        — thread/status/changed, model/rerouted, item/completed contextCompaction
//   - ResultMessage        — turn/completed, error notification
//   - DiffStatMessage      — caic_diff_stat injection
//   - RawMessage           — unrecognised wire types (preserved verbatim)
func parseMessage(line []byte) ([]agent.Message, error) {
	// Fast probe: check for "type" (caic-injected) vs "method"/"id" (JSON-RPC).
	var probe codex.MessageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines have a "type" field (not "jsonrpc").
	if probe.Type != "" {
		switch probe.Type {
		case "caic_session":
			var m agent.MetaSessionMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return []agent.Message{&agent.InitMessage{
				SessionID: m.SessionID,
				Model:     m.Model,
				Version:   m.AgentVersion,
			}}, nil
		case "caic_diff_stat":
			var m agent.DiffStatMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return []agent.Message{&m}, nil
		case "caic_exit":
			var m agent.ExitMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return []agent.Message{&m}, nil
		default:
			return []agent.Message{&agent.RawMessage{MessageType: probe.Type, Raw: append([]byte(nil), line...)}}, nil
		}
	}

	// JSON-RPC response (has "id").
	if probe.ID != nil {
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil
	}

	// JSON-RPC notification — dispatch on method.
	var msg codex.JSONRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	switch msg.Method {
	case codex.MethodThreadStarted:
		var p codex.ThreadStartedNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("thread/started params: %w", err)
		}
		return []agent.Message{&agent.InitMessage{
			SessionID: p.Thread.ID,
			Cwd:       p.Thread.CWD,
			Version:   p.Thread.CLIVersion,
		}}, nil

	case codex.MethodTurnStarted:
		return nil, nil

	case codex.MethodTurnCompleted:
		var p codex.TurnCompletedNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("turn/completed params: %w", err)
		}
		durationMs := int64(0)
		if p.Turn.Duration != nil {
			durationMs = p.Turn.Duration.AsDuration().Milliseconds()
		}
		switch p.Turn.Status {
		case codex.TurnStatusFailed, codex.TurnStatusInterrupted:
			errMsg := ""
			if p.Turn.Error != nil {
				errMsg = p.Turn.Error.Message
			}
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "result",
				IsError:     true,
				Result:      errMsg,
				DurationMs:  durationMs,
			}}, nil
		default: // completed, inProgress
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "result",
				DurationMs:  durationMs,
			}}, nil
		}

	case codex.MethodItemStarted:
		return parseItemStarted(&msg)

	case codex.MethodItemCompleted:
		return parseItemCompleted(&msg)

	case codex.MethodItemDelta:
		var p codex.AgentMessageDeltaNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("item/agentMessage/delta params: %w", err)
		}
		return []agent.Message{&agent.TextDeltaMessage{Text: p.Delta}}, nil

	case codex.MethodErrorNotification:
		var p codex.ErrorNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("error notification params: %w", err)
		}
		if p.WillRetry || p.Error == nil {
			return nil, nil
		}
		return []agent.Message{&agent.ResultMessage{
			MessageType: "result",
			Subtype:     "result",
			IsError:     true,
			Result:      p.Error.Message,
		}}, nil

	case codex.MethodAccountRateLimitsUpdated:
		return parseAccountRateLimitsUpdated(msg.Params)

	case codex.MethodThreadGoalUpdated:
		return parseThreadGoalUpdated(msg.Params)

	case codex.MethodReasoningSummaryTextDelta:
		var p codex.ReasoningSummaryTextDeltaNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("item/reasoning/summaryTextDelta params: %w", err)
		}
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: p.Delta}}, nil

	case codex.MethodCommandOutputDelta:
		var p codex.CommandExecutionOutputDeltaNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("commandExecution/outputDelta params: %w", err)
		}
		return []agent.Message{&agent.ToolOutputDeltaMessage{ToolUseID: p.ItemID, Delta: p.Delta}}, nil

	case codex.MethodMcpToolCallProgress:
		var p codex.McpToolCallProgressNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("mcpToolCall/progress params: %w", err)
		}
		return []agent.Message{&agent.ToolOutputDeltaMessage{ToolUseID: p.ItemID, Delta: p.Message}}, nil

	case codex.MethodThreadStatusChanged:
		var p codex.ThreadStatusChangedNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("thread/status/changed params: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     string(p.Status.Type),
		}}, nil

	case codex.MethodModelRerouted:
		var p codex.ModelReroutedNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("model/rerouted params: %w", err)
		}
		detail := p.FromModel + " → " + p.ToModel
		if p.Reason != "" {
			detail += " (" + string(p.Reason) + ")"
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "model_rerouted",
			Detail:      detail,
			Model:       p.ToModel,
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append([]byte(nil), line...)}}, nil
	}
}

func parseAccountRateLimitsUpdated(params json.RawMessage) ([]agent.Message, error) {
	var p codexRateLimitsUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("account/rateLimits/updated params: %w", err)
	}
	return codexRateLimitMessages(&p.RateLimits.RateLimitSnapshot, p.RateLimits.SpendControlReached), nil
}

// codexRateLimitsUpdatedNotification preserves whether a sparse notification
// explicitly set spendControlReached. The upstream DTO uses bool, which cannot
// distinguish a false recovery update from an omitted field.
type codexRateLimitsUpdatedNotification struct {
	RateLimits codexRateLimitSnapshot `json:"rateLimits"`
}

type codexRateLimitSnapshot struct {
	codex.RateLimitSnapshot

	SpendControlReached *bool `json:"spendControlReached"`
}

func parseThreadGoalUpdated(params json.RawMessage) ([]agent.Message, error) {
	var p codex.ThreadGoalUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("thread/goal/updated params: %w", err)
	}
	if p.Goal.Status != codex.ThreadGoalStatusUsageLimited {
		return nil, nil
	}
	return []agent.Message{newCodexRateLimitMessage(agent.RateLimitStatusRejected, "thread_goal", "thread_goal", 1, time.Time{})}, nil
}

func codexRateLimitMessages(s *codex.RateLimitSnapshot, spendControlReached *bool) []agent.Message {
	msgs := make([]agent.Message, 0, 4)
	if s.Primary != nil {
		msgs = append(msgs, codexWindowRateLimitMessage(s.Primary, "primary", "5h"))
	}
	if s.Secondary != nil {
		msgs = append(msgs, codexWindowRateLimitMessage(s.Secondary, "secondary", "7d"))
	}
	if s.IndividualLimit != nil {
		usedPercent := 100 - s.IndividualLimit.RemainingPercent
		status := codexRateLimitStatus(usedPercent)
		if spendControlReached != nil && *spendControlReached {
			status = agent.RateLimitStatusRejected
		}
		msgs = append(msgs, newCodexRateLimitMessage(
			status,
			"individual",
			"individual",
			codexUtilization(usedPercent),
			codexResetTime(s.IndividualLimit.ResetsAt),
		))
	} else if spendControlReached != nil {
		status := agent.RateLimitStatusAllowed
		utilization := 0.0
		if *spendControlReached {
			status = agent.RateLimitStatusRejected
			utilization = 1
		}
		msgs = append(msgs, newCodexRateLimitMessage(status, "individual", "individual", utilization, time.Time{}))
	}

	switch s.RateLimitReachedType {
	case "":
		return msgs
	case codex.RateLimitReachedTypeRateLimitReached:
		rejected := false
		for _, msg := range msgs {
			rateLimit, ok := msg.(*agent.RateLimitMessage)
			if ok && rateLimit.RateLimitType != "individual" && rateLimit.Utilization >= 1 {
				rateLimit.Status = agent.RateLimitStatusRejected
				rejected = true
			}
		}
		if rejected {
			return msgs
		}
	case codex.RateLimitReachedTypeWorkspaceOwnerUsageLimitReached,
		codex.RateLimitReachedTypeWorkspaceMemberUsageLimitReached:
		for _, msg := range msgs {
			rateLimit, ok := msg.(*agent.RateLimitMessage)
			if ok && rateLimit.RateLimitType == "individual" {
				rateLimit.Status = agent.RateLimitStatusRejected
				return msgs
			}
		}
	case codex.RateLimitReachedTypeWorkspaceOwnerCreditsDepleted,
		codex.RateLimitReachedTypeWorkspaceMemberCreditsDepleted:
		// Credit depletion is independent from rolling rate-limit windows and
		// is appended below as its own canonical quota update.
	}

	window := codexReachedQuotaWindow(s.RateLimitReachedType)
	return append(msgs, newCodexRateLimitMessage(
		agent.RateLimitStatusRejected,
		string(s.RateLimitReachedType),
		window,
		1,
		time.Time{},
	))
}

func codexWindowRateLimitMessage(w *codex.RateLimitWindow, rateLimitType, fallbackWindow string) *agent.RateLimitMessage {
	return newCodexRateLimitMessage(
		codexRateLimitStatus(w.UsedPercent),
		rateLimitType,
		codexQuotaWindow(w.WindowDurationMins, fallbackWindow),
		codexUtilization(w.UsedPercent),
		codexResetTime(w.ResetsAt),
	)
}

func newCodexRateLimitMessage(status agent.RateLimitStatus, rateLimitType, window string, utilization float64, resetsAt time.Time) *agent.RateLimitMessage {
	return &agent.RateLimitMessage{
		Status:        status,
		ResetsAt:      resetsAt,
		RateLimitType: rateLimitType,
		Utilization:   utilization,
		QuotaProvider: agent.QuotaProviderCodex,
		QuotaLabel:    "Codex",
		QuotaWindow:   window,
	}
}

func codexRateLimitStatus(usedPercent int) agent.RateLimitStatus {
	if usedPercent >= 80 {
		return agent.RateLimitStatusAllowedWarning
	}
	return agent.RateLimitStatusAllowed
}

func codexUtilization(usedPercent int) float64 {
	return float64(min(max(usedPercent, 0), 100)) / 100
}

func codexQuotaWindow(minutes int64, fallback string) string {
	const (
		minutesPerHour = 60
		minutesPerDay  = 24 * minutesPerHour
	)
	switch {
	case minutes <= 0:
		return fallback
	case minutes%minutesPerDay == 0:
		return strconv.FormatInt(minutes/minutesPerDay, 10) + "d"
	case minutes%minutesPerHour == 0:
		return strconv.FormatInt(minutes/minutesPerHour, 10) + "h"
	default:
		return strconv.FormatInt(minutes, 10) + "m"
	}
}

func codexResetTime(ts base.TimeS) time.Time {
	if ts.IsZero() {
		return time.Time{}
	}
	return ts.AsTime()
}

func codexReachedQuotaWindow(reason codex.RateLimitReachedType) string {
	switch reason {
	case codex.RateLimitReachedTypeWorkspaceOwnerCreditsDepleted,
		codex.RateLimitReachedTypeWorkspaceMemberCreditsDepleted:
		return "credits"
	case codex.RateLimitReachedTypeWorkspaceOwnerUsageLimitReached,
		codex.RateLimitReachedTypeWorkspaceMemberUsageLimitReached:
		return "individual"
	default:
		return "rate_limit"
	}
}

// parseItemStarted handles item/started notifications.
func parseItemStarted(msg *codex.JSONRPCMessage) ([]agent.Message, error) {
	var p codex.ItemStartedNotification
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/started params: %w", err)
	}
	var h codex.ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/started header: %w", err)
	}
	switch h.Type {
	case codex.ItemTypeUserMessage:
		var item codex.UserMessageItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started userMessage: %w", err)
		}
		return []agent.Message{userInputFromContent(item.Content)}, nil

	case codex.ItemTypeCommandExecution:
		var item codex.CommandExecutionItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started commandExecution: %w", err)
		}
		input, err := json.Marshal(map[string]string{"command": item.Command, "cwd": item.Cwd})
		if err != nil {
			return nil, fmt.Errorf("marshal Bash input: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      "Bash",
			Input:     input,
		}}, nil

	case codex.ItemTypeFileChange:
		var item codex.FileChangeItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started fileChange: %w", err)
		}
		input, err := json.Marshal(item.Changes)
		if err != nil {
			return nil, fmt.Errorf("marshal FileChange input: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      toolNameForChanges(item.Changes),
			Input:     input,
			Detail:    fileChangeDetail(item.Changes),
			InputView: fileChangeInputView(item.Changes),
		}}, nil

	case codex.ItemTypeMCPToolCall:
		var item codex.McpToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started mcpToolCall: %w", err)
		}
		if _, ok := agent.WidgetToolNames[item.Tool]; ok {
			return []agent.Message{agent.NewWidgetMessage(item.ID, item.Arguments)}, nil
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      item.Tool,
			Input:     item.Arguments,
		}}, nil

	case codex.ItemTypeDynamicToolCall:
		var item codex.DynamicToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started dynamicToolCall: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      item.Tool,
			Input:     item.Arguments,
		}}, nil

	case codex.ItemTypeCollabAgentToolCall:
		var item codex.CollabAgentToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started collabAgentToolCall: %w", err)
		}
		toolName := string(item.Tool)
		if toolName == "" {
			toolName = "collabAgent"
		}
		input, err := json.Marshal(map[string]string{"prompt": item.Prompt})
		if err != nil {
			return nil, fmt.Errorf("marshal collabAgent input: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      toolName,
			Input:     input,
		}}, nil

	case codex.ItemTypeImageGeneration:
		var item codex.ImageGenerationItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/started imageGeneration: %w", err)
		}
		input, err := json.Marshal(map[string]string{"revisedPrompt": item.RevisedPrompt})
		if err != nil {
			return nil, fmt.Errorf("marshal ImageGeneration input: %w", err)
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: item.ID,
			Name:      "ImageGeneration",
			Input:     input,
		}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}

// parseItemCompleted handles item/completed notifications.
func parseItemCompleted(msg *codex.JSONRPCMessage) ([]agent.Message, error) {
	var p codex.ItemCompletedNotification
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, fmt.Errorf("item/completed params: %w", err)
	}
	var h codex.ItemHeader
	if err := json.Unmarshal(p.Item, &h); err != nil {
		return nil, fmt.Errorf("item/completed header: %w", err)
	}
	switch h.Type {
	case codex.ItemTypeUserMessage:
		// Codex emits userMessage for both item/started and item/completed.
		// The started item is enough to show the prompt and avoids duplicates.
		return nil, nil

	case codex.ItemTypeAgentMessage:
		var item codex.AgentMessageItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed agentMessage: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text, Phase: string(item.Phase)}}, nil

	case codex.ItemTypeReasoning:
		var item codex.ReasoningItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed reasoning: %w", err)
		}
		text := strings.Join(item.Summary, "\n")
		return []agent.Message{&agent.ThinkingMessage{Text: text}}, nil

	case codex.ItemTypePlan:
		var item codex.PlanItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed plan: %w", err)
		}
		return []agent.Message{&agent.TextMessage{Text: item.Text}}, nil

	case codex.ItemTypeCommandExecution:
		var item codex.CommandExecutionItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed commandExecution: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Duration != nil {
			m.DurationMs = item.Duration.AsDuration().Milliseconds()
		}
		return []agent.Message{m}, nil

	case codex.ItemTypeFileChange:
		var item codex.FileChangeItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed fileChange: %w", err)
		}
		return []agent.Message{&agent.ToolResultMessage{ToolUseID: item.ID}}, nil

	case codex.ItemTypeMCPToolCall:
		var item codex.McpToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed mcpToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Error != nil {
			m.Error = item.Error.Message
		}
		return []agent.Message{m}, nil

	case codex.ItemTypeDynamicToolCall:
		var item codex.DynamicToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed dynamicToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Success != nil && !*item.Success {
			m.Error = "tool call failed"
		}
		return []agent.Message{m}, nil

	case codex.ItemTypeCollabAgentToolCall:
		var item codex.CollabAgentToolCallItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed collabAgentToolCall: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Status == codex.CollabAgentToolCallStatusFailed {
			m.Error = "collab agent tool call failed"
		}
		return []agent.Message{m}, nil

	case codex.ItemTypeContextCompaction:
		var item codex.ContextCompactionThreadItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed contextCompaction: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "compact_boundary",
		}}, nil

	case codex.ItemTypeWebSearch:
		var item codex.WebSearchItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed webSearch: %w", err)
		}
		input, err := json.Marshal(map[string]string{"query": item.Query})
		if err != nil {
			return nil, fmt.Errorf("marshal WebSearch input: %w", err)
		}
		return []agent.Message{
			&agent.ToolUseMessage{ToolUseID: item.ID, Name: "WebSearch", Input: input},
			&agent.ToolResultMessage{ToolUseID: item.ID},
		}, nil

	case codex.ItemTypeImageGeneration:
		var item codex.ImageGenerationItem
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return nil, fmt.Errorf("item/completed imageGeneration: %w", err)
		}
		m := &agent.ToolResultMessage{ToolUseID: item.ID}
		if item.Status == "failed" {
			m.Error = "image generation failed"
		}
		return []agent.Message{m}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(msg.Method), Raw: append(msg.Params[:0:0], msg.Params...)}}, nil
	}
}

func userInputFromContent(content []codex.UserInput) *agent.UserInputMessage {
	var texts []string
	for _, b := range content {
		if b.Type == codex.TurnInputTypeText && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return &agent.UserInputMessage{Text: strings.Join(texts, "\n")}
}

// toolNameForChanges returns "Write" if any change has Kind.Type == "add", else "Edit".
func toolNameForChanges(changes []codex.FileUpdateChange) string {
	for _, c := range changes {
		if c.Kind.Type == codex.PatchChangeKindTypeAdd {
			return "Write"
		}
	}
	return "Edit"
}

func fileChangeDetail(changes []codex.FileUpdateChange) string {
	if len(changes) == 0 {
		return ""
	}
	if len(changes) > 1 {
		return fmt.Sprintf("%d files", len(changes))
	}
	return path.Base(changes[0].Path)
}

func fileChangeInputView(changes []codex.FileUpdateChange) agent.ToolInputView {
	files := make([]agent.FileChange, 0, len(changes))
	for _, c := range changes {
		if c.Diff == "" {
			continue
		}
		files = append(files, agent.FileChange{Path: c.Path, Patch: c.Diff})
	}
	return agent.FileChangesInputView(files)
}
