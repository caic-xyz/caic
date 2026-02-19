package codex

import (
	"encoding/json"
	"testing"
)

func TestJSONRPCMessage(t *testing.T) {
	t.Run("Notification", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"t1"}}}`
		var msg JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != MethodThreadStarted {
			t.Errorf("Method = %q, want %q", msg.Method, MethodThreadStarted)
		}
		if msg.IsResponse() {
			t.Error("IsResponse() = true, want false for notification")
		}
	})
	t.Run("Response", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"t1"}}}`
		var msg JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if !msg.IsResponse() {
			t.Error("IsResponse() = false, want true for response")
		}
	})
	t.Run("ErrorResponse", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"invalid request"}}`
		var msg JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if !msg.IsResponse() {
			t.Error("IsResponse() = false, want true for error response")
		}
		if msg.Error == nil {
			t.Fatal("Error = nil")
		}
		if msg.Error.Code != -32600 {
			t.Errorf("Error.Code = %d", msg.Error.Code)
		}
		if msg.Error.Message != "invalid request" {
			t.Errorf("Error.Message = %q", msg.Error.Message)
		}
	})
}

func TestThreadStartedParams(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}`
		var p ThreadStartedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Thread.ID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
			t.Errorf("Thread.ID = %q", p.Thread.ID)
		}
		if len(p.Extra) != 0 {
			t.Errorf("unexpected extra fields: %v", p.Extra)
		}
	})
	t.Run("UnknownFields", func(t *testing.T) {
		const input = `{"thread":{"id":"t1"},"new_field":"surprise"}`
		var p ThreadStartedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 unknown field", p.Extra)
		}
		if _, ok := p.Extra["new_field"]; !ok {
			t.Error("expected 'new_field' in Extra")
		}
	})
}

func TestTurnCompletedParams(t *testing.T) {
	t.Run("Completed", func(t *testing.T) {
		const input = `{"turn":{"status":"completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}}`
		var p TurnCompletedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Turn.Status != "completed" {
			t.Errorf("Status = %q", p.Turn.Status)
		}
		if p.Turn.Usage.InputTokens != 24763 {
			t.Errorf("InputTokens = %d", p.Turn.Usage.InputTokens)
		}
		if p.Turn.Usage.CachedInputTokens != 24448 {
			t.Errorf("CachedInputTokens = %d", p.Turn.Usage.CachedInputTokens)
		}
		if p.Turn.Usage.OutputTokens != 122 {
			t.Errorf("OutputTokens = %d", p.Turn.Usage.OutputTokens)
		}
	})
	t.Run("Failed", func(t *testing.T) {
		const input = `{"turn":{"status":"failed","error":"something went wrong","usage":{}}}`
		var p TurnCompletedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Turn.Status != "failed" {
			t.Errorf("Status = %q", p.Turn.Status)
		}
		if p.Turn.Error != "something went wrong" {
			t.Errorf("Error = %q", p.Turn.Error)
		}
	})
}

func TestItemParams(t *testing.T) {
	t.Run("CommandExecution", func(t *testing.T) {
		const input = `{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsrc\n","exit_code":0,"status":"completed"}}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Item.ID != "item_1" {
			t.Errorf("ID = %q", p.Item.ID)
		}
		if p.Item.Type != ItemTypeCommandExecution {
			t.Errorf("Type = %q", p.Item.Type)
		}
		if p.Item.Command != "bash -lc ls" {
			t.Errorf("Command = %q", p.Item.Command)
		}
		if p.Item.AggregatedOutput != "docs\nsrc\n" {
			t.Errorf("AggregatedOutput = %q", p.Item.AggregatedOutput)
		}
		if p.Item.ExitCode == nil || *p.Item.ExitCode != 0 {
			t.Errorf("ExitCode = %v", p.Item.ExitCode)
		}
	})
	t.Run("FileChange", func(t *testing.T) {
		const input = `{"item":{"id":"item_4","type":"file_change","changes":[{"path":"docs/foo.md","kind":"add"}],"status":"completed"}}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Item.Type != ItemTypeFileChange {
			t.Errorf("Type = %q", p.Item.Type)
		}
		if len(p.Item.Changes) != 1 {
			t.Fatalf("Changes = %d, want 1", len(p.Item.Changes))
		}
		if p.Item.Changes[0].Path != "docs/foo.md" {
			t.Errorf("Path = %q", p.Item.Changes[0].Path)
		}
		if p.Item.Changes[0].Kind != "add" {
			t.Errorf("Kind = %q", p.Item.Changes[0].Kind)
		}
	})
	t.Run("AgentMessage", func(t *testing.T) {
		const input = `{"item":{"id":"item_3","type":"agent_message","text":"Done.","status":"completed"}}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Item.Type != ItemTypeAgentMessage {
			t.Errorf("Type = %q", p.Item.Type)
		}
		if p.Item.Text != "Done." {
			t.Errorf("Text = %q", p.Item.Text)
		}
	})
	t.Run("Reasoning", func(t *testing.T) {
		const input = `{"item":{"id":"item_0","type":"reasoning","text":"**Scanning...**","status":"completed"}}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Item.Type != ItemTypeReasoning {
			t.Errorf("Type = %q", p.Item.Type)
		}
		if p.Item.Text != "**Scanning...**" {
			t.Errorf("Text = %q", p.Item.Text)
		}
	})
}

func TestItemDeltaParams(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"item_id":"item_3","delta":"Hello "}`
		var p ItemDeltaParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.ItemID != "item_3" {
			t.Errorf("ItemID = %q", p.ItemID)
		}
		if p.Delta != "Hello " {
			t.Errorf("Delta = %q", p.Delta)
		}
	})
	t.Run("UnknownFields", func(t *testing.T) {
		const input = `{"item_id":"x","delta":"y","new_field":42}`
		var p ItemDeltaParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 unknown field", p.Extra)
		}
	})
}
