package claude

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Run("SystemInit", func(t *testing.T) {
		line := `{"type":"system","subtype":"init","cwd":"/home/user","session_id":"abc-123","tools":["Bash","Read"],"model":"claude-opus-4-6","claude_code_version":"2.1.34","uuid":"uuid-1"}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_01","role":"assistant","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":10,"output_tokens":5}},"session_id":"abc","uuid":"u1"}`
		msgs, err := ParseMessage([]byte(line))
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
		if um.Model != "claude-opus-4-6" {
			t.Errorf("model = %q, want %q", um.Model, "claude-opus-4-6")
		}
	})
	t.Run("AssistantToolUse", func(t *testing.T) {
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}],"usage":{}}}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"ask_1","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","header":"Pick","options":[{"label":"A"},{"label":"B"}]}]}}],"usage":{}}}`
		msgs, err := ParseMessage([]byte(line))
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
	})
	t.Run("AssistantTodoWrite", func(t *testing.T) {
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"td_1","name":"TodoWrite","input":{"todos":[{"content":"Fix bug","status":"pending","activeForm":"Fixing bug"}]}}],"usage":{}}}`
		msgs, err := ParseMessage([]byte(line))
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
	})
	t.Run("AssistantMultiBlock", func(t *testing.T) {
		line := `{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"thinking..."},{"type":"tool_use","id":"tu_1","name":"Read","input":{"file":"x.go"}}],"usage":{"input_tokens":100,"output_tokens":50}}}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"user","message":{"role":"user","content":"hello"}}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"user","message":{"content":[{"type":"text","text":"ok"}],"is_error":false},"parent_tool_use_id":"tu_1"}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"user","message":{"content":[{"type":"text","text":"file not found"}],"is_error":true},"parent_tool_use_id":"tu_2"}`
		msgs, err := ParseMessage([]byte(line))
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
	t.Run("Result", func(t *testing.T) {
		line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"num_turns":3,"result":"done","total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50}}`
		msgs, err := ParseMessage([]byte(line))
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
	})
	t.Run("StreamEventTextDelta", func(t *testing.T) {
		line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}}`
		msgs, err := ParseMessage([]byte(line))
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
		line := `{"type":"caic_diff_stat","diff_stat":[{"path":"main.go","added":10,"deleted":3}]}`
		msgs, err := ParseMessage([]byte(line))
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
	})
	t.Run("RawFallback", func(t *testing.T) {
		line := `{"type":"tool_progress","data":"some progress"}`
		msgs, err := ParseMessage([]byte(line))
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
}
