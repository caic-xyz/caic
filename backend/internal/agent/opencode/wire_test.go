package opencode

import (
	"encoding/json"
	"testing"

	oc "github.com/maruel/genai/providers/opencode"
)

func TestJSONRPCMessage(t *testing.T) {
	t.Run("Notification", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{}}}`
		var msg oc.JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != oc.MethodSessionUpdate {
			t.Errorf("Method = %q, want %q", msg.Method, oc.MethodSessionUpdate)
		}
		if msg.IsResponse() {
			t.Error("IsResponse() = true, want false for notification")
		}
	})
	t.Run("Response", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1"}}`
		var msg oc.JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if !msg.IsResponse() {
			t.Error("IsResponse() = false, want true for response")
		}
	})
	t.Run("ErrorResponse", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"invalid request"}}`
		var msg oc.JSONRPCMessage
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

func TestAgentMessageChunkUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}`
		var u oc.AgentMessageChunkUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Content.Text != "hello" {
			t.Errorf("Content.Text = %q", u.Content.Text)
		}
	})
}

func TestToolCallUpdate(t *testing.T) {
	t.Run("AllKnownFields", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","kind":"execute","status":"pending","locations":[{"path":"/tmp/a.go","line":10}],"rawInput":{"command":"ls"}}`
		var u oc.ToolCallUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.ToolCallID != "c1" {
			t.Errorf("ToolCallID = %q", u.ToolCallID)
		}
		if u.Title != "bash" {
			t.Errorf("Title = %q", u.Title)
		}
		if u.Kind != oc.KindExecute {
			t.Errorf("Kind = %q", u.Kind)
		}
		if len(u.Locations) != 1 || u.Locations[0].Path != "/tmp/a.go" || u.Locations[0].Line != 10 {
			t.Errorf("Locations = %+v", u.Locations)
		}
	})
}

func TestToolCallUpdateUpdate(t *testing.T) {
	t.Run("CompletedWithContent", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}`
		var u oc.ToolCallUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Status != oc.StatusCompleted {
			t.Errorf("Status = %q", u.Status)
		}
		if len(u.Content) != 1 || u.Content[0].Content.Text != "done" {
			t.Errorf("Content = %+v", u.Content)
		}
	})
	t.Run("WithRawOutput", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"failed","rawOutput":{"output":"","error":"permission denied","metadata":{"code":1}}}`
		var u oc.ToolCallUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.RawOutput == nil {
			t.Fatal("RawOutput = nil")
		}
		if u.RawOutput.Error != "permission denied" {
			t.Errorf("RawOutput.Error = %q", u.RawOutput.Error)
		}
	})
	t.Run("DiffContent", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","content":[{"type":"diff","path":"main.go","oldText":"old","newText":"new"}]}`
		var u oc.ToolCallUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Content) != 1 {
			t.Fatalf("Content len = %d", len(u.Content))
		}
		c := u.Content[0]
		if c.Type != "diff" || c.Path != "main.go" || c.OldText != "old" || c.NewText != "new" {
			t.Errorf("Content[0] = %+v", c)
		}
	})
}

func TestPlanUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"plan","entries":[{"priority":"medium","status":"completed","content":"step 1"},{"status":"pending","content":"step 2"}]}`
		var u oc.PlanUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Entries) != 2 {
			t.Fatalf("Entries = %d", len(u.Entries))
		}
		if u.Entries[0].Priority != "medium" {
			t.Errorf("Entries[0].Priority = %q", u.Entries[0].Priority)
		}
		if u.Entries[0].Status != oc.PlanStatusCompleted {
			t.Errorf("Entries[0].Status = %q", u.Entries[0].Status)
		}
		if u.Entries[1].Status != oc.PlanStatusPending {
			t.Errorf("Entries[1].Status = %q", u.Entries[1].Status)
		}
	})
	t.Run("CancelledStatus", func(t *testing.T) {
		const input = `{"sessionUpdate":"plan","entries":[{"status":"cancelled","content":"dropped"}]}`
		var u oc.PlanUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Entries[0].Status != oc.PlanStatusCancelled {
			t.Errorf("Status = %q, want cancelled", u.Entries[0].Status)
		}
	})
}

func TestUsageUpdateUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"usage_update","used":50000,"size":200000,"cost":{"amount":0.42,"currency":"USD"}}`
		var u oc.UsageUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Used != 50000 {
			t.Errorf("Used = %d", u.Used)
		}
		if u.Size != 200000 {
			t.Errorf("Size = %d", u.Size)
		}
		if u.Cost.Amount != 0.42 {
			t.Errorf("Cost.Amount = %f", u.Cost.Amount)
		}
		if u.Cost.Currency != "USD" {
			t.Errorf("Cost.Currency = %q", u.Cost.Currency)
		}
	})
}

func TestContentBlock(t *testing.T) {
	t.Run("Text", func(t *testing.T) {
		const input = `{"type":"text","text":"hello"}`
		var b oc.ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "text" || b.Text != "hello" {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("Image", func(t *testing.T) {
		const input = `{"type":"image","data":"aGVsbG8=","mimeType":"image/png","uri":"file:///tmp/img.png"}`
		var b oc.ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "image" || b.Data != "aGVsbG8=" || b.MimeType != "image/png" || b.URI != "file:///tmp/img.png" {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("ResourceLink", func(t *testing.T) {
		const input = `{"type":"resource_link","uri":"file:///tmp/a.go","name":"a.go","mimeType":"text/x-go"}`
		var b oc.ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "resource_link" || b.URI != "file:///tmp/a.go" || b.Name != "a.go" {
			t.Errorf("block = %+v", b)
		}
	})
}

func TestInitializeResult(t *testing.T) {
	t.Run("WithCapabilities", func(t *testing.T) {
		const input = `{"protocolVersion":1,"AgentCapabilities":{"PromptCapabilities":{"image":true,"embeddedContext":true},"loadSession":true},"AgentInfo":{"name":"opencode","version":"0.5.0"}}`
		var r oc.InitializeResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if r.ProtocolVersion != 1 {
			t.Errorf("ProtocolVersion = %d", r.ProtocolVersion)
		}
		if !r.AgentCapabilities.PromptCapabilities.Image {
			t.Error("PromptCapabilities.Image = false")
		}
		if !r.AgentCapabilities.PromptCapabilities.EmbeddedContext {
			t.Error("PromptCapabilities.EmbeddedContext = false")
		}
		if !r.AgentCapabilities.LoadSession {
			t.Error("LoadSession = false")
		}
		if r.AgentInfo.Name != "opencode" {
			t.Errorf("AgentInfo.Name = %q", r.AgentInfo.Name)
		}
	})
}

func TestPromptResult(t *testing.T) {
	t.Run("WithUsage", func(t *testing.T) {
		const input = `{"stopReason":"end_turn","usage":{"totalTokens":5000,"inputTokens":3000,"outputTokens":500,"thoughtTokens":200,"cachedReadTokens":100,"cachedWriteTokens":50}}`
		var r oc.PromptResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if r.StopReason != "end_turn" {
			t.Errorf("StopReason = %q", r.StopReason)
		}
		if r.Usage.InputTokens != 3000 {
			t.Errorf("InputTokens = %d", r.Usage.InputTokens)
		}
		if r.Usage.ThoughtTokens != 200 {
			t.Errorf("ThoughtTokens = %d", r.Usage.ThoughtTokens)
		}
		if r.Usage.CachedReadTokens != 100 {
			t.Errorf("CachedReadTokens = %d", r.Usage.CachedReadTokens)
		}
	})
}

func TestPermissionRequestParams(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionId":"s1","toolCall":{"toolCallId":"c1","title":"bash","kind":"execute","rawInput":{"command":"rm -rf /"}},"options":[{"optionId":"o1","kind":"allow_once","name":"Allow"}]}`
		var p oc.PermissionRequestParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.SessionID != "s1" {
			t.Errorf("SessionID = %q", p.SessionID)
		}
		if p.ToolCall.ToolCallID != "c1" {
			t.Errorf("ToolCall.ToolCallID = %q", p.ToolCall.ToolCallID)
		}
		if len(p.Options) != 1 || p.Options[0].Kind != "allow_once" {
			t.Errorf("Options = %+v", p.Options)
		}
	})
}

func TestCurrentModeUpdate(t *testing.T) {
	t.Run("WithFields", func(t *testing.T) {
		const input = `{"sessionUpdate":"current_mode_update","modeId":"code","modeName":"Code Mode"}`
		var u oc.CurrentModeUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.ModeID != "code" {
			t.Errorf("ModeID = %q", u.ModeID)
		}
		if u.ModeName != "Code Mode" {
			t.Errorf("ModeName = %q", u.ModeName)
		}
	})
}

func TestToolCallRawOutput(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"output":"success","error":"","metadata":{"exitCode":0}}`
		var o oc.ToolCallRawOutput
		if err := json.Unmarshal([]byte(input), &o); err != nil {
			t.Fatal(err)
		}
		if o.Output != "success" {
			t.Errorf("Output = %q", o.Output)
		}
	})
}
