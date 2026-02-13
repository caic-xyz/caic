package gemini

import (
	"encoding/json"
	"testing"
)

func TestRecord(t *testing.T) {
	t.Run("Init", func(t *testing.T) {
		const input = `{"type":"init","timestamp":"2026-02-13T19:00:05.416Z","session_id":"730077db-6c7a-4e97-8285-252a9ad8ae87","model":"auto-gemini-3"}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Type != TypeInit {
			t.Fatalf("Type = %q, want %q", rec.Type, TypeInit)
		}
		r, err := rec.AsInit()
		if err != nil {
			t.Fatal(err)
		}
		if r.SessionID != "730077db-6c7a-4e97-8285-252a9ad8ae87" {
			t.Errorf("SessionID = %q", r.SessionID)
		}
		if r.Model != "auto-gemini-3" {
			t.Errorf("Model = %q", r.Model)
		}
		if len(r.Extra) != 0 {
			t.Errorf("unexpected extra fields: %v", r.Extra)
		}
	})
	t.Run("UnknownFields", func(t *testing.T) {
		const input = `{"type":"init","timestamp":"2026-02-13T19:00:05.416Z","session_id":"s","model":"m","new_field":"surprise"}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		r, err := rec.AsInit()
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 unknown field", r.Extra)
		}
		if _, ok := r.Extra["new_field"]; !ok {
			t.Error("expected 'new_field' in Extra")
		}
	})
}

func TestMessageRecord(t *testing.T) {
	t.Run("User", func(t *testing.T) {
		const input = `{"type":"message","timestamp":"2026-02-13T19:00:05.418Z","role":"user","content":"Say hello"}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		r, err := rec.AsMessage()
		if err != nil {
			t.Fatal(err)
		}
		if r.Role != "user" {
			t.Errorf("Role = %q", r.Role)
		}
		if r.Content != "Say hello" {
			t.Errorf("Content = %q", r.Content)
		}
		if r.Delta {
			t.Error("Delta should be false for user message")
		}
	})
	t.Run("AssistantDelta", func(t *testing.T) {
		const input = `{"type":"message","timestamp":"2026-02-13T19:00:10.729Z","role":"assistant","content":"Hello.","delta":true}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		r, err := rec.AsMessage()
		if err != nil {
			t.Fatal(err)
		}
		if r.Role != "assistant" {
			t.Errorf("Role = %q", r.Role)
		}
		if !r.Delta {
			t.Error("Delta should be true")
		}
	})
}

func TestToolUseRecord(t *testing.T) {
	const input = `{"type":"tool_use","timestamp":"2026-02-13T19:00:22.912Z","tool_name":"read_file","tool_id":"read_file-1771009222912-ae333db2dcc5d8","parameters":{"file_path":"/etc/hostname"}}`
	var rec Record
	if err := json.Unmarshal([]byte(input), &rec); err != nil {
		t.Fatal(err)
	}
	r, err := rec.AsToolUse()
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolName != "read_file" {
		t.Errorf("ToolName = %q", r.ToolName)
	}
	if r.ToolID != "read_file-1771009222912-ae333db2dcc5d8" {
		t.Errorf("ToolID = %q", r.ToolID)
	}
	var params map[string]string
	if err := json.Unmarshal(r.Parameters, &params); err != nil {
		t.Fatal(err)
	}
	if params["file_path"] != "/etc/hostname" {
		t.Errorf("file_path = %q", params["file_path"])
	}
}

func TestToolResultRecord(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		const input = `{"type":"tool_result","timestamp":"2026-02-13T19:00:26.397Z","tool_id":"run_shell_command-123","status":"success","output":"md-caic-w0"}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		r, err := rec.AsToolResult()
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "success" {
			t.Errorf("Status = %q", r.Status)
		}
		if r.Output != "md-caic-w0" {
			t.Errorf("Output = %q", r.Output)
		}
		if r.Error != nil {
			t.Errorf("Error should be nil")
		}
	})
	t.Run("Error", func(t *testing.T) {
		const input = `{"type":"tool_result","timestamp":"2026-02-13T19:00:22.918Z","tool_id":"read_file-123","status":"error","output":"Path not in workspace","error":{"type":"invalid_tool_params","message":"Path not in workspace"}}`
		var rec Record
		if err := json.Unmarshal([]byte(input), &rec); err != nil {
			t.Fatal(err)
		}
		r, err := rec.AsToolResult()
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "error" {
			t.Errorf("Status = %q", r.Status)
		}
		if r.Error == nil {
			t.Fatal("Error should not be nil")
		}
		if r.Error.Type != "invalid_tool_params" {
			t.Errorf("Error.Type = %q", r.Error.Type)
		}
		if r.Error.Message != "Path not in workspace" {
			t.Errorf("Error.Message = %q", r.Error.Message)
		}
	})
}

func TestResultRecord(t *testing.T) {
	const input = `{"type":"result","timestamp":"2026-02-13T19:00:10.738Z","status":"success","stats":{"total_tokens":12359,"input_tokens":11744,"output_tokens":47,"cached":0,"input":11744,"duration_ms":5322,"tool_calls":0}}`
	var rec Record
	if err := json.Unmarshal([]byte(input), &rec); err != nil {
		t.Fatal(err)
	}
	r, err := rec.AsResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "success" {
		t.Errorf("Status = %q", r.Status)
	}
	if r.Stats == nil {
		t.Fatal("Stats should not be nil")
	}
	if r.Stats.TotalTokens != 12359 {
		t.Errorf("TotalTokens = %d", r.Stats.TotalTokens)
	}
	if r.Stats.InputTokens != 11744 {
		t.Errorf("InputTokens = %d", r.Stats.InputTokens)
	}
	if r.Stats.OutputTokens != 47 {
		t.Errorf("OutputTokens = %d", r.Stats.OutputTokens)
	}
	if r.Stats.DurationMs != 5322 {
		t.Errorf("DurationMs = %d", r.Stats.DurationMs)
	}
}
