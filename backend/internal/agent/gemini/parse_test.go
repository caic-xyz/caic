package gemini

import (
	"testing"

	"github.com/maruel/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Run("Init", func(t *testing.T) {
		const input = `{"type":"init","timestamp":"2026-02-13T19:00:05.416Z","session_id":"abc","model":"auto-gemini-3"}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		init, ok := msg.(*agent.SystemInitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.SystemInitMessage", msg)
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
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		if len(am.Message.Content) != 1 {
			t.Fatalf("content blocks = %d, want 1", len(am.Message.Content))
		}
		if am.Message.Content[0].Type != "text" {
			t.Errorf("content type = %q, want text", am.Message.Content[0].Type)
		}
		if am.Message.Content[0].Text != "Hello." {
			t.Errorf("text = %q", am.Message.Content[0].Text)
		}
	})
	t.Run("UserMessage", func(t *testing.T) {
		const input = `{"type":"message","timestamp":"2026-02-13T19:00:05.418Z","role":"user","content":"Say hello"}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		um, ok := msg.(*agent.UserMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserMessage", msg)
		}
		if um.Type() != "user" {
			t.Errorf("Type() = %q", um.Type())
		}
	})
	t.Run("ToolUse", func(t *testing.T) {
		const input = `{"type":"tool_use","timestamp":"2026-02-13T19:00:22.912Z","tool_name":"read_file","tool_id":"read_file-123","parameters":{"file_path":"/etc/hostname"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		if len(am.Message.Content) != 1 {
			t.Fatalf("content blocks = %d, want 1", len(am.Message.Content))
		}
		cb := am.Message.Content[0]
		if cb.Type != "tool_use" {
			t.Errorf("content type = %q, want tool_use", cb.Type)
		}
		if cb.Name != "Read" {
			t.Errorf("Name = %q, want Read (normalized from read_file)", cb.Name)
		}
		if cb.ID != "read_file-123" {
			t.Errorf("ID = %q", cb.ID)
		}
	})
	t.Run("ToolResult", func(t *testing.T) {
		const input = `{"type":"tool_result","timestamp":"2026-02-13T19:00:26.397Z","tool_id":"run_shell_command-123","status":"success","output":"md-caic-w0"}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		um, ok := msg.(*agent.UserMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserMessage", msg)
		}
		if um.ParentToolUseID == nil || *um.ParentToolUseID != "run_shell_command-123" {
			t.Errorf("ParentToolUseID = %v", um.ParentToolUseID)
		}
	})
	t.Run("ResultSuccess", func(t *testing.T) {
		const input = `{"type":"result","timestamp":"2026-02-13T19:00:10.738Z","status":"success","stats":{"total_tokens":12359,"input_tokens":11744,"output_tokens":47,"cached":0,"input":11744,"duration_ms":5322,"tool_calls":2}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msg.(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msg)
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
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msg.(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msg)
		}
		if !rm.IsError {
			t.Error("IsError should be true for error status")
		}
	})
	t.Run("UnknownType", func(t *testing.T) {
		const input = `{"type":"unknown_event","data":"something"}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := msg.(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msg)
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
