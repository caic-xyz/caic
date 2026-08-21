// Claude Code stream-json parser. Converts Claude's wire format into backend-neutral agent.Message types.

package claudecode

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// toAgentUsage converts the wire MsgUsage to the backend-neutral agent.Usage.
func toAgentUsage(u *claudecode.MsgUsage) agent.Usage {
	usage := agent.Usage{
		InputTokens:              int(u.InputTokens),
		OutputTokens:             int(u.OutputTokens),
		CacheCreationInputTokens: int(u.CacheCreationInputTokens),
		CacheReadInputTokens:     int(u.CacheReadInputTokens),
	}
	// Anthropic prompt cache has a 5-minute TTL by default.
	// See https://platform.claude.com/docs/en/build-with-claude/prompt-caching
	usage.CacheTTLSeconds = 300
	if u.CacheCreation.Ephemeral5mInputTokens > 0 &&
		u.CacheCreation.Ephemeral5mInputTokens >= u.CacheCreation.Ephemeral1hInputTokens {
		usage.CacheTTLSeconds = 300
	} else if u.CacheCreation.Ephemeral1hInputTokens > 0 {
		usage.CacheTTLSeconds = 3600
	}
	return usage
}

const (
	maxTrackedWidgetBlocks     = 64
	maxBufferedWidgetJSONBytes = 1 << 20
)

// WidgetTracker tracks which content block indices are widget tools during
// streaming, enabling input_json_delta events to be emitted as WidgetDeltaMessage.
// It bounds both tracked blocks and aggregate partial JSON so malformed streams
// cannot retain unbounded parser state.
type WidgetTracker struct {
	// activeWidgets maps content block index → toolUseID for blocks whose
	// tool name is in agent.WidgetToolNames.
	activeWidgets map[int]string
	// accum maps content block index → accumulated partial JSON string.
	accum map[int]string
	// lastHTMLLen maps content block index → length of HTML already emitted,
	// so only new bytes are sent as deltas.
	lastHTMLLen map[int]int
	// exceeded maps content block index → true when accumulated HTML or JSON
	// exceeds its limit. No further deltas are emitted.
	exceeded map[int]struct{}
	// bufferedJSONBytes is the aggregate size of accum values.
	bufferedJSONBytes int
}

// NewWidgetTracker creates a new WidgetTracker.
func NewWidgetTracker() *WidgetTracker {
	return &WidgetTracker{
		activeWidgets: make(map[int]string),
		accum:         make(map[int]string),
		lastHTMLLen:   make(map[int]int),
		exceeded:      make(map[int]struct{}),
	}
}

func (wt *WidgetTracker) forgetAccum(index int) {
	wt.bufferedJSONBytes -= len(wt.accum[index])
	delete(wt.accum, index)
}

func (wt *WidgetTracker) forget(index int) {
	wt.forgetAccum(index)
	delete(wt.activeWidgets, index)
	delete(wt.lastHTMLLen, index)
	delete(wt.exceeded, index)
}

// handleStreamEvent processes a stream event and returns widget messages if
// the event belongs to a tracked widget block. Returns (nil, false) if the
// event is not widget-related and should be handled by the normal path.
func (wt *WidgetTracker) handleStreamEvent(w *claudecode.OutputStreamEventMsg) ([]agent.Message, bool) {
	switch w.Event.Type {
	case "content_block_start":
		cb := w.Event.ContentBlock
		if cb.Type == "tool_use" && func() bool { _, ok := agent.WidgetToolNames[cb.Name]; return ok }() {
			wt.forget(w.Event.Index)
			if len(wt.activeWidgets) < maxTrackedWidgetBlocks {
				wt.activeWidgets[w.Event.Index] = cb.ID
			}
		}
		return nil, false
	case "content_block_delta":
		if w.Event.Delta.Type == "input_json_delta" {
			toolUseID, ok := wt.activeWidgets[w.Event.Index]
			if !ok {
				return nil, false
			}
			if _, ok := wt.exceeded[w.Event.Index]; ok {
				return nil, true // absorbed but no emission
			}
			partial := w.Event.Delta.PartialJSON
			if len(partial) > maxBufferedWidgetJSONBytes-wt.bufferedJSONBytes {
				wt.exceeded[w.Event.Index] = struct{}{}
				wt.forgetAccum(w.Event.Index)
				return nil, true
			}
			wt.accum[w.Event.Index] += partial
			wt.bufferedJSONBytes += len(partial)
			html := extractPartialWidgetCode(wt.accum[w.Event.Index])
			if len(html) > agent.MaxWidgetHTMLBytes {
				wt.exceeded[w.Event.Index] = struct{}{}
				wt.forgetAccum(w.Event.Index)
				return nil, true
			}
			prevLen := wt.lastHTMLLen[w.Event.Index]
			if len(html) > prevLen {
				delta := html[prevLen:]
				wt.lastHTMLLen[w.Event.Index] = len(html)
				return []agent.Message{&agent.WidgetDeltaMessage{
					ToolUseID: toolUseID,
					Delta:     delta,
				}}, true
			}
			return nil, true // absorbed, no new HTML yet
		}
		return nil, false
	case "content_block_stop":
		if _, ok := wt.activeWidgets[w.Event.Index]; ok {
			wt.forget(w.Event.Index)
			return nil, true
		}
		return nil, false
	}
	return nil, false
}

// parseMessageWithTracker decodes a single Claude Code NDJSON line with
// optional widget tracking. When wt is non-nil, content_block_start and
// input_json_delta events for widget tools produce WidgetDeltaMessage.
func parseMessageWithTracker(line []byte, wt *WidgetTracker) ([]agent.Message, error) {
	var env claudecode.OutputTypeProbe
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case claudecode.OutputSystem:
		return parseSystem(line, env.Subtype)
	case claudecode.OutputAssistant:
		return parseAssistant(line)
	case claudecode.OutputUser:
		return parseUser(line)
	case claudecode.OutputResult:
		var w claudecode.OutputResultMsg
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		usage := toAgentUsage(&w.Usage)
		usage.ReasoningOutputTokens = resultThinkingTokens(line)
		return []agent.Message{&agent.ResultMessage{
			MessageType:   string(w.Type),
			Subtype:       string(w.Subtype),
			IsError:       w.IsError,
			DurationMs:    w.Duration.AsDuration().Milliseconds(),
			DurationAPIMs: w.DurationAPI.AsDuration().Milliseconds(),
			NumTurns:      w.NumTurns,
			Result:        w.Result,
			SessionID:     w.SessionID,
			TotalCostUSD:  w.TotalCostUSD,
			Usage:         usage,
			UUID:          w.UUID,
		}}, nil
	case claudecode.OutputStreamEvent:
		return parseStreamEvent(line, wt)
	case claudecode.OutputRateLimitEvent:
		var w claudecode.OutputRateLimitEventMsg
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		// Claude Code rate-limit events describe its OAuth subscription. Keep
		// them under claudecode rather than anthropic, which represents direct
		// Anthropic API usage and has independent quotas.
		return []agent.Message{&agent.RateLimitMessage{
			Status:          agent.RateLimitStatus(w.RateLimitInfo.Status),
			ResetsAt:        epochSecondsToTime(w.RateLimitInfo.ResetsAt),
			RateLimitType:   string(w.RateLimitInfo.RateLimitType),
			Utilization:     w.RateLimitInfo.Utilization,
			IsUsingOverage:  w.RateLimitInfo.IsUsingOverage,
			OverageResetsAt: epochSecondsToTime(w.RateLimitInfo.OverageResetsAt),
			QuotaProvider:   agent.QuotaProviderClaudeCode,
			QuotaLabel:      "Claude Code",
			QuotaWindow:     canonicalQuotaWindow(w.RateLimitInfo.RateLimitType),
		}}, nil
	case claudecode.OutputControlRequest:
		return parseControlRequest(line)
	case agent.PendingUserActionMessageType:
		var m agent.PendingUserActionMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	case "caic_stripped_env":
		var m agent.StrippedEnvMessage
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
		return []agent.Message{&agent.RawMessage{MessageType: string(env.Type), Raw: append([]byte(nil), line...)}}, nil
	}
}

func epochSecondsToTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	sec := int64(seconds)
	nsec := int64((seconds - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// canonicalQuotaWindow translates Claude Code's protocol-specific window type
// to the provider-neutral identifier used by the usage tracker.
func canonicalQuotaWindow(rateLimitType claudecode.RateLimitType) string {
	switch rateLimitType {
	case claudecode.RateLimitFiveHour:
		return "5h"
	case claudecode.RateLimitSevenDay, claudecode.RateLimitSevenDayOpus, claudecode.RateLimitSevenDaySonnet, claudecode.RateLimitSevenDayOverage:
		return "7d"
	case claudecode.RateLimitOverage:
		// Overage is a pay-as-you-go billing window, not one of the
		// subscription windows fetched as 5h or 7d.
		return "overage"
	default:
		return string(rateLimitType)
	}
}

func parseControlRequest(line []byte) ([]agent.Message, error) {
	var w claudecode.OutputControlRequestMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	can, ok := decodeCanUseTool(w)
	if !ok || can.Subtype != claudecode.ControlCanUseTool || can.ToolName != "AskUserQuestion" {
		return []agent.Message{&agent.RawMessage{MessageType: string(w.Type), Raw: append([]byte(nil), line...)}}, nil
	}
	inputRaw, err := rawObject(can.Input)
	if err != nil {
		return nil, fmt.Errorf("marshal AskUserQuestion input: %w", err)
	}
	input, ok := decodeAskUserQuestionInput(inputRaw)
	if !ok || len(input.Questions) == 0 {
		return []agent.Message{&agent.RawMessage{MessageType: string(w.Type), Raw: append([]byte(nil), line...)}}, nil
	}
	return []agent.Message{&agent.PendingUserActionMessage{
		MessageType: agent.PendingUserActionMessageType,
		Action: agent.PendingUserAction{
			Kind:      agent.PendingUserActionAskUserQuestion,
			RequestID: w.RequestID,
			ToolUseID: can.ToolUseID,
			Ask: agent.PendingAskAction{
				Questions: askQuestionsFromClaude(input.Questions),
			},
		},
	}}, nil
}

func decodeCanUseTool(m claudecode.OutputControlRequestMsg) (claudecode.ControlReqCanUseTool, bool) {
	can, err := m.DecodeCanUseTool()
	return can, err == nil
}

func decodeAskUserQuestionInput(raw json.RawMessage) (claudecode.AskUserQuestionInput, bool) {
	var input claudecode.AskUserQuestionInput
	err := json.Unmarshal(raw, &input)
	return input, err == nil
}

func parseSystem(line []byte, subtype string) ([]agent.Message, error) {
	if claudecode.SystemSubtype(subtype) == claudecode.SystemInit {
		var w claudecode.OutputInitMsg
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.InitMessage{
			SessionID: w.SessionID,
			Cwd:       w.Cwd,
			Tools:     w.Tools,
			Model:     w.Model,
			Version:   w.Version,
		}}, nil
	}
	if subtype == "thinking_tokens" {
		return nil, nil
	}
	var w claudecode.OutputSystemMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	switch w.Subtype {
	case claudecode.SystemTaskStarted:
		return []agent.Message{&agent.SubagentStartMessage{
			TaskID:      w.TaskID,
			Description: w.Description,
		}}, nil
	case "task_updated":
		if w.Patch.Status != "" {
			return []agent.Message{&agent.SubagentEndMessage{
				TaskID: w.TaskID,
				Status: string(w.Patch.Status),
			}}, nil
		}
		return nil, nil
	case claudecode.SystemTaskNotification:
		return []agent.Message{&agent.SubagentEndMessage{
			TaskID: w.TaskID,
			Status: w.Status,
		}}, nil
	case claudecode.SystemStatus, claudecode.SystemTaskProgress, claudecode.SystemCommandsChanged, claudecode.SystemTurnDuration:
		return nil, nil
	default:
		return []agent.Message{&agent.SystemMessage{
			MessageType: string(w.Type),
			Subtype:     string(w.Subtype),
			SessionID:   w.SessionID,
			UUID:        w.UUID,
		}}, nil
	}
}

func parseAssistant(line []byte) ([]agent.Message, error) {
	var w claudecode.OutputAssistantMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	var msgs []agent.Message
	for i := range w.Message.Content {
		b := &w.Message.Content[i]
		switch b.Type {
		case "text":
			if b.Text != "" {
				msgs = append(msgs, &agent.TextMessage{Text: b.Text})
			}
		case "tool_use":
			toolMsgs, err := parseToolUseBlock(b)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, toolMsgs...)
		case "thinking":
			if b.Thinking != "" {
				msgs = append(msgs, &agent.ThinkingMessage{Text: b.Thinking})
			}
		case "server_tool_use", "web_search_tool_result", "tool_result":
			continue
		}
	}
	u := w.Message.Usage
	if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0 {
		usage := toAgentUsage(&u)
		usage.ReasoningOutputTokens = assistantThinkingTokens(line)
		msgs = append(msgs, &agent.UsageMessage{
			Usage: usage,
			Model: w.Message.Model,
		})
	}
	if len(msgs) == 0 {
		// Preserve as raw if nothing was extracted (e.g. empty content).
		msgs = append(msgs, &agent.RawMessage{MessageType: "assistant", Raw: append([]byte(nil), line...)})
	}
	return msgs, nil
}

func parseToolUseBlock(b *claudecode.OutputContentBlock) ([]agent.Message, error) {
	inputRaw, err := rawObject(b.Input)
	if err != nil {
		return nil, fmt.Errorf("marshal %s input: %w", b.Name, err)
	}
	switch {
	case b.Name == "Skill":
		// Skill is a Claude Code built-in that loads plugin skills into
		// context. Suppress it — internal machinery that adds noise.
		return nil, nil
	case b.Name == "AskUserQuestion":
		var input claudecode.AskUserQuestionInput
		if json.Unmarshal(inputRaw, &input) == nil && len(input.Questions) > 0 {
			return []agent.Message{&agent.AskMessage{
				ToolUseID: b.ID,
				Questions: askQuestionsFromClaude(input.Questions),
			}}, nil
		}
		// Fall through to generic ToolUseMessage if parse fails.
	case b.Name == "TodoWrite":
		var input claudecode.TodoWriteInput
		if json.Unmarshal(inputRaw, &input) == nil && len(input.Todos) > 0 {
			return []agent.Message{&agent.TodoMessage{
				ToolUseID: b.ID,
				Todos:     todoItemsFromClaude(input.Todos),
			}}, nil
		}
	case func() bool { _, ok := agent.WidgetToolNames[b.Name]; return ok }():
		return []agent.Message{agent.NewWidgetMessage(b.ID, inputRaw)}, nil
	}
	use := &agent.ToolUseMessage{
		ToolUseID: b.ID,
		Name:      b.Name,
		Input:     inputRaw,
	}
	addEditInputView(use)
	return []agent.Message{use}, nil
}

func addEditInputView(use *agent.ToolUseMessage) {
	if use == nil {
		return
	}
	name := strings.ToLower(use.Name)
	if name != "edit" && name != "multiedit" {
		return
	}
	p, replacements, ok := parseEditInput(use.Input)
	if !ok {
		return
	}
	use.Detail = path.Base(p)
	use.InputView = agent.FileChangesInputViewFromReplacements(p, replacements)
}

func parseEditInput(raw json.RawMessage) (string, []agent.TextReplacement, bool) {
	if len(raw) == 0 {
		return "", nil, false
	}
	var multi claudecode.MultiEditInput
	if json.Unmarshal(raw, &multi) == nil && multi.FilePath != "" && len(multi.Edits) > 0 {
		replacements := make([]agent.TextReplacement, 0, len(multi.Edits))
		for _, edit := range multi.Edits {
			if edit.OldString == "" {
				return "", nil, false
			}
			replacements = append(replacements, agent.TextReplacement{OldText: edit.OldString, NewText: edit.NewString})
		}
		return multi.FilePath, replacements, true
	}
	var input claudecode.EditInput
	if json.Unmarshal(raw, &input) != nil || input.FilePath == "" || input.OldString == "" {
		return "", nil, false
	}
	return input.FilePath, []agent.TextReplacement{{
		OldText: input.OldString,
		NewText: input.NewString,
	}}, true
}

func askQuestionsFromClaude(in []claudecode.AskUserQuestion) []agent.AskQuestion {
	out := make([]agent.AskQuestion, len(in))
	for i := range in {
		out[i] = agent.AskQuestion{
			Question:    in[i].Question,
			Header:      in[i].Header,
			MultiSelect: in[i].MultiSelect,
			Options:     make([]agent.AskOption, len(in[i].Options)),
		}
		for j := range in[i].Options {
			out[i].Options[j] = agent.AskOption{
				Label:       in[i].Options[j].Label,
				Description: in[i].Options[j].Description,
			}
		}
	}
	return out
}

func todoItemsFromClaude(in []claudecode.TodoWriteItem) []agent.TodoItem {
	out := make([]agent.TodoItem, len(in))
	for i := range in {
		out[i] = agent.TodoItem{
			Content:    in[i].Content,
			Status:     in[i].Status,
			ActiveForm: in[i].ActiveForm,
		}
	}
	return out
}

func rawObject(m map[string]json.RawMessage) (json.RawMessage, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func parseUser(line []byte) ([]agent.Message, error) {
	var w claudecode.OutputUserMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	// Claude Code sets isSynthetic on user messages injected by the runtime
	// (e.g. skill context injections). These are internal and should not be
	// shown to the end user.
	if w.IsSynthetic {
		return nil, nil
	}

	// Standard tool result: parent_tool_use_id set at the top level.
	if w.ParentToolUseID != "" {
		return []agent.Message{extractToolResult(w.ParentToolUseID, w.Message)}, nil
	}

	// Parse the message body once to handle all remaining cases.
	return parseUserMessage(w.Message), nil
}

// parseUserMessage dispatches on the message body shape. It handles plain text
// user input, block-style user input (text + images), and inline tool_result
// content blocks (MCP tools that arrive without parent_tool_use_id).
func parseUserMessage(raw json.RawMessage) []agent.Message {
	if len(raw) == 0 {
		return []agent.Message{&agent.UserInputMessage{}}
	}
	var text claudecode.OutputUserText
	if err := json.Unmarshal(raw, &text); err == nil && text.Role == "user" && text.Content != "" {
		return []agent.Message{&agent.UserInputMessage{Text: text.Content}}
	}

	// Block-style content ("content": [...]).
	var blockMsg claudecode.OutputUserBlock
	if err := json.Unmarshal(raw, &blockMsg); err != nil || blockMsg.Role != "user" {
		return []agent.Message{&agent.UserInputMessage{}}
	}
	// Check for inline tool_result blocks (MCP tools).
	for i := range blockMsg.Content {
		b := &blockMsg.Content[i]
		if b.Type == "tool_result" && b.ToolUseID != "" {
			return []agent.Message{toolResultFromBlock(b)}
		}
	}
	// Regular user input with text/image blocks.
	ui := &agent.UserInputMessage{}
	for i := range blockMsg.Content {
		b := &blockMsg.Content[i]
		switch b.Type {
		case "text":
			ui.Text = b.Text
		case "image":
			if b.Source.Type != "" {
				ui.Images = append(ui.Images, agent.ImageData{
					MediaType: b.Source.MediaType,
					Data:      b.Source.Data,
				})
			}
		}
	}
	return []agent.Message{ui}
}

// toolResultFromBlock converts an inline tool_result content block to a ToolResultMessage.
func toolResultFromBlock(b *claudecode.OutputUserContentBlock) *agent.ToolResultMessage {
	m := &agent.ToolResultMessage{ToolUseID: b.ToolUseID}
	if b.IsError {
		blocks := b.Content.TextBlocks()
		for i := range blocks {
			c := &blocks[i]
			if c.Type == "text" && c.Text != "" {
				m.Error = c.Text
				return m
			}
		}
	}
	return m
}

// extractToolResult builds a ToolResultMessage from the top-level
// parent_tool_use_id path (standard Claude Code tools).
func extractToolResult(toolUseID string, raw json.RawMessage) *agent.ToolResultMessage {
	m := &agent.ToolResultMessage{ToolUseID: toolUseID}
	if len(raw) == 0 {
		return m
	}
	var msg claudecode.OutputToolResult
	if json.Unmarshal(raw, &msg) == nil && msg.IsError {
		blocks := msg.Content.TextBlocks()
		for i := range blocks {
			c := &blocks[i]
			if c.Type == "text" && c.Text != "" {
				m.Error = c.Text
				return m
			}
		}
	}
	return m
}

func parseStreamEvent(line []byte, wt *WidgetTracker) ([]agent.Message, error) {
	var w claudecode.OutputStreamEventMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}

	// Let the widget tracker handle the event first (if present).
	if wt != nil {
		if msgs, handled := wt.handleStreamEvent(&w); handled {
			return msgs, nil
		}
	}

	switch w.Event.Type {
	case "content_block_delta":
		if w.Event.Delta.Type == "" {
			return nil, nil
		}
		switch w.Event.Delta.Type {
		case "text_delta":
			if w.Event.Delta.Text != "" {
				return []agent.Message{&agent.TextDeltaMessage{Text: w.Event.Delta.Text}}, nil
			}
			return nil, nil
		case "thinking_delta":
			if w.Event.Delta.Thinking != "" {
				return []agent.Message{&agent.ThinkingDeltaMessage{Text: w.Event.Delta.Thinking}}, nil
			}
			return nil, nil
		case "input_json_delta", "signature_delta":
			return nil, nil
		default:
			return nil, nil
		}
	case "content_block_start", "content_block_stop",
		"message_start", "message_stop", "message_delta", "ping":
		if w.Event.Type == "message_delta" && !w.Event.Usage.IsZero() {
			usage := toAgentUsage(&w.Event.Usage)
			usage.ReasoningOutputTokens = int(w.Event.Usage.OutputTokensDetails.ThinkingTokens)
			if hasUsage(usage) {
				return []agent.Message{&agent.UsageMessage{Usage: usage}}, nil
			}
		}
		return nil, nil
	case "error":
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     "api_error",
		}}, nil
	default:
		return []agent.Message{&agent.RawMessage{MessageType: "stream_event", Raw: append([]byte(nil), line...)}}, nil
	}
}

func assistantThinkingTokens(line []byte) int {
	var p struct {
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &p) != nil {
		return 0
	}
	return usageThinkingTokens(p.Message.Usage)
}

func resultThinkingTokens(line []byte) int {
	var p struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(line, &p) != nil {
		return 0
	}
	return usageThinkingTokens(p.Usage)
}

func systemThinkingTokenEstimate(line []byte) (int, bool) {
	var w claudecode.OutputSystemMsg
	if json.Unmarshal(line, &w) != nil ||
		w.Type != claudecode.OutputSystem ||
		w.Subtype != claudecode.SystemThinkingTokens ||
		w.EstimatedTokensDelta <= 0 {
		return 0, false
	}
	return int(w.EstimatedTokensDelta), true
}

func usageThinkingTokens(raw json.RawMessage) int {
	var p struct {
		OutputTokensDetails struct {
			ThinkingTokens int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return 0
	}
	return p.OutputTokensDetails.ThinkingTokens
}

func hasUsage(u agent.Usage) bool {
	return u.InputTokens > 0 ||
		u.OutputTokens > 0 ||
		u.CacheCreationInputTokens > 0 ||
		u.CacheReadInputTokens > 0 ||
		u.ReasoningOutputTokens > 0
}

// extractPartialWidgetCode extracts the widget_code value from a partially
// accumulated JSON string. It scans for the "widget_code":" prefix and then
// reads a JSON string value, handling escape sequences. If the string is
// unterminated, everything up to the end is returned.
func extractPartialWidgetCode(partial string) string {
	// Find the start of the widget_code value.
	const marker = `"widget_code":"`
	idx := strings.Index(partial, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	// Read a JSON string value (handle escapes).
	var sb strings.Builder
	for i := start; i < len(partial); i++ {
		c := partial[i]
		if c == '\\' && i+1 < len(partial) {
			next := partial[i+1]
			switch next {
			case '"', '\\', '/':
				sb.WriteByte(next)
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte('\\')
				sb.WriteByte(next)
			}
			i++
			continue
		}
		if c == '"' {
			// Terminated string.
			return sb.String()
		}
		sb.WriteByte(c)
	}
	// Unterminated — return what we have so far.
	return sb.String()
}
