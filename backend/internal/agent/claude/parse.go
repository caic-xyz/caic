// Claude Code stream-json parser. Converts Claude's wire format into
// backend-neutral agent.Message types.
package claude

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// outputKnownFields caches the known field sets for output wire types,
// built on first use. Uses sync.Map: few writes (once per type), many reads.
var outputKnownFields sync.Map

// unmarshalOutput unmarshals data into v and warns via fw for any unknown
// JSON fields. The name identifies the type for logging.
func unmarshalOutput(data []byte, v any, name string, fw *jsonutil.FieldWarner) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	val, ok := outputKnownFields.Load(name)
	if !ok {
		val, _ = outputKnownFields.LoadOrStore(name, jsonutil.KnownFields(reflect.ValueOf(v).Elem().Interface()))
	}
	known := val.(map[string]struct{})
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) == nil {
		fw.Warn(name, jsonutil.CollectUnknown(raw, known))
	}
	fw.WarnOverflows(name, v)
	return nil
}

// WidgetTracker tracks which content block indices are widget tools during
// streaming, enabling input_json_delta events to be emitted as WidgetDeltaMessage.
// It also accumulates partial JSON for each widget block so that the widget_code
// field can be extracted incrementally.
type WidgetTracker struct {
	// activeWidgets maps content block index → toolUseID for blocks whose
	// tool name is in agent.WidgetToolNames.
	activeWidgets map[int]string
	// accum maps content block index → accumulated partial JSON string.
	accum map[int]string
	// lastHTMLLen maps content block index → length of HTML already emitted,
	// so only new bytes are sent as deltas.
	lastHTMLLen map[int]int
	// exceeded maps content block index → true when accumulated HTML exceeds
	// agent.MaxWidgetHTMLBytes. No further deltas are emitted.
	exceeded map[int]bool
}

// NewWidgetTracker creates a new WidgetTracker.
func NewWidgetTracker() *WidgetTracker {
	return &WidgetTracker{
		activeWidgets: make(map[int]string),
		accum:         make(map[int]string),
		lastHTMLLen:   make(map[int]int),
		exceeded:      make(map[int]bool),
	}
}

// handleStreamEvent processes a stream event and returns widget messages if
// the event belongs to a tracked widget block. Returns (nil, false) if the
// event is not widget-related and should be handled by the normal path.
func (wt *WidgetTracker) handleStreamEvent(w *outputStreamEvent) ([]agent.Message, bool) {
	switch w.Event.Type {
	case "content_block_start":
		var cb contentBlockStart
		if json.Unmarshal(w.Event.ContentBlock, &cb) == nil &&
			cb.Type == "tool_use" && agent.WidgetToolNames[cb.Name] {
			wt.activeWidgets[w.Event.Index] = cb.ID
		}
		return nil, false
	case "content_block_delta":
		if w.Event.Delta != nil && w.Event.Delta.Type == "input_json_delta" {
			toolUseID, ok := wt.activeWidgets[w.Event.Index]
			if !ok {
				return nil, false
			}
			if wt.exceeded[w.Event.Index] {
				return nil, true // absorbed but no emission
			}
			wt.accum[w.Event.Index] += w.Event.Delta.PartialJSON
			html := extractPartialWidgetCode(wt.accum[w.Event.Index])
			if len(html) > agent.MaxWidgetHTMLBytes {
				wt.exceeded[w.Event.Index] = true
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
			delete(wt.activeWidgets, w.Event.Index)
			delete(wt.accum, w.Event.Index)
			delete(wt.lastHTMLLen, w.Event.Index)
			delete(wt.exceeded, w.Event.Index)
			return nil, true
		}
		return nil, false
	}
	return nil, false
}

// parseMessage decodes a single Claude Code NDJSON line into one or more
// typed agent.Messages. A single "assistant" line may contain multiple
// content blocks (text + tool_use + usage), each producing a separate message.
//
// Emitted agent.Message types:
//   - InitMessage          — system/init
//   - SystemMessage        — system subtypes (compact_boundary, model_rerouted, api_error, …)
//   - SubagentStartMessage — system/task_started
//   - SubagentEndMessage   — system/task_notification
//   - TextMessage          — assistant content text blocks
//   - TextDeltaMessage     — stream_event content_block_delta/text_delta
//   - ThinkingMessage      — assistant content thinking blocks
//   - ThinkingDeltaMessage — stream_event content_block_delta/thinking_delta
//   - ToolUseMessage       — assistant tool_use blocks (generic tools)
//   - AskMessage           — AskUserQuestion tool_use block
//   - TodoMessage          — TodoWrite tool_use block
//   - ToolResultMessage    — user message with parent_tool_use_id
//   - UserInputMessage     — user message without parent_tool_use_id
//   - UsageMessage         — assistant message usage counters
//   - ResultMessage        — result record
//   - DiffStatMessage      — caic_diff_stat injection
//   - RawMessage           — unrecognised wire types (preserved verbatim)
//
// parseMessage decodes a single Claude Code NDJSON line without widget
// tracking. Use parseMessageWithTracker for streaming sessions that need
// progressive widget rendering.
func parseMessage(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	return parseMessageWithTracker(line, nil, fw)
}

// parseMessageWithTracker decodes a single Claude Code NDJSON line with
// optional widget tracking. When wt is non-nil, content_block_start and
// input_json_delta events for widget tools produce WidgetDeltaMessage.
func parseMessageWithTracker(line []byte, wt *WidgetTracker, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var env outputTypeProbe
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case OutputSystem:
		return parseSystem(line, env.Subtype, fw)
	case OutputAssistant:
		return parseAssistant(line, fw)
	case OutputUser:
		return parseUser(line, fw)
	case OutputResult:
		var w outputResult
		if err := unmarshalOutput(line, &w, "outputResult", fw); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ResultMessage{
			MessageType:   string(w.Type),
			Subtype:       w.Subtype,
			IsError:       w.IsError,
			DurationMs:    w.DurationMs,
			DurationAPIMs: w.DurationAPIMs,
			NumTurns:      w.NumTurns,
			Result:        w.Result,
			SessionID:     w.SessionID,
			TotalCostUSD:  w.TotalCostUSD,
			Usage:         w.Usage,
			UUID:          w.UUID,
		}}, nil
	case OutputStreamEvent:
		return parseStreamEvent(line, wt, fw)
	case OutputRateLimitEvent:
		var w outputRateLimitEvent
		if err := unmarshalOutput(line, &w, "outputRateLimitEvent", fw); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.RateLimitMessage{
			Status:          w.RateLimitInfo.Status,
			ResetsAt:        w.RateLimitInfo.ResetsAt,
			RateLimitType:   w.RateLimitInfo.RateLimitType,
			Utilization:     w.RateLimitInfo.Utilization,
			IsUsingOverage:  w.RateLimitInfo.IsUsingOverage,
			OverageResetsAt: w.RateLimitInfo.OverageResetsAt,
		}}, nil
	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil
	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(env.Type), Raw: append([]byte(nil), line...)}}, nil
	}
}

func parseSystem(line []byte, subtype string, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	if SystemSubtype(subtype) == SystemInit {
		var w outputInit
		if err := unmarshalOutput(line, &w, "outputInit", fw); err != nil {
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
	var w outputSystem
	if err := unmarshalOutput(line, &w, "outputSystem", fw); err != nil {
		return nil, err
	}
	switch w.Subtype {
	case SystemTaskStarted:
		return []agent.Message{&agent.SubagentStartMessage{
			TaskID:      jsonString(w.TaskID),
			Description: jsonString(w.Description),
		}}, nil
	case SystemTaskNotification:
		return []agent.Message{&agent.SubagentEndMessage{
			TaskID: jsonString(w.TaskID),
			Status: jsonString(w.Status),
		}}, nil
	case SystemStatus, SystemTaskProgress, "turn_duration":
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

func parseAssistant(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var w outputAssistant
	if err := unmarshalOutput(line, &w, "outputAssistant", fw); err != nil {
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
			msgs = append(msgs, parseToolUseBlock(b)...)
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
		msgs = append(msgs, &agent.UsageMessage{
			Usage: u,
			Model: w.Message.Model,
		})
	}
	if len(msgs) == 0 {
		// Preserve as raw if nothing was extracted (e.g. empty content).
		msgs = append(msgs, &agent.RawMessage{MessageType: "assistant", Raw: append([]byte(nil), line...)})
	}
	return msgs, nil
}

func parseToolUseBlock(b *outputContentBlock) []agent.Message {
	switch {
	case b.Name == "Skill":
		// Skill is a Claude Code built-in that loads plugin skills into
		// context. Suppress it — internal machinery that adds noise.
		return nil
	case b.Name == "AskUserQuestion":
		var input askInput
		if json.Unmarshal(b.Input, &input) == nil && len(input.Questions) > 0 {
			return []agent.Message{&agent.AskMessage{
				ToolUseID: b.ID,
				Questions: input.Questions,
			}}
		}
		// Fall through to generic ToolUseMessage if parse fails.
	case b.Name == "TodoWrite":
		var input todoInput
		if json.Unmarshal(b.Input, &input) == nil && len(input.Todos) > 0 {
			return []agent.Message{&agent.TodoMessage{
				ToolUseID: b.ID,
				Todos:     input.Todos,
			}}
		}
	case agent.WidgetToolNames[b.Name]:
		return []agent.Message{agent.NewWidgetMessage(b.ID, b.Input)}
	}
	return []agent.Message{&agent.ToolUseMessage{
		ToolUseID: b.ID,
		Name:      b.Name,
		Input:     b.Input,
	}}
}

func parseUser(line []byte, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var w outputUser
	if err := unmarshalOutput(line, &w, "outputUser", fw); err != nil {
		return nil, err
	}
	// Claude Code sets isSynthetic on user messages injected by the runtime
	// (e.g. skill context injections). These are internal and should not be
	// shown to the end user.
	if w.IsSynthetic {
		return nil, nil
	}

	// Standard tool result: parent_tool_use_id set at the top level.
	if w.ParentToolUseID != nil {
		return []agent.Message{extractToolResult(*w.ParentToolUseID, w.Message)}, nil
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
	// Try plain text content first ("content": "hello").
	var textMsg outputUserText
	if json.Unmarshal(raw, &textMsg) == nil && textMsg.Role == "user" && textMsg.Content != "" {
		return []agent.Message{&agent.UserInputMessage{Text: textMsg.Content}}
	}
	// Block-style content ("content": [...]).
	var blockMsg outputUserBlock
	if json.Unmarshal(raw, &blockMsg) != nil || blockMsg.Role != "user" {
		return []agent.Message{&agent.UserInputMessage{}}
	}
	// Check for inline tool_result blocks (MCP tools).
	for _, b := range blockMsg.Content {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			return []agent.Message{toolResultFromBlock(&b)}
		}
	}
	// Regular user input with text/image blocks.
	ui := &agent.UserInputMessage{}
	for _, b := range blockMsg.Content {
		switch b.Type {
		case "text":
			ui.Text = b.Text
		case "image":
			if b.Source != nil {
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
func toolResultFromBlock(b *outputUserContentBlock) *agent.ToolResultMessage {
	m := &agent.ToolResultMessage{ToolUseID: b.ToolUseID}
	if b.IsError {
		for _, c := range b.Content {
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
	var msg outputToolResult
	if json.Unmarshal(raw, &msg) == nil && msg.IsError {
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text != "" {
				m.Error = c.Text
				return m
			}
		}
	}
	return m
}

func parseStreamEvent(line []byte, wt *WidgetTracker, fw *jsonutil.FieldWarner) ([]agent.Message, error) {
	var w outputStreamEvent
	if err := unmarshalOutput(line, &w, "outputStreamEvent", fw); err != nil {
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
		if w.Event.Delta == nil {
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

// jsonString extracts a JSON string value from a json.RawMessage.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}
