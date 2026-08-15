// Tests for the Claude Code wire-format message parser.

package claudecode

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	genclaudecode "github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

func TestWidgetTrackerBounds(t *testing.T) {
	t.Parallel()

	stream := func(t *testing.T, wt *WidgetTracker, event map[string]any) {
		t.Helper()
		line, err := json.Marshal(map[string]any{"type": "stream_event", "event": event})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseMessageWithTracker(line, wt, &jsonutil.FieldWarner{}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(t *testing.T, wt *WidgetTracker, index int) {
		t.Helper()
		stream(t, wt, map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("widget-%d", index), "name": "show_widget"},
		})
	}

	t.Run("tracked blocks", func(t *testing.T) {
		t.Parallel()
		wt := NewWidgetTracker()
		for index := range maxTrackedWidgetBlocks + 8 {
			start(t, wt, index)
		}
		if len(wt.activeWidgets) != maxTrackedWidgetBlocks {
			t.Fatalf("tracked blocks = %d, want %d", len(wt.activeWidgets), maxTrackedWidgetBlocks)
		}
		if len(wt.accum) != 0 || len(wt.lastHTMLLen) != 0 || len(wt.exceeded) != 0 {
			t.Fatalf("unexpected state before deltas: %+v", wt)
		}
	})

	t.Run("aggregate JSON", func(t *testing.T) {
		t.Parallel()
		wt := NewWidgetTracker()
		start(t, wt, 0)
		stream(t, wt, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": strings.Repeat("x", maxBufferedWidgetJSONBytes)},
		})
		if wt.bufferedJSONBytes != maxBufferedWidgetJSONBytes {
			t.Fatalf("buffered JSON = %d, want %d", wt.bufferedJSONBytes, maxBufferedWidgetJSONBytes)
		}
		stream(t, wt, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "x"},
		})
		if wt.bufferedJSONBytes != 0 || len(wt.accum) != 0 {
			t.Fatalf("over-limit JSON retained %d bytes in %#v", wt.bufferedJSONBytes, wt.accum)
		}
		if _, ok := wt.exceeded[0]; !ok {
			t.Fatal("over-limit widget was not marked exceeded")
		}
		stream(t, wt, map[string]any{"type": "content_block_stop", "index": 0})
		if len(wt.activeWidgets) != 0 || len(wt.exceeded) != 0 || wt.bufferedJSONBytes != 0 {
			t.Fatalf("stop did not release state: %+v", wt)
		}
	})
}

func TestAPIMessage(t *testing.T) {
	t.Parallel()
	t.Run("Metadata", func(t *testing.T) {
		t.Parallel()
		line := `{"id":"msg_01","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"true"},"caller":{"type":"code_execution_20260120","tool_id":"srv_1"}}],"stop_details":{"type":"refusal","category":"cyber","explanation":"blocked"},"container":{"id":"container_1","expires_at":"2026-06-07T12:00:00Z","skills":[{"skill_id":"sk_1","type":"anthropic","version":"latest"}]},"diagnostics":{"cache_miss_reason":{"type":"tools_changed","cache_missed_input_tokens":42}}}`
		var msg genclaudecode.AssistantMessageBody
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Container.ID != "container_1" {
			t.Errorf("Container.ID = %q, want container_1", msg.Container.ID)
		}
		if len(msg.Container.Skills) != 1 || msg.Container.Skills[0].Type != genclaudecode.SkillAnthropic {
			t.Fatalf("Container.Skills = %+v, want one anthropic skill", msg.Container.Skills)
		}
		if msg.StopDetails.Type != "refusal" {
			t.Errorf("StopDetails.Type = %q, want refusal", msg.StopDetails.Type)
		}
		if msg.StopDetails.Category != "cyber" {
			t.Errorf("StopDetails.Category = %q, want cyber", msg.StopDetails.Category)
		}
		if msg.Diagnostics.CacheMissReason.Type != genclaudecode.CacheMissReasonToolsChanged {
			t.Errorf("CacheMissReason.Type = %q, want %q", msg.Diagnostics.CacheMissReason.Type, genclaudecode.CacheMissReasonToolsChanged)
		}
		if msg.Diagnostics.CacheMissReason.CacheMissedInputTokens != 42 {
			t.Errorf("CacheMissedInputTokens = %d, want 42", msg.Diagnostics.CacheMissReason.CacheMissedInputTokens)
		}
		if msg.Content[0].Caller.Type != "code_execution_20260120" {
			t.Errorf("Caller.Type = %q, want code_execution_20260120", msg.Content[0].Caller.Type)
		}
		if msg.Content[0].Caller.ToolID != "srv_1" {
			t.Errorf("Caller.ToolID = %q, want srv_1", msg.Content[0].Caller.ToolID)
		}
	})
	t.Run("ContextManagementAppliedEdits", func(t *testing.T) {
		t.Parallel()
		line := `{"id":"msg_01","type":"message","model":"claude-opus-4-6","role":"assistant","content":[],"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_input_tokens":123,"cleared_tool_uses":4},{"type":"clear_thinking_20251015","cleared_input_tokens":456,"cleared_thinking_turns":7}]}}`
		var msg genclaudecode.AssistantMessageBody
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		if len(msg.ContextManagement.AppliedEdits) != 2 {
			t.Fatalf("len(AppliedEdits) = %d, want 2", len(msg.ContextManagement.AppliedEdits))
		}
		edit := msg.ContextManagement.AppliedEdits[0]
		if edit.Type != genclaudecode.AppliedEditClearToolUses {
			t.Errorf("AppliedEdits[0].Type = %q, want %q", edit.Type, genclaudecode.AppliedEditClearToolUses)
		}
		if edit.ClearedInputTokens != 123 {
			t.Errorf("AppliedEdits[0].ClearedInputTokens = %d, want 123", edit.ClearedInputTokens)
		}
		if edit.ClearedToolUses != 4 {
			t.Errorf("AppliedEdits[0].ClearedToolUses = %d, want 4", edit.ClearedToolUses)
		}
		edit = msg.ContextManagement.AppliedEdits[1]
		if edit.Type != genclaudecode.AppliedEditClearThinking {
			t.Errorf("AppliedEdits[1].Type = %q, want %q", edit.Type, genclaudecode.AppliedEditClearThinking)
		}
		if edit.ClearedInputTokens != 456 {
			t.Errorf("AppliedEdits[1].ClearedInputTokens = %d, want 456", edit.ClearedInputTokens)
		}
		if edit.ClearedThinkingTurns != 7 {
			t.Errorf("AppliedEdits[1].ClearedThinkingTurns = %d, want 7", edit.ClearedThinkingTurns)
		}
	})
}

func TestOutputKnownFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  any
		line string
	}{
		{
			name: "OutputSystemMsg",
			typ:  genclaudecode.OutputSystemMsg{},
			line: `{"type":"system","subtype":"task_started","task_id":"task-abc","tool_use_id":"toolu_1","description":"Find harness/model selection logic","subagent_type":"Explore","task_type":"local_agent","prompt":"Find harness/model selection logic","uuid":"u1","session_id":"s1"}`,
		},
		{
			name: "OutputUserMsg",
			typ:  genclaudecode.OutputUserMsg{},
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Find harness/model selection logic"}]},"parent_tool_use_id":"toolu_1","session_id":"s1","uuid":"u1","timestamp":"2026-06-13T20:16:11.423Z","subagent_type":"Explore","task_description":"Find harness/model selection logic"}`,
		},
		{
			name: "OutputAssistantMsg",
			typ:  genclaudecode.OutputAssistantMsg{},
			line: `{"type":"assistant","message":{"model":"claude-opus-4-8","id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}},"parent_tool_use_id":"toolu_1","session_id":"s1","uuid":"u1","subagent_type":"Explore","task_description":"Find harness/model selection logic"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			known := jsonutil.KnownFields(tt.typ)
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.line), &raw); err != nil {
				t.Fatal(err)
			}
			unknown := jsonutil.CollectUnknown(raw, known)
			if len(unknown) != 0 {
				t.Fatalf("unknown fields = %v, want none", sortedRawMessageKeys(unknown))
			}
		})
	}
}

func sortedRawMessageKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestParseMessage(t *testing.T) {
	t.Parallel()
	t.Run("SystemInit", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"system","subtype":"init","cwd":"/home/user","session_id":"abc-123","tools":["Bash","Read"],"model":"claude-opus-4-6","claude_code_version":"2.1.34","uuid":"uuid-1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.InitMessage", msgs[0])
		}
		if m.Model != "claude-opus-4-6" {
			t.Errorf("model = %q, want %q", m.Model, "claude-opus-4-6")
		}
		if len(m.Tools) != 2 {
			t.Errorf("tools = %v, want 2 items", m.Tools)
		}
	})
	t.Run("AssistantTextAndUsage", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_01","role":"assistant","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"thinking_tokens":3}}},"session_id":"abc","uuid":"u1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) < 2 {
			t.Fatalf("got %d messages, want 2 (text + usage)", len(msgs))
		}
		tm, ok := msgs[0].(*agent.TextMessage)
		if !ok {
			t.Fatalf("msgs[0] is %T, want *agent.TextMessage", msgs[0])
		}
		if tm.Text != "hello world" {
			t.Errorf("text = %q, want %q", tm.Text, "hello world")
		}
		um, ok := msgs[1].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("msgs[1] is %T, want *agent.UsageMessage", msgs[1])
		}
		if um.Usage.InputTokens != 10 || um.Usage.OutputTokens != 5 {
			t.Errorf("usage = %+v, want input=10 output=5", um.Usage)
		}
		if um.Usage.ReasoningOutputTokens != 3 {
			t.Errorf("reasoning output tokens = %d, want 3", um.Usage.ReasoningOutputTokens)
		}
		if um.Model != "claude-opus-4-6" {
			t.Errorf("model = %q, want %q", um.Model, "claude-opus-4-6")
		}
	})
	t.Run("AssistantToolUse", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}],"usage":{}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolUseMessage", msgs[0])
		}
		if tu.Name != "Bash" {
			t.Errorf("name = %q, want %q", tu.Name, "Bash")
		}
		if tu.ToolUseID != "tu_1" {
			t.Errorf("id = %q, want %q", tu.ToolUseID, "tu_1")
		}
	})
	t.Run("AssistantAskUserQuestion", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"ask_1","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","header":"Pick","options":[{"label":"A","description":"First"},{"label":"B"}],"multiSelect":true}]}}],"usage":{}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ask, ok := msgs[0].(*agent.AskMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.AskMessage", msgs[0])
		}
		if ask.ToolUseID != "ask_1" {
			t.Errorf("id = %q, want %q", ask.ToolUseID, "ask_1")
		}
		if len(ask.Questions) != 1 {
			t.Fatalf("questions = %d, want 1", len(ask.Questions))
		}
		if ask.Questions[0].Question != "Which?" {
			t.Errorf("question = %q, want %q", ask.Questions[0].Question, "Which?")
		}
		if !ask.Questions[0].MultiSelect {
			t.Error("MultiSelect = false, want true")
		}
		if ask.Questions[0].Options[0].Description != "First" {
			t.Errorf("description = %q, want %q", ask.Questions[0].Options[0].Description, "First")
		}
	})
	t.Run("AssistantTodoWrite", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"td_1","name":"TodoWrite","input":{"todos":[{"content":"Fix bug","status":"pending","activeForm":"Fixing bug"}]}}],"usage":{}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		todo, ok := msgs[0].(*agent.TodoMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.TodoMessage", msgs[0])
		}
		if len(todo.Todos) != 1 {
			t.Fatalf("todos = %d, want 1", len(todo.Todos))
		}
		if todo.Todos[0].Content != "Fix bug" {
			t.Errorf("content = %q, want %q", todo.Todos[0].Content, "Fix bug")
		}
		if todo.Todos[0].ActiveForm != "Fixing bug" {
			t.Errorf("active form = %q, want %q", todo.Todos[0].ActiveForm, "Fixing bug")
		}
	})
	t.Run("AssistantMultiBlock", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"thinking..."},{"type":"tool_use","id":"tu_1","name":"Read","input":{"file":"x.go"}}],"usage":{"input_tokens":100,"output_tokens":50}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		// text + tool_use + usage = 3
		if len(msgs) != 3 {
			t.Fatalf("got %d messages, want 3", len(msgs))
		}
		if _, ok := msgs[0].(*agent.TextMessage); !ok {
			t.Errorf("msgs[0] is %T, want *agent.TextMessage", msgs[0])
		}
		if _, ok := msgs[1].(*agent.ToolUseMessage); !ok {
			t.Errorf("msgs[1] is %T, want *agent.ToolUseMessage", msgs[1])
		}
		if _, ok := msgs[2].(*agent.UsageMessage); !ok {
			t.Errorf("msgs[2] is %T, want *agent.UsageMessage", msgs[2])
		}
	})
	t.Run("UserInput", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"role":"user","content":"hello"}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "hello" {
			t.Errorf("text = %q, want %q", ui.Text, "hello")
		}
	})
	t.Run("ToolResult", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"content":[{"type":"text","text":"ok"}],"is_error":false},"parent_tool_use_id":"tu_1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "tu_1" {
			t.Errorf("tool_use_id = %q, want %q", tr.ToolUseID, "tu_1")
		}
		if tr.Error != "" {
			t.Errorf("error = %q, want empty", tr.Error)
		}
	})
	t.Run("ToolResultError", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"content":[{"type":"text","text":"file not found"}],"is_error":true},"parent_tool_use_id":"tu_2"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.Error != "file not found" {
			t.Errorf("error = %q, want %q", tr.Error, "file not found")
		}
	})
	t.Run("ToolResultErrorStringContent", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"content":"file not found","is_error":true},"parent_tool_use_id":"tu_2"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.Error != "file not found" {
			t.Errorf("error = %q, want %q", tr.Error, "file not found")
		}
	})
	t.Run("InlineToolResult", func(t *testing.T) {
		t.Parallel()
		// MCP tool results arrive as user messages without parent_tool_use_id,
		// but with a tool_result content block carrying the tool_use_id inline.
		line := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_abc123","type":"tool_result","content":[{"type":"text","text":"Widget rendered."}]}]},"parent_tool_use_id":null}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "toolu_abc123" {
			t.Errorf("tool_use_id = %q, want %q", tr.ToolUseID, "toolu_abc123")
		}
		if tr.Error != "" {
			t.Errorf("error = %q, want empty", tr.Error)
		}
	})
	t.Run("InlineToolResultError", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_err","type":"tool_result","is_error":true,"content":[{"type":"text","text":"tool failed"}]}]},"parent_tool_use_id":null}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "toolu_err" {
			t.Errorf("tool_use_id = %q, want %q", tr.ToolUseID, "toolu_err")
		}
		if tr.Error != "tool failed" {
			t.Errorf("error = %q, want %q", tr.Error, "tool failed")
		}
	})
	t.Run("InlineToolResultErrorStringContent", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_err","type":"tool_result","is_error":true,"content":"tool failed"}]},"parent_tool_use_id":null}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.Error != "tool failed" {
			t.Errorf("error = %q, want %q", tr.Error, "tool failed")
		}
	})
	t.Run("Result", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234.5,"duration_api_ms":987.6,"num_turns":3,"result":"done","total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50,"output_tokens_details":{"thinking_tokens":12}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", msgs[0])
		}
		if m.NumTurns != 3 {
			t.Errorf("turns = %d, want 3", m.NumTurns)
		}
		if m.DurationMs != 1234 {
			t.Errorf("DurationMs = %d, want 1234", m.DurationMs)
		}
		if m.DurationAPIMs != 987 {
			t.Errorf("DurationAPIMs = %d, want 987", m.DurationAPIMs)
		}
		if m.Usage.ReasoningOutputTokens != 12 {
			t.Errorf("ReasoningOutputTokens = %d, want 12", m.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("StreamEventTextDelta", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.TextDeltaMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.TextDeltaMessage", msgs[0])
		}
		if m.Text != "Hello" {
			t.Errorf("text = %q, want %q", m.Text, "Hello")
		}
	})
	t.Run("DiffStat", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"caic_diff_stat","diff_stat":[{"path":"main.go","added":10,"deleted":3}],"ts":1719500000.123}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.DiffStatMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.DiffStatMessage", msgs[0])
		}
		if len(m.DiffStat) != 1 {
			t.Fatalf("diff_stat len = %d, want 1", len(m.DiffStat))
		}
		if m.Ts != 1719500000.123 {
			t.Fatalf("ts = %f, want 1719500000.123", m.Ts)
		}
	})
	t.Run("PendingAskUserAction", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which login boundary should Google use in caic?","header":"Login","options":[{"label":"Identity only","description":"Use identity-only login boundary"},{"label":"Forge-coupled","description":"Use forge-coupled login boundary"}],"multiSelect":false}]},"tool_use_id":"toolu-1"}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.PendingUserActionMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.PendingUserActionMessage", msgs[0])
		}
		if m.Action.Kind != agent.PendingUserActionAskUserQuestion {
			t.Errorf("Kind = %q, want %q", m.Action.Kind, agent.PendingUserActionAskUserQuestion)
		}
		if m.Action.RequestID != "req-1" {
			t.Errorf("RequestID = %q, want req-1", m.Action.RequestID)
		}
		if m.Action.ToolUseID != "toolu-1" {
			t.Errorf("ToolUseID = %q, want toolu-1", m.Action.ToolUseID)
		}
		if len(m.Action.Ask.Questions) != 1 {
			t.Fatalf("Questions len = %d, want 1", len(m.Action.Ask.Questions))
		}
		if len(m.Action.Ask.Questions[0].Options) != 2 {
			t.Fatalf("Options len = %d, want 2", len(m.Action.Ask.Questions[0].Options))
		}
		if m.Action.Ask.Questions[0].Options[1].Label != "Forge-coupled" {
			t.Errorf("option = %q, want Forge-coupled", m.Action.Ask.Questions[0].Options[1].Label)
		}
	})
	t.Run("RawFallback", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"tool_progress","data":"some progress"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.RawMessage); !ok {
			t.Fatalf("got %T, want *agent.RawMessage", msgs[0])
		}
	})
	t.Run("SystemNoiseDropped", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"system","subtype":"status","session_id":"s1","uuid":"u1"}`,
			`{"type":"system","subtype":"task_progress","session_id":"s1","uuid":"u1"}`,
			`{"type":"system","subtype":"turn_duration","session_id":"s1","uuid":"u1"}`,
			`{"type":"system","subtype":"commands_changed","commands":[{"name":"widget","description":"Render widgets","argumentHint":"","aliases":["caic-widget:widget"]}],"session_id":"s1","uuid":"u1"}`,
		}
		for _, line := range lines {
			msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
			if err != nil {
				t.Fatalf("line %s: %v", line, err)
			}
			if len(msgs) != 0 {
				t.Errorf("line %s: got %d messages, want 0", line, len(msgs))
			}
		}
	})
	t.Run("SystemUsefulSubtypes", func(t *testing.T) {
		t.Parallel()
		for _, subtype := range []string{"compact_boundary", "context_cleared", "api_error"} {
			line := `{"type":"system","subtype":"` + subtype + `","session_id":"s1","uuid":"u1"}`
			msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
			if err != nil {
				t.Fatalf("subtype %q: %v", subtype, err)
			}
			if len(msgs) != 1 {
				t.Fatalf("subtype %q: got %d messages, want 1", subtype, len(msgs))
			}
			sm, ok := msgs[0].(*agent.SystemMessage)
			if !ok {
				t.Fatalf("subtype %q: got %T, want *agent.SystemMessage", subtype, msgs[0])
			}
			if sm.Subtype != subtype {
				t.Errorf("subtype = %q, want %q", sm.Subtype, subtype)
			}
		}
	})
	t.Run("SystemTaskStarted", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"system","subtype":"task_started","session_id":"s1","uuid":"u1","task_id":"task-abc","description":"Explore codebase"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.SubagentStartMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.SubagentStartMessage", msgs[0])
		}
		if m.TaskID != "task-abc" {
			t.Errorf("task_id = %q, want %q", m.TaskID, "task-abc")
		}
		if m.Description != "Explore codebase" {
			t.Errorf("description = %q, want %q", m.Description, "Explore codebase")
		}
	})
	t.Run("SystemTaskNotification", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"system","subtype":"task_notification","session_id":"s1","uuid":"u1","task_id":"task-abc","status":"completed"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.SubagentEndMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.SubagentEndMessage", msgs[0])
		}
		if m.TaskID != "task-abc" {
			t.Errorf("task_id = %q, want %q", m.TaskID, "task-abc")
		}
		if m.Status != "completed" {
			t.Errorf("status = %q, want %q", m.Status, "completed")
		}
	})
	t.Run("SystemTaskUpdated", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"system","subtype":"task_updated","session_id":"s1","uuid":"u1","task_id":"task-abc","patch":{"status":"completed","end_time":1780832660165}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.SubagentEndMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.SubagentEndMessage", msgs[0])
		}
		if m.TaskID != "task-abc" {
			t.Errorf("task_id = %q, want %q", m.TaskID, "task-abc")
		}
		if m.Status != "completed" {
			t.Errorf("status = %q, want %q", m.Status, "completed")
		}
	})
	t.Run("SystemThinkingTokens", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"system","subtype":"thinking_tokens","estimated_tokens":138,"estimated_tokens_delta":88,"uuid":"u1","session_id":"s1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("got %d messages, want 0", len(msgs))
		}
	})
	t.Run("AssistantThinking", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"let me think..."},{"type":"text","text":"hello"}],"usage":{"input_tokens":10,"output_tokens":5}},"session_id":"abc","uuid":"u1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("got %d messages, want 3 (thinking + text + usage)", len(msgs))
		}
		tm, ok := msgs[0].(*agent.ThinkingMessage)
		if !ok {
			t.Fatalf("msgs[0] is %T, want *agent.ThinkingMessage", msgs[0])
		}
		if tm.Text != "let me think..." {
			t.Errorf("thinking = %q, want %q", tm.Text, "let me think...")
		}
		if _, ok := msgs[1].(*agent.TextMessage); !ok {
			t.Errorf("msgs[1] is %T, want *agent.TextMessage", msgs[1])
		}
		if _, ok := msgs[2].(*agent.UsageMessage); !ok {
			t.Errorf("msgs[2] is %T, want *agent.UsageMessage", msgs[2])
		}
	})
	t.Run("AssistantServerToolUseSkipped", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","id":"msg_01","role":"assistant","content":[{"type":"server_tool_use","id":"stu_1","name":"web_search"},{"type":"text","text":"result"}],"usage":{"input_tokens":10,"output_tokens":5}},"session_id":"abc","uuid":"u1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("got %d messages, want 2 (text + usage)", len(msgs))
		}
		if _, ok := msgs[0].(*agent.TextMessage); !ok {
			t.Errorf("msgs[0] is %T, want *agent.TextMessage", msgs[0])
		}
		if _, ok := msgs[1].(*agent.UsageMessage); !ok {
			t.Errorf("msgs[1] is %T, want *agent.UsageMessage", msgs[1])
		}
	})
	t.Run("AssistantOnlyThinking", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"deep thought"}],"usage":{}},"session_id":"abc","uuid":"u1"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		tm, ok := msgs[0].(*agent.ThinkingMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ThinkingMessage", msgs[0])
		}
		if tm.Text != "deep thought" {
			t.Errorf("text = %q, want %q", tm.Text, "deep thought")
		}
	})
	t.Run("StreamEventThinkingDelta", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"partial thought"}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.ThinkingDeltaMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ThinkingDeltaMessage", msgs[0])
		}
		if m.Text != "partial thought" {
			t.Errorf("text = %q, want %q", m.Text, "partial thought")
		}
	})
	t.Run("StreamEventMessageDeltaUsage", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":2,"output_tokens":192,"cache_read_input_tokens":409477,"output_tokens_details":{"thinking_tokens":49}}},"uuid":"u1","session_id":"s1","parent_tool_use_id":null}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.UsageMessage", msgs[0])
		}
		if m.Usage.OutputTokens != 192 {
			t.Errorf("OutputTokens = %d, want 192", m.Usage.OutputTokens)
		}
		if m.Usage.ReasoningOutputTokens != 49 {
			t.Errorf("ReasoningOutputTokens = %d, want 49", m.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("ResultUsesPendingReasoningTokens", func(t *testing.T) {
		t.Parallel()
		b := New()
		lines := []string{
			`{"type":"system","subtype":"thinking_tokens","estimated_tokens":138,"estimated_tokens_delta":88,"uuid":"u1","session_id":"s1"}`,
			`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":2,"output_tokens":192,"cache_read_input_tokens":409477,"output_tokens_details":{"thinking_tokens":49}}},"uuid":"u1","session_id":"s1","parent_tool_use_id":null}`,
			`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":200,"cache_read_input_tokens":500000,"output_tokens_details":{"thinking_tokens":51}}},"uuid":"u2","session_id":"s1","parent_tool_use_id":null}`,
		}
		for _, line := range lines {
			if _, err := b.ParseMessage([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"num_turns":3,"result":"done","total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50}}`
		msgs, err := b.ParseMessage([]byte(resultLine))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", msgs[0])
		}
		if m.Usage.ReasoningOutputTokens != 100 {
			t.Errorf("ReasoningOutputTokens = %d, want 100", m.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("ResultUsesEstimatedReasoningTokensWhenActualMissing", func(t *testing.T) {
		t.Parallel()
		b := New()
		lines := []string{
			`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"uuid":"u1","session_id":"s1"}`,
			`{"type":"system","subtype":"thinking_tokens","estimated_tokens":150,"estimated_tokens_delta":100,"uuid":"u2","session_id":"s1"}`,
		}
		for _, line := range lines {
			if _, err := b.ParseMessage([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"num_turns":1,"result":"done","total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20}}`
		msgs, err := b.ParseMessage([]byte(resultLine))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", msgs[0])
		}
		if m.Usage.ReasoningOutputTokens != 150 {
			t.Errorf("ReasoningOutputTokens = %d, want 150", m.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("StreamEventNoiseDropped", func(t *testing.T) {
		t.Parallel()
		noiseLines := []string{
			`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`,
			`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
			`{"type":"stream_event","event":{"type":"message_start","index":0}}`,
			`{"type":"stream_event","event":{"type":"message_stop","index":0}}`,
			`{"type":"stream_event","event":{"type":"message_delta","index":0}}`,
			`{"type":"stream_event","event":{"type":"ping","index":0}}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","text":"sig"}}}`,
		}
		for _, line := range noiseLines {
			msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
			if err != nil {
				t.Fatalf("line %s: %v", line, err)
			}
			if len(msgs) != 0 {
				t.Errorf("line %s: got %d messages, want 0", line, len(msgs))
			}
		}
	})
	t.Run("StreamEventError", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"stream_event","event":{"type":"error","index":0}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		sm, ok := msgs[0].(*agent.SystemMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.SystemMessage", msgs[0])
		}
		if sm.Subtype != "api_error" {
			t.Errorf("subtype = %q, want %q", sm.Subtype, "api_error")
		}
	})
	t.Run("AssistantWidgetToolUse", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"wid_1","name":"show_widget","input":{"widget_code":"<h1>Hello</h1>","title":"My Widget"}}],"usage":{}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		w, ok := msgs[0].(*agent.WidgetMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.WidgetMessage", msgs[0])
		}
		if w.ToolUseID != "wid_1" {
			t.Errorf("id = %q, want %q", w.ToolUseID, "wid_1")
		}
		if w.Title != "My Widget" {
			t.Errorf("title = %q, want %q", w.Title, "My Widget")
		}
		if w.HTML != "<h1>Hello</h1>" {
			t.Errorf("html = %q, want %q", w.HTML, "<h1>Hello</h1>")
		}
	})
	t.Run("WidgetStreamStart", func(t *testing.T) {
		t.Parallel()
		wt := NewWidgetTracker()
		line := `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"wid_2","name":"show_widget"}}}`
		msgs, err := parseMessageWithTracker([]byte(line), wt, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages, want 0 (start is absorbed)", len(msgs))
		}
		if _, ok := wt.activeWidgets[0]; !ok {
			t.Error("widget not tracked after content_block_start")
		}
	})
	t.Run("WidgetInputDelta", func(t *testing.T) {
		t.Parallel()
		wt := NewWidgetTracker()
		// Register a widget block.
		start := `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"wid_3","name":"show_widget"}}}`
		if _, err := parseMessageWithTracker([]byte(start), wt, &jsonutil.FieldWarner{}); err != nil {
			t.Fatal(err)
		}
		// Send partial JSON with widget_code.
		delta := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"widget_code\":\"<h1>Hi"}}}`
		msgs, err := parseMessageWithTracker([]byte(delta), wt, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		wd, ok := msgs[0].(*agent.WidgetDeltaMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.WidgetDeltaMessage", msgs[0])
		}
		if wd.ToolUseID != "wid_3" {
			t.Errorf("id = %q, want %q", wd.ToolUseID, "wid_3")
		}
		if wd.Delta != "<h1>Hi" {
			t.Errorf("delta = %q, want %q", wd.Delta, "<h1>Hi")
		}
	})
	t.Run("NonWidgetInputDeltaIgnored", func(t *testing.T) {
		t.Parallel()
		// Without tracker, input_json_delta should be dropped (normal parse path).
		line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages, want 0", len(msgs))
		}
	})
	t.Run("WidgetBlockStop", func(t *testing.T) {
		t.Parallel()
		wt := NewWidgetTracker()
		start := `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"wid_4","name":"show_widget"}}}`
		if _, err := parseMessageWithTracker([]byte(start), wt, &jsonutil.FieldWarner{}); err != nil {
			t.Fatal(err)
		}
		stop := `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`
		msgs, err := parseMessageWithTracker([]byte(stop), wt, &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages on stop, want 0", len(msgs))
		}
		if _, ok := wt.activeWidgets[0]; ok {
			t.Error("widget should be cleaned up after stop")
		}
	})
	t.Run("ExtractPartialWidgetCode", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name  string
			input string
			want  string
		}{
			{"NoMarker", `{"title":"x"}`, ""},
			{"EmptyCode", `{"widget_code":""}`, ""},
			{"SimpleHTML", `{"widget_code":"<h1>Hi</h1>"}`, "<h1>Hi</h1>"},
			{"Unterminated", `{"widget_code":"<h1>partial`, "<h1>partial"},
			{"Escapes", `{"widget_code":"a\nb\\c\"d"}`, "a\nb\\c\"d"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := extractPartialWidgetCode(tc.input)
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	})
	t.Run("SkillToolUseSuppressed", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"sk_1","name":"Skill","input":{"skill":"widget-plugin:widget"}}],"usage":{}}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		// Skill tool_use is suppressed; only a raw fallback for the empty assistant message.
		for _, m := range msgs {
			if _, ok := m.(*agent.ToolUseMessage); ok {
				t.Error("Skill tool_use should be suppressed, got ToolUseMessage")
			}
		}
	})
	t.Run("SyntheticUserSuppressed", func(t *testing.T) {
		t.Parallel()
		// Claude Code sets isSynthetic:true on skill context injections.
		line := `{"type":"user","isSynthetic":true,"message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /tmp/widget-plugin/skills/widget\n\n# Widget Rendering"}]}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Errorf("isSynthetic user should be suppressed, got %d messages", len(msgs))
		}
	})
	t.Run("SyntheticFalseNotSuppressed", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","isSynthetic":false,"message":{"role":"user","content":"hello"}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "hello" {
			t.Errorf("text = %q, want %q", ui.Text, "hello")
		}
	})
	t.Run("NormalUserInputNotSuppressed", func(t *testing.T) {
		t.Parallel()
		line := `{"type":"user","message":{"role":"user","content":"explain this code"}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "explain this code" {
			t.Errorf("text = %q, want %q", ui.Text, "explain this code")
		}
	})
	t.Run("RateLimitEvent", func(t *testing.T) {
		t.Parallel()
		// Wire format uses camelCase (matches Claude Code CLI JSON output).
		line := `{"type":"rate_limit_event","uuid":"u1","session_id":"s1","rate_limit_info":{"status":"allowed_warning","resetsAt":1711000000,"rateLimitType":"five_hour","utilization":0.85,"isUsingOverage":false}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != agent.RateLimitStatusAllowedWarning {
			t.Errorf("status = %q, want %q", rl.Status, agent.RateLimitStatusAllowedWarning)
		}
		wantReset := time.Unix(1711000000, 0).UTC()
		if !rl.ResetsAt.Equal(wantReset) {
			t.Errorf("resets_at = %v, want %v", rl.ResetsAt, wantReset)
		}
		if rl.RateLimitType != string(genclaudecode.RateLimitFiveHour) {
			t.Errorf("rate_limit_type = %q, want %q", rl.RateLimitType, genclaudecode.RateLimitFiveHour)
		}
		if rl.QuotaProvider != agent.QuotaProviderClaudeCode || rl.QuotaLabel != "Claude Code" || rl.QuotaWindow != "5h" {
			t.Errorf("canonical quota = (%q, %q, %q), want (%q, Claude Code, 5h)", rl.QuotaProvider, rl.QuotaLabel, rl.QuotaWindow, agent.QuotaProviderClaudeCode)
		}
		if rl.Utilization != 0.85 {
			t.Errorf("utilization = %v, want 0.85", rl.Utilization)
		}
		if rl.IsUsingOverage {
			t.Error("is_using_overage = true, want false")
		}
	})
	t.Run("RateLimitWindowCanonicalization", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name      string
			wire      genclaudecode.RateLimitType
			canonical string
		}{
			{name: "FiveHour", wire: genclaudecode.RateLimitFiveHour, canonical: "5h"},
			{name: "SevenDay", wire: genclaudecode.RateLimitSevenDay, canonical: "7d"},
			{name: "SevenDayOpus", wire: genclaudecode.RateLimitSevenDayOpus, canonical: "7d"},
			{name: "SevenDaySonnet", wire: genclaudecode.RateLimitSevenDaySonnet, canonical: "7d"},
			{name: "SevenDayOverageIncluded", wire: genclaudecode.RateLimitSevenDayOverage, canonical: "7d"},
			{name: "Overage", wire: genclaudecode.RateLimitOverage, canonical: "overage"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				line := `{"type":"rate_limit_event","uuid":"u1","session_id":"s1","rate_limit_info":{"status":"allowed","rateLimitType":"` + string(tt.wire) + `"}}`
				msgs, err := parseMessage([]byte(line), nil)
				if err != nil {
					t.Fatal(err)
				}
				rl, ok := msgs[0].(*agent.RateLimitMessage)
				if !ok {
					t.Fatalf("message = %T, want *agent.RateLimitMessage", msgs[0])
				}
				if rl.QuotaWindow != tt.canonical {
					t.Errorf("QuotaWindow = %q, want %q", rl.QuotaWindow, tt.canonical)
				}
			})
		}
	})
	t.Run("RateLimitEventOverage", func(t *testing.T) {
		t.Parallel()
		// When the plan limit is hit but overage is allowed, status is "rejected"
		// with isUsingOverage=true and overageResetsAt set.
		line := `{"type":"rate_limit_event","uuid":"u1","session_id":"s1","rate_limit_info":{"status":"rejected","resetsAt":1775340000,"rateLimitType":"five_hour","overageStatus":"allowed","overageResetsAt":1777593600,"isUsingOverage":true}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != agent.RateLimitStatusRejected {
			t.Errorf("status = %q, want %q", rl.Status, agent.RateLimitStatusRejected)
		}
		if rl.RateLimitType != string(genclaudecode.RateLimitFiveHour) {
			t.Errorf("rate_limit_type = %q, want %q", rl.RateLimitType, genclaudecode.RateLimitFiveHour)
		}
		if rl.QuotaProvider != agent.QuotaProviderClaudeCode || rl.QuotaWindow != "5h" {
			t.Errorf("canonical quota = (%q, %q), want (%q, 5h)", rl.QuotaProvider, rl.QuotaWindow, agent.QuotaProviderClaudeCode)
		}
		if !rl.IsUsingOverage {
			t.Error("is_using_overage = false, want true")
		}
		wantOverageReset := time.Unix(1777593600, 0).UTC()
		if !rl.OverageResetsAt.Equal(wantOverageReset) {
			t.Errorf("overage_resets_at = %v, want %v", rl.OverageResetsAt, wantOverageReset)
		}
	})
	t.Run("RateLimitEventMinimal", func(t *testing.T) {
		t.Parallel()
		// Only status is required; other fields may be absent.
		line := `{"type":"rate_limit_event","uuid":"u1","session_id":"s1","rate_limit_info":{"status":"rejected"}}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != agent.RateLimitStatusRejected {
			t.Errorf("status = %q, want %q", rl.Status, agent.RateLimitStatusRejected)
		}
		if !rl.ResetsAt.IsZero() {
			t.Errorf("resets_at = %v, want zero", rl.ResetsAt)
		}
	})
	t.Run("UnknownFieldsForwardCompat", func(t *testing.T) {
		t.Parallel()
		// An init record with an extra unknown field should parse successfully
		// (forward compatibility). The known fields must still be extracted.
		line := `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"s1","tools":[],"model":"m","claude_code_version":"1.0","uuid":"u1","brand_new_field":"surprise"}`
		msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatalf("unknown field caused error: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		m, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.InitMessage", msgs[0])
		}
		if m.SessionID != "s1" {
			t.Errorf("session_id = %q, want %q", m.SessionID, "s1")
		}
		if m.Model != "m" {
			t.Errorf("model = %q, want %q", m.Model, "m")
		}
	})
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("reports supported effort levels per model", func(t *testing.T) {
		t.Parallel()

		models := New().ModelInventory().Models
		if len(models) != 4 {
			t.Fatalf("len(ModelInventory().Models) = %d, want 4", len(models))
		}
		want := []string{"low", "medium", "high", "xhigh", "max"}
		for _, model := range models {
			if !slices.Equal(model.EffortOptions, want) {
				t.Fatalf("%s effort options = %v, want %v", model.ID, model.EffortOptions, want)
			}
		}
	})

	t.Run("edit tool display", func(t *testing.T) {
		t.Parallel()

		use := &agent.ToolUseMessage{
			Name:  "Edit",
			Input: json.RawMessage(`{"file_path":"src/main.ts","old_string":"before","new_string":"after"}`),
		}
		addEditInputView(use)
		if use.Detail != "main.ts" {
			t.Fatalf("detail = %q", use.Detail)
		}
		if use.InputView.Kind != agent.ToolInputFileChanges || len(use.InputView.Files) != 1 {
			t.Fatalf("input view = %#v", use.InputView)
		}
		file := use.InputView.Files[0]
		if file.Path != "src/main.ts" || !strings.Contains(file.Patch, "-before\n+after\n") {
			t.Fatalf("file change = %#v", file)
		}
	})
	t.Run("read edit bash fixture normalizes edit", func(t *testing.T) {
		t.Parallel()

		msgs := agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", New().NewWire().ParseMessage)
		var use *agent.ToolUseMessage
		for _, msg := range msgs {
			candidate, ok := msg.(*agent.ToolUseMessage)
			if ok && candidate.Name == "Edit" && candidate.InputView.Kind == agent.ToolInputFileChanges {
				use = candidate
				break
			}
		}
		if use == nil {
			t.Fatal("no normalized Edit tool use found")
		}
		if use.Detail != "main.go" {
			t.Fatalf("detail = %q, want main.go", use.Detail)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		file := use.InputView.Files[0]
		if file.Path != "/workspace/main.go" || !strings.Contains(file.Patch, "-\tfmt.Println(\"Hello, World!\")") || !strings.Contains(file.Patch, "+\tfmt.Println(\"Hi, World!\")") {
			t.Fatalf("file change = %#v", file)
		}
	})
	t.Run("edit input", func(t *testing.T) {
		t.Parallel()

		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			p, replacements, ok := parseEditInput(json.RawMessage(`{"file_path":"src/main.ts","old_string":"before","new_string":"after"}`))
			if !ok {
				t.Fatal("parseEditInput returned false")
			}
			if p != "src/main.ts" || len(replacements) != 1 || replacements[0].OldText != "before" || replacements[0].NewText != "after" {
				t.Fatalf("path/replacements = %q/%+v", p, replacements)
			}
		})

		t.Run("multi edit", func(t *testing.T) {
			t.Parallel()
			_, replacements, ok := parseEditInput(json.RawMessage(`{"file_path":"src/main.ts","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d"}]}`))
			if !ok {
				t.Fatal("parseEditInput returned false")
			}
			if len(replacements) != 2 {
				t.Fatalf("got %d replacements, want 2", len(replacements))
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, _, ok := parseEditInput(json.RawMessage(`{"file_path":"src/main.ts","new_string":"after"}`)); ok {
				t.Fatal("parseEditInput accepted missing old string")
			}
		})
	})
}
