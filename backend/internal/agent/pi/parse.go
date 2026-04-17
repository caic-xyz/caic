// Pi event parser. Converts Pi's type-dispatched JSONL events into normalized
// agent.Message types.

package pi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	pi "github.com/maruel/genai/providers/pi"
)

// parseMessage decodes a single JSONL line from Pi's stdout into one or more
// typed agent.Messages.
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (caic_diff_stat, etc.)
//   - A Pi event dispatched by the "type" field (message_update, tool_execution_end, etc.)
//   - A Pi response envelope (type:"response")
//
// Emitted agent.Message types:
//   - TextDeltaMessage     — message_update (text_delta)
//   - ThinkingDeltaMessage — message_update (thinking_delta)
//   - ToolUseMessage       — message_update (toolcall_start) or tool_execution_start
//   - ToolResultMessage    — tool_execution_end
//   - ToolOutputDeltaMessage — tool_execution_update
//   - DiffStatMessage      — caic_diff_stat injection
//   - RawMessage           — unrecognised event types
func parseMessage(line []byte, _ *jsonutil.FieldWarner) ([]agent.Message, error) {
	var probe pi.LineProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// caic-injected lines.
	switch probe.Type {
	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil

	case pi.EventMessageUpdate:
		return parseMessageUpdate(line)

	case pi.EventToolExecStart:
		return parseToolExecStart(line)

	case pi.EventToolExecUpdate:
		return parseToolExecUpdate(line)

	case pi.EventToolExecEnd:
		return parseToolExecEnd(line)

	case pi.EventAgentStart, pi.EventMessageStart, pi.EventMessageEnd, pi.EventTurnStart:
		// Lifecycle events with no semantic content; skip.
		return nil, nil

	case pi.EventResponse:
		// Command responses (e.g. set_model ack); skip unless error.
		return parseResponse(line)

	case pi.EventExtensionUI:
		// Extension UI requests are passed through as RawMessage.
		// The wireFormat handles auto-responses.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(pi.EventExtensionUI),
			Raw:         append([]byte(nil), line...),
		}}, nil

	case pi.EventAgentEnd, pi.EventTurnEnd:
		// Handled by wireFormat; if we reach here (stateless replay), pass through.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(probe.Type),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}

	// Unknown event type.
	if probe.Type != "" {
		return []agent.Message{&agent.RawMessage{
			MessageType: string(probe.Type),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
	return nil, nil
}

// parseMessageUpdate dispatches on the assistantMessageEvent delta type.
func parseMessageUpdate(line []byte) ([]agent.Message, error) {
	var ev pi.MessageUpdateEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal message_update: %w", err)
	}

	delta := &ev.AssistantMessageEvent
	switch delta.Type {
	case pi.DeltaTextDelta:
		return []agent.Message{&agent.TextDeltaMessage{Text: delta.Delta}}, nil

	case pi.DeltaThinkDelta:
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: delta.Delta}}, nil

	case pi.DeltaToolStart:
		if delta.ToolCall == nil {
			return nil, nil
		}
		name := normalizeToolName(delta.ToolCall.Name)

		// Marshal tool arguments as JSON for the input field.
		var input json.RawMessage
		if delta.ToolCall.Arguments != nil {
			input, _ = json.Marshal(delta.ToolCall.Arguments)
		}

		if agent.WidgetToolNames[name] {
			return []agent.Message{agent.NewWidgetMessage(delta.ToolCall.ID, input)}, nil
		}
		return []agent.Message{&agent.ToolUseMessage{
			ToolUseID: delta.ToolCall.ID,
			Name:      name,
			Input:     input,
		}}, nil

	case pi.DeltaTextStart, pi.DeltaTextEnd, pi.DeltaThinkStart, pi.DeltaThinkEnd,
		pi.DeltaToolDelta, pi.DeltaToolEnd, pi.DeltaStart:
		// Boundary markers; skip.
		return nil, nil

	case pi.DeltaDone, pi.DeltaError:
		// Handled by wireFormat.ParseMessage; in stateless mode, pass through.
		return []agent.Message{&agent.RawMessage{
			MessageType: "message_update:" + string(delta.Type),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}

	return nil, nil
}

// parseToolExecStart converts a tool_execution_start event.
func parseToolExecStart(line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecStartEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_start: %w", err)
	}
	name := normalizeToolName(ev.ToolName)

	var input json.RawMessage
	if ev.Args != nil {
		input, _ = json.Marshal(ev.Args)
	}

	if agent.WidgetToolNames[name] {
		return []agent.Message{agent.NewWidgetMessage(ev.ToolCallID, input)}, nil
	}
	return []agent.Message{&agent.ToolUseMessage{
		ToolUseID: ev.ToolCallID,
		Name:      name,
		Input:     input,
	}}, nil
}

// parseToolExecUpdate converts a tool_execution_update event to a streaming delta.
func parseToolExecUpdate(line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecUpdateEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_update: %w", err)
	}
	if ev.PartialResult == nil {
		return nil, nil
	}
	// Try to extract a string from partialResult.
	var s string
	if json.Unmarshal(ev.PartialResult, &s) != nil {
		s = string(ev.PartialResult)
	}
	if s == "" {
		return nil, nil
	}
	return []agent.Message{&agent.ToolOutputDeltaMessage{
		ToolUseID: ev.ToolCallID,
		Delta:     s,
	}}, nil
}

// parseToolExecEnd converts a tool_execution_end event.
func parseToolExecEnd(line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_end: %w", err)
	}
	msg := &agent.ToolResultMessage{ToolUseID: ev.ToolCallID}
	if ev.IsError {
		// Try to extract error string from result.
		if ev.Result != nil {
			var s string
			if json.Unmarshal(ev.Result, &s) == nil && s != "" {
				msg.Error = s
			} else {
				msg.Error = "tool execution failed"
			}
		} else {
			msg.Error = "tool execution failed"
		}
	}
	return []agent.Message{msg}, nil
}

// parseResponse handles response envelopes. A failed prompt is terminal since
// Pi will never emit agent events; other failures are passed through as raw.
func parseResponse(line []byte) ([]agent.Message, error) {
	var resp pi.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if !resp.Success && resp.Error != "" {
		if resp.Command == pi.CmdPrompt {
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "error",
				IsError:     true,
				Result:      resp.Error,
			}}, nil
		}
		return []agent.Message{&agent.RawMessage{
			MessageType: "response:" + string(resp.Command),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
	return nil, nil
}

// normalizeToolName maps Pi tool names to caic canonical names.
func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	switch lower {
	case "bash", "shell", "terminal", "run_shell_command":
		return "Bash"
	case "edit", "replace", "edit_file":
		return "Edit"
	case "write", "write_file":
		return "Write"
	case "read", "read_file":
		return "Read"
	case "glob", "find_files":
		return "Glob"
	case "grep", "search", "grep_search":
		return "Grep"
	case "list", "list_directory", "ls":
		return "ListDirectory"
	case "webfetch", "web_fetch":
		return "WebFetch"
	case "websearch", "web_search", "google_web_search":
		return "WebSearch"
	case "todowrite", "todo_write":
		return "TodoWrite"
	case "task", "agent":
		return "Agent"
	case "patch":
		return "Edit"
	case "notebook_edit":
		return "NotebookEdit"
	}
	// Check widget tools before returning original.
	if agent.WidgetToolNames[name] {
		return name
	}
	return name
}
