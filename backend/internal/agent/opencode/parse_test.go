package opencode

import (
	"encoding/json"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

func TestParseMessage(t *testing.T) {
	t.Run("AgentMessageChunk", func(t *testing.T) {
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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

	t.Run("ToolCallInProgressWithOutput", func(t *testing.T) {
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_1",
					"status":        "in_progress",
					"content": []any{
						map[string]any{
							"type":    "content",
							"content": map[string]any{"type": "text", "text": "output line"},
						},
					},
				},
			},
		})
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		if td.Delta != "output line" {
			t.Errorf("Delta = %q", td.Delta)
		}
	})

	t.Run("PlanUpdate", func(t *testing.T) {
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
	})

	t.Run("UserMessageChunk", func(t *testing.T) {
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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

	t.Run("caicInit", func(t *testing.T) {
		input := mustJSON(t, map[string]any{
			"type":       "caic_init",
			"session_id": "ses_abc",
			"model":      "anthropic/claude-sonnet-4",
			"version":    "0.5.0",
		})
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		init, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.InitMessage", msgs[0])
		}
		if init.SessionID != "ses_abc" {
			t.Errorf("SessionID = %q, want %q", init.SessionID, "ses_abc")
		}
		if init.Model != "anthropic/claude-sonnet-4" {
			t.Errorf("Model = %q, want %q", init.Model, "anthropic/claude-sonnet-4")
		}
		if init.Version != "0.5.0" {
			t.Errorf("Version = %q, want %q", init.Version, "0.5.0")
		}
	})

	t.Run("CaicDiffStat", func(t *testing.T) {
		input := mustJSON(t, map[string]any{
			"type": "caic_diff_stat",
			"ts":   1719500000.5,
			"diff_stat": []any{
				map[string]any{"path": "main.go", "added": 10, "deleted": 3},
			},
		})
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ds, ok := msgs[0].(*agent.DiffStatMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.DiffStatMessage", msgs[0])
		}
		if len(ds.DiffStat) != 1 {
			t.Fatalf("DiffStat = %d, want 1", len(ds.DiffStat))
		}
		if ds.DiffStat[0].Path != "main.go" {
			t.Errorf("Path = %q", ds.DiffStat[0].Path)
		}
	})

	t.Run("JSONRPCResponse", func(t *testing.T) {
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (session_info_update is skipped)", len(msgs))
		}
	})

	t.Run("ToolCallFailedWithRawOutput", func(t *testing.T) {
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "current_mode_update",
					"modeId":        "ask",
					"modeName":      "Ask Mode",
				},
			},
		})
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
		if sm.Subtype != "mode_update" {
			t.Errorf("Subtype = %q", sm.Subtype)
		}
		if sm.Detail != "Ask Mode" {
			t.Errorf("Detail = %q, want %q", sm.Detail, "Ask Mode")
		}
	})

	t.Run("UnknownUpdateType", func(t *testing.T) {
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
		msgs, err := parseMessage(input, &jsonutil.FieldWarner{})
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
	tests := []struct {
		title string
		kind  ToolKind
		want  string
	}{
		{"bash", KindExecute, "Bash"},
		{"edit", KindEdit, "Edit"},
		{"write", KindEdit, "Write"},
		{"read", KindRead, "Read"},
		{"glob", KindSearch, "Glob"},
		{"grep", KindSearch, "Grep"},
		{"list", KindRead, "ListDirectory"},
		{"webfetch", KindFetch, "WebFetch"},
		{"websearch", KindSearch, "WebSearch"},
		{"todowrite", KindOther, "TodoWrite"},
		{"task", KindOther, "Agent"},
		// Additional name mappings.
		{"patch", KindEdit, "Edit"},
		// Kind-based fallback.
		{"unknown_tool", KindExecute, "Bash"},
		{"unknown_tool", KindEdit, "Edit"},
		{"unknown_tool", KindRead, "Read"},
		{"unknown_tool", KindSearch, "Grep"},
		{"unknown_tool", KindFetch, "WebFetch"},
		// Passthrough.
		{"custom_mcp_tool", KindOther, "custom_mcp_tool"},
	}
	for _, tt := range tests {
		t.Run(tt.title+"_"+string(tt.kind), func(t *testing.T) {
			got := normalizeToolName(tt.title, tt.kind)
			if got != tt.want {
				t.Errorf("normalizeToolName(%q, %q) = %q, want %q", tt.title, tt.kind, got, tt.want)
			}
		})
	}
}

func TestWireFormatPromptResponse(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 5
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
		msgs, err := w.ParseMessage(input)
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
	})

	t.Run("Cancelled", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 7
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      7,
			"result": map[string]any{
				"stopReason": "cancelled",
			},
		})
		msgs, err := w.ParseMessage(input)
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

	t.Run("Error", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 9
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      9,
			"error": map[string]any{
				"code":    -32603,
				"message": "internal error",
			},
		})
		msgs, err := w.ParseMessage(input)
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

	t.Run("FallbackToAccumulatedUsage", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 3
		w.totalUsage = agent.Usage{InputTokens: 1000, OutputTokens: 200}
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result": map[string]any{
				"stopReason": "end_turn",
			},
		})
		msgs, err := w.ParseMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		rm := msgs[0].(*agent.ResultMessage)
		if rm.Usage.InputTokens != 1000 {
			t.Errorf("InputTokens = %d, want 1000 (fallback)", rm.Usage.InputTokens)
		}
		// totalUsage should be reset.
		if w.totalUsage != (agent.Usage{}) {
			t.Errorf("totalUsage not reset: %+v", w.totalUsage)
		}
	})

	t.Run("NonPromptResponse", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 5
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      99, // Different ID.
			"result":  map[string]any{},
		})
		msgs, err := w.ParseMessage(input)
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

	t.Run("SyntheticFinalMessages", func(t *testing.T) {
		w := &wireFormat{sessionID: "ses_1"}
		w.promptReqID = 10

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
			if _, err := w.ParseMessage(chunk); err != nil {
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
			if _, err := w.ParseMessage(chunk); err != nil {
				t.Fatal(err)
			}
		}

		// Now send prompt response.
		resp := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      10,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
		msgs, err := w.ParseMessage(resp)
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
}

// mustJSON marshals v to []byte, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
