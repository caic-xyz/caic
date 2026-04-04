package gemini

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

func TestParseMessage(t *testing.T) {
	t.Run("Init", func(t *testing.T) {
		const input = `{"type":"init","timestamp":"2026-02-13T19:00:05.416Z","session_id":"abc","model":"auto-gemini-3"}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
		if init.SessionID != "abc" {
			t.Errorf("SessionID = %q", init.SessionID)
		}
		if init.Model != "auto-gemini-3" {
			t.Errorf("Model = %q", init.Model)
		}
	})
	t.Run("AssistantText", func(t *testing.T) {
		const input = `{"type":"message","timestamp":"2026-02-13T19:00:10.729Z","role":"assistant","content":"Hello.","delta":true}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tm, ok := msgs[0].(*agent.TextMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.TextMessage", msgs[0])
		}
		if tm.Text != "Hello." {
			t.Errorf("Text = %q", tm.Text)
		}
	})
	t.Run("UserMessage", func(t *testing.T) {
		const input = `{"type":"message","timestamp":"2026-02-13T19:00:05.418Z","role":"user","content":"Say hello"}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		um, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if um.Type() != "user_input" {
			t.Errorf("Type() = %q", um.Type())
		}
	})
	t.Run("ToolUse", func(t *testing.T) {
		const input = `{"type":"tool_use","timestamp":"2026-02-13T19:00:22.912Z","tool_name":"read_file","tool_id":"read_file-123","parameters":{"file_path":"/etc/hostname"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
		if tu.Name != "Read" {
			t.Errorf("Name = %q, want Read (normalized from read_file)", tu.Name)
		}
		if tu.ToolUseID != "read_file-123" {
			t.Errorf("ToolUseID = %q", tu.ToolUseID)
		}
	})
	t.Run("ToolResult", func(t *testing.T) {
		const input = `{"type":"tool_result","timestamp":"2026-02-13T19:00:26.397Z","tool_id":"run_shell_command-123","status":"success","output":"md-caic-0"}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
		if tr.ToolUseID != "run_shell_command-123" {
			t.Errorf("ToolUseID = %q", tr.ToolUseID)
		}
	})
	t.Run("ResultSuccess", func(t *testing.T) {
		const input = `{"type":"result","timestamp":"2026-02-13T19:00:10.738Z","status":"success","stats":{"total_tokens":12359,"input_tokens":11744,"output_tokens":47,"cached":0,"input":11744,"duration_ms":5322,"tool_calls":2}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
			t.Error("IsError should be false for success")
		}
		if rm.DurationMs != 5322 {
			t.Errorf("DurationMs = %d", rm.DurationMs)
		}
		if rm.NumTurns != 2 {
			t.Errorf("NumTurns = %d, want 2 (from tool_calls)", rm.NumTurns)
		}
		if rm.Usage.InputTokens != 11744 {
			t.Errorf("InputTokens = %d", rm.Usage.InputTokens)
		}
		if rm.Usage.OutputTokens != 47 {
			t.Errorf("OutputTokens = %d", rm.Usage.OutputTokens)
		}
	})
	t.Run("ResultError", func(t *testing.T) {
		const input = `{"type":"result","timestamp":"2026-02-13T19:00:10.738Z","status":"error","stats":{"total_tokens":0,"input_tokens":0,"output_tokens":0,"cached":0,"input":0,"duration_ms":100,"tool_calls":0}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
			t.Error("IsError should be true for error status")
		}
	})
	t.Run("UnknownType", func(t *testing.T) {
		const input = `{"type":"unknown_event","data":"something"}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		raw, ok := msgs[0].(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msgs[0])
		}
		if raw.Type() != "unknown_event" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
}

func TestNormalizeToolName(t *testing.T) {
	tests := []struct {
		gemini string
		want   string
	}{
		{"read_file", "Read"},
		{"read_many_files", "Read"},
		{"write_file", "Write"},
		{"replace", "Edit"},
		{"run_shell_command", "Bash"},
		{"grep", "Grep"},
		{"grep_search", "Grep"},
		{"glob", "Glob"},
		{"web_fetch", "WebFetch"},
		{"google_web_search", "WebSearch"},
		{"ask_user", "AskUserQuestion"},
		{"write_todos", "TodoWrite"},
		{"list_directory", "ListDirectory"},
		{"some_new_tool", "some_new_tool"},
	}
	for _, tt := range tests {
		t.Run(tt.gemini, func(t *testing.T) {
			if got := normalizeToolName(tt.gemini); got != tt.want {
				t.Errorf("normalizeToolName(%q) = %q, want %q", tt.gemini, got, tt.want)
			}
		})
	}
}
