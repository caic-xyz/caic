// Tests and benchmarks shared ACP content, tools, prompt correlation, replay ownership, and bounded final messages.

package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Parallel()
	t.Run("AgentMessageChunk", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Hello world"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		td, ok := msgs[0].(*agent.TextDeltaMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.TextDeltaMessage", msgs[0])
		}
		if td.Text != "Hello world" {
			t.Errorf("Text = %q, want %q", td.Text, "Hello world")
		}
	})

	t.Run("AgentThoughtChunk", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "agent_thought_chunk",
					"content":       map[string]any{"type": "text", "text": "Let me think..."},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		td, ok := msgs[0].(*agent.ThinkingDeltaMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ThinkingDeltaMessage", msgs[0])
		}
		if td.Text != "Let me think..." {
			t.Errorf("Text = %q", td.Text)
		}
	})

	t.Run("ToolCall", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    "call_1",
					"title":         "bash",
					"kind":          "execute",
					"status":        "pending",
					"rawInput":      map[string]any{"command": "ls -la"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolUseMessage", msgs[0])
		}
		if tu.ToolUseID != "call_1" {
			t.Errorf("ToolUseID = %q", tu.ToolUseID)
		}
		if tu.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", tu.Name)
		}
	})

	t.Run("ToolCallUpdateCompleted", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_1",
					"status":        "completed",
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "call_1" {
			t.Errorf("ToolUseID = %q", tr.ToolUseID)
		}
		if tr.Error != "" {
			t.Errorf("Error = %q, want empty", tr.Error)
		}
	})

	t.Run("ToolCallUpdateFailed", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_2",
					"status":        "failed",
					"content": []any{
						map[string]any{
							"type":    "content",
							"content": map[string]any{"type": "text", "text": "permission denied"},
						},
					},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.Error != "permission denied" {
			t.Errorf("Error = %q, want %q", tr.Error, "permission denied")
		}
	})

	t.Run("ToolCallInProgressWithRawInputNoContent", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_5",
					"title":         "read",
					"kind":          "read",
					"status":        "in_progress",
					"rawInput":      map[string]any{"filePath": "/workspace/main.go"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		// Should emit ToolUseMessage with the real input.
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolUseMessage", msgs[0])
		}
		if tu.Name != "Read" {
			t.Errorf("Name = %q, want Read", tu.Name)
		}
		if string(tu.Input) != `{"filePath":"/workspace/main.go"}` {
			t.Errorf("Input = %s, want filePath", tu.Input)
		}
	})

	t.Run("ToolCallInProgressWithOutput", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_1",
					"title":         "bash",
					"kind":          "execute",
					"status":        "in_progress",
					"rawInput":      map[string]any{"command": "ls"},
					"content": []any{
						map[string]any{
							"type":    "content",
							"content": map[string]any{"type": "text", "text": "output line"},
						},
					},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		// Now emits both ToolUseMessage and ToolOutputDeltaMessage.
		if len(msgs) != 2 {
			t.Fatalf("msgs = %d, want 2 (ToolUse + ToolOutputDelta)", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.ToolUseMessage", msgs[0])
		}
		if tu.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", tu.Name)
		}
		td, ok := msgs[1].(*agent.ToolOutputDeltaMessage)
		if !ok {
			t.Fatalf("msgs[1] type = %T, want *agent.ToolOutputDeltaMessage", msgs[1])
		}
		if td.Delta != "output line" {
			t.Errorf("Delta = %q, want %q", td.Delta, "output line")
		}
	})

	t.Run("PlanUpdate", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "plan",
					"entries": []any{
						map[string]any{"status": "completed", "content": "Read error logs"},
						map[string]any{"status": "in_progress", "content": "Fix the bug"},
						map[string]any{"status": "pending", "content": "Add tests"},
					},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tm, ok := msgs[0].(*agent.TodoMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.TodoMessage", msgs[0])
		}
		if len(tm.Todos) != 3 {
			t.Fatalf("Todos = %d, want 3", len(tm.Todos))
		}
		if tm.Todos[0].Content != "Read error logs" || tm.Todos[0].Status != "completed" {
			t.Errorf("Todos[0] = %+v", tm.Todos[0])
		}
	})

	t.Run("UsageUpdate", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "usage_update",
					"used":          45000,
					"size":          200000,
					"cost":          map[string]any{"amount": 0.42, "currency": "USD"},
				},
			},
		})
		msgs, err := testBackend().NewWire().ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		um, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UsageMessage", msgs[0])
		}
		if um.ContextWindow != 200000 {
			t.Errorf("ContextWindow = %d, want 200000", um.ContextWindow)
		}
		if um.CumulativeCostUSD == nil || *um.CumulativeCostUSD != 0.42 {
			t.Errorf("CumulativeCostUSD = %v, want 0.42", um.CumulativeCostUSD)
		}
	})

	t.Run("UserMessageChunk", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Fix the bug"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "Fix the bug" {
			t.Errorf("Text = %q", ui.Text)
		}
	})

	t.Run("JSONRPCResponse", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msgs[0])
		}
		if rm.MessageType != "jsonrpc_response" {
			t.Errorf("MessageType = %q", rm.MessageType)
		}
	})

	t.Run("WidgetToolCall", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    "widget_1",
					"title":         "show_widget",
					"kind":          "other",
					"status":        "pending",
					"rawInput":      map[string]any{"title": "Chart", "widget_code": "<div>chart</div>"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		wm, ok := msgs[0].(*agent.WidgetMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.WidgetMessage", msgs[0])
		}
		if wm.Title != "Chart" {
			t.Errorf("Title = %q", wm.Title)
		}
		if wm.HTML != "<div>chart</div>" {
			t.Errorf("HTML = %q", wm.HTML)
		}
	})

	t.Run("SessionInfoSkipped", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "session_info_update",
					"title":         "Fix null pointer",
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (session_info_update is skipped)", len(msgs))
		}
	})

	t.Run("ToolCallFailedWithRawOutput", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_3",
					"status":        "failed",
					"rawOutput":     map[string]any{"error": "access denied", "output": ""},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.Error != "access denied" {
			t.Errorf("Error = %q, want %q", tr.Error, "access denied")
		}
	})

	t.Run("ToolCallInProgressWithRawOutput", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_4",
					"status":        "in_progress",
					"rawOutput":     map[string]any{"output": "streaming output"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		td, ok := msgs[0].(*agent.ToolOutputDeltaMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolOutputDeltaMessage", msgs[0])
		}
		if td.Delta != "streaming output" {
			t.Errorf("Delta = %q, want %q", td.Delta, "streaming output")
		}
	})

	t.Run("CurrentModeUpdateWithDetail", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "current_mode_update",
					"currentModeId": "ask",
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		sm, ok := msgs[0].(*agent.SystemMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.SystemMessage", msgs[0])
		}
		if sm.Subtype != "notice" || sm.Detail != "Mode: ask" {
			t.Errorf("system = %q %q, want notice %q", sm.Subtype, sm.Detail, "Mode: ask")
		}
	})

	t.Run("UnknownUpdateType", func(t *testing.T) {
		t.Parallel()
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "future_update_type",
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msgs[0])
		}
		if rm.MessageType != "session/update:future_update_type" {
			t.Errorf("MessageType = %q", rm.MessageType)
		}
	})
}

func TestNormalizeToolName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		title string
		kind  acp.ToolKind
		want  string
	}{
		{"bash", acp.KindExecute, "Bash"},
		{"edit", acp.KindEdit, "Edit"},
		{"write", acp.KindEdit, "Write"},
		{"read", acp.KindRead, "Read"},
		{"glob", acp.KindSearch, "Glob"},
		{"grep", acp.KindSearch, "Grep"},
		{"list", acp.KindRead, "ListDirectory"},
		{"webfetch", acp.KindFetch, "WebFetch"},
		{"websearch", acp.KindSearch, "WebSearch"},
		{"todowrite", acp.KindOther, "TodoWrite"},
		{"task", acp.KindOther, "Agent"},
		// Additional name mappings.
		{"patch", acp.KindEdit, "Edit"},
		// Kind-based fallback.
		{"unknown_tool", acp.KindExecute, "Bash"},
		{"unknown_tool", acp.KindEdit, "Edit"},
		{"unknown_tool", acp.KindRead, "Read"},
		{"unknown_tool", acp.KindSearch, "Grep"},
		{"unknown_tool", acp.KindFetch, "WebFetch"},
		// Passthrough.
		{"custom_mcp_tool", acp.KindOther, "custom_mcp_tool"},
	}
	for _, tt := range tests {
		t.Run(tt.title+"_"+string(tt.kind), func(t *testing.T) {
			t.Parallel()
			got := normalizeToolName(tt.title, tt.kind)
			if got != tt.want {
				t.Errorf("normalizeToolName(%q, %q) = %q, want %q", tt.title, tt.kind, got, tt.want)
			}
		})
	}
}

func TestWireFormatPromptResponse(t *testing.T) {
	t.Parallel()
	t.Run("ReconnectDirectionalState", func(t *testing.T) {
		t.Parallel()
		w := testBackend().NewWire()
		parser, err := agent.NewLogRecordParser(w)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{
			`{"t":"agent","ts":1.000,"msg":{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"previous turn"}}}}}`,
			`{"t":"input","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"session","prompt":[{"type":"text","text":"next"}]}}}`,
			`{"t":"agent","ts":3.000,"msg":{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"current turn"}}}}}`,
			// An output permission request must not reset accumulated prompt text.
			`{"t":"agent","ts":4.000,"msg":{"jsonrpc":"2.0","id":18,"method":"session/request_permission","params":{}}}`,
		} {
			if _, err := parser.ParseRecord([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		// Continue on the same provider after reconnect. An error response has no
		// stopReason fallback, so only the recorded input ID identifies this turn.
		messages, err := w.ParseOutput([]byte(`{"jsonrpc":"2.0","id":17,"error":{"code":-32603,"message":"interrupted"}}`))
		if err != nil || len(messages) != 2 {
			t.Fatalf("continued response = %#v, %v", messages, err)
		}
		text, ok := messages[0].(*agent.TextMessage)
		if !ok || text.Text != "current turn" {
			t.Fatalf("accumulated response = %#v", messages[0])
		}
		result, ok := messages[1].(*agent.ResultMessage)
		if !ok || !result.IsError || result.Result != "interrupted" {
			t.Fatalf("continued result = %#v", messages[1])
		}
	})
	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      5,
			"result": map[string]any{
				"stopReason": "end_turn",
				"usage": map[string]any{
					"inputTokens":   3000,
					"outputTokens":  500,
					"thoughtTokens": 200,
				},
			},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.IsError {
			t.Error("IsError should be false")
		}
		if rm.Usage.InputTokens != 3000 {
			t.Errorf("InputTokens = %d, want 3000", rm.Usage.InputTokens)
		}
		if rm.Usage.OutputTokens != 500 {
			t.Errorf("OutputTokens = %d, want 500", rm.Usage.OutputTokens)
		}
		if rm.Usage.ReasoningOutputTokens != 200 {
			t.Errorf("ReasoningOutputTokens = %d, want 200", rm.Usage.ReasoningOutputTokens)
		}
		if rm.Usage.CacheTTLSeconds != 0 {
			t.Errorf("CacheTTLSeconds = %d, want unknown", rm.Usage.CacheTTLSeconds)
		}
	})

	t.Run("Cancelled", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      7,
			"result": map[string]any{
				"stopReason": "cancelled",
			},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if !rm.IsError {
			t.Error("IsError should be true for cancelled")
		}
		if rm.Result != "cancelled" {
			t.Errorf("Result = %q, want %q", rm.Result, "cancelled")
		}
	})

	t.Run("CacheOnlyUsage", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      8,
			"result": map[string]any{
				"stopReason": "end_turn",
				"usage": map[string]any{
					"cachedReadTokens":  8000,
					"cachedWriteTokens": 1000,
				},
			},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		result, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if result.Usage.CacheReadInputTokens != 8000 || result.Usage.CacheCreationInputTokens != 1000 {
			t.Errorf("Usage = %#v, want cache read=8000 and write=1000", result.Usage)
		}
		if result.Usage.CacheTTLSeconds != 0 {
			t.Errorf("CacheTTLSeconds = %d, want unknown", result.Usage.CacheTTLSeconds)
		}
	})

	t.Run("Error", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      9,
			"error": map[string]any{
				"code":    -32603,
				"message": "internal error",
			},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if !rm.IsError {
			t.Error("IsError should be true")
		}
		if rm.Result != "internal error" {
			t.Errorf("Result = %q", rm.Result)
		}
	})

	t.Run("EmptyPromptUsageStaysEmpty", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result": map[string]any{
				"stopReason": "end_turn",
			},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", msgs[0])
		}
		if !rm.Usage.Equal(agent.Usage{}) {
			t.Errorf("Usage = %+v, want empty", rm.Usage)
		}
	})

	t.Run("NonPromptResponse", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      99, // Different ID.
			"result":  map[string]any{},
		})
		msgs, err := w.ParseOutput(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.RawMessage); !ok {
			t.Fatalf("type = %T, want *agent.RawMessage for non-prompt response", msgs[0])
		}
	})

	t.Run("UncorrelatedPromptShapedResponse", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config}
		msgs, err := w.ParseOutput([]byte(`{"jsonrpc":"2.0","id":42,"result":{"stopReason":"end_turn"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want raw response", len(msgs))
		}
		if _, ok := msgs[0].(*agent.RawMessage); !ok {
			t.Fatalf("response = %T, want raw", msgs[0])
		}
	})

	t.Run("ReplayPromptErrorAfterRecordedRequest", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config}
		req := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      11,
			"method":  "session/prompt",
			"params":  map[string]any{"sessionId": "ses_1"},
		})
		if _, err := w.ParseInput(req); err != nil {
			t.Fatal(err)
		}
		resp := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      11,
			"error": map[string]any{
				"code":    -32603,
				"message": "prompt failed",
			},
		})
		msgs, err := w.ParseOutput(resp)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
		if !rm.IsError || rm.Result != "prompt failed" {
			t.Errorf("ResultMessage = %+v, want prompt failed error", rm)
		}
	})

	t.Run("RecordedPromptRestoresUserInput", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config}
		req := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      12,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": "ses_1",
				"prompt": []map[string]any{
					{"type": "text", "text": "fix the bug"},
					{"type": "image", "mimeType": "image/png", "data": "aW1hZ2U="},
				},
			},
		})
		msgs, err := w.ParseInput(req)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("messages = %d, want input and raw request", len(msgs))
		}
		input, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("messages[0] = %T, want *agent.UserInputMessage", msgs[0])
		}
		if input.Text != "fix the bug" || len(input.Images) != 1 || input.Images[0].MediaType != "image/png" || input.Images[0].Data != "aW1hZ2U=" {
			t.Errorf("input = %#v", input)
		}
		if _, ok := msgs[1].(*agent.RawMessage); !ok {
			t.Fatalf("messages[1] = %T, want *agent.RawMessage", msgs[1])
		}
	})

	t.Run("SyntheticFinalMessages", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config, session: agent.SessionMetadata{SessionID: "ses_1"}}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}

		// Simulate streaming chunks.
		for _, text := range []string{"Hello ", "world"} {
			chunk := mustJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "ses_1",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": text},
					},
				},
			})
			if _, err := w.ParseOutput(chunk); err != nil {
				t.Fatal(err)
			}
		}
		for _, text := range []string{"I think ", "carefully"} {
			chunk := mustJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "ses_1",
					"update": map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": text},
					},
				},
			})
			if _, err := w.ParseOutput(chunk); err != nil {
				t.Fatal(err)
			}
		}

		// Now send prompt response.
		resp := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      10,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
		msgs, err := w.ParseOutput(resp)
		if err != nil {
			t.Fatal(err)
		}
		// Expect: ThinkingMessage, TextMessage, ResultMessage.
		if len(msgs) != 3 {
			t.Fatalf("msgs = %d, want 3", len(msgs))
		}
		tm, ok := msgs[0].(*agent.ThinkingMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.ThinkingMessage", msgs[0])
		}
		if tm.Text != "I think carefully" {
			t.Errorf("ThinkingMessage.Text = %q", tm.Text)
		}
		txm, ok := msgs[1].(*agent.TextMessage)
		if !ok {
			t.Fatalf("msgs[1] type = %T, want *agent.TextMessage", msgs[1])
		}
		if txm.Text != "Hello world" {
			t.Errorf("TextMessage.Text = %q", txm.Text)
		}
		if _, ok := msgs[2].(*agent.ResultMessage); !ok {
			t.Fatalf("msgs[2] type = %T, want *agent.ResultMessage", msgs[2])
		}
	})

	t.Run("AccumulationOverflowKeepsStreamingHistoryAuthoritative", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{config: testBackend().Config}
		if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{}}`)); err != nil {
			t.Fatal(err)
		}
		chunk := func(text string) []byte {
			return mustJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "ses_1",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": text},
					},
				},
			})
		}
		for _, text := range []string{strings.Repeat("x", maxAccumulatedOutputBytes), "y"} {
			msgs, err := w.ParseOutput(chunk(text))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("streaming chunk messages = %d, want 1", len(msgs))
			}
		}
		response := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      10,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
		msgs, err := w.ParseOutput(response)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("overflow response messages = %d, want result only", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ResultMessage); !ok {
			t.Fatalf("overflow response message = %T, want *agent.ResultMessage", msgs[0])
		}
	})
}

// mustJSON marshals v to []byte, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseTestMessage(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, err
	}
	msgs, _, err := parseMessage(line, &probe)
	return msgs, err
}

func TestReplayBufferOwnership(t *testing.T) {
	t.Parallel()
	wire := testBackend().NewWire()
	line := []byte(`{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"tool_call","toolCallId":"call","title":"read","kind":"read","rawInput":{"filePath":"hello.txt"}}}}`)
	msgs, err := wire.ParseOutput(line)
	if err != nil {
		t.Fatal(err)
	}
	for i := range line {
		line[i] = 'x'
	}
	msg, ok := msgs[0].(*agent.ToolUseMessage)
	if !ok || string(msg.Input) != `{"filePath":"hello.txt"}` {
		t.Fatalf("message = %#v", msgs)
	}
}

func TestAddEditInputView(t *testing.T) {
	t.Parallel()
	t.Run("edit tool display", func(t *testing.T) {
		t.Parallel()

		use := &agent.ToolUseMessage{
			Name:  "Edit",
			Input: json.RawMessage(`{"filePath":"src/main.ts","oldString":"before","newString":"after"}`),
		}
		addEditInputView(use)
		if use.Detail != "main.ts" {
			t.Fatalf("detail = %q, want main.ts", use.Detail)
		}
		if use.InputView.Kind != agent.ToolInputFileChanges {
			t.Fatalf("input view kind = %q, want fileChanges", use.InputView.Kind)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		file := use.InputView.Files[0]
		if file.Path != "src/main.ts" || !strings.Contains(file.Patch, "-before\n+after\n") {
			t.Fatalf("file change = %#v", file)
		}
	})
}

func BenchmarkParseToolCallUpdateRawOutput(b *testing.B) {
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress","rawOutput":{"output":"streaming output"}}}}`)
	b.ReportAllocs()
	for b.Loop() {
		msgs, err := parseTestMessage(line)
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) != 1 {
			b.Fatalf("parseMessage returned %d messages, want 1", len(msgs))
		}
	}
}
