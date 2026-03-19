package opencode

import (
	"encoding/json"
	"testing"
)

func TestJSONRPCMessage(t *testing.T) {
	t.Run("Notification", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{}}}`
		var msg JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != MethodSessionUpdate {
			t.Errorf("Method = %q, want %q", msg.Method, MethodSessionUpdate)
		}
		if msg.IsResponse() {
			t.Error("IsResponse() = true, want false for notification")
		}
	})
	t.Run("Response", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1"}}`
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

func TestAgentMessageChunkUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}`
		var u AgentMessageChunkUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Content.Text != "hello" {
			t.Errorf("Content.Text = %q", u.Content.Text)
		}
		if len(u.Extra) != 0 {
			t.Errorf("unexpected extra fields: %v", u.Extra)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"},"new_field":"surprise"}`
		var u AgentMessageChunkUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Content.Text != "hi" {
			t.Errorf("Content.Text = %q", u.Content.Text)
		}
		if len(u.Extra) != 1 {
			t.Errorf("Extra = %v, want 1 entry", u.Extra)
		}
		if _, ok := u.Extra["new_field"]; !ok {
			t.Error("Extra missing new_field")
		}
	})
}

func TestToolCallUpdate(t *testing.T) {
	t.Run("AllKnownFields", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","kind":"execute","status":"pending","locations":[{"path":"/tmp/a.go","line":10}],"rawInput":{"command":"ls"}}`
		var u ToolCallUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.ToolCallID != "c1" {
			t.Errorf("ToolCallID = %q", u.ToolCallID)
		}
		if u.Title != "bash" {
			t.Errorf("Title = %q", u.Title)
		}
		if u.Kind != KindExecute {
			t.Errorf("Kind = %q", u.Kind)
		}
		if len(u.Locations) != 1 || u.Locations[0].Path != "/tmp/a.go" || u.Locations[0].Line != 10 {
			t.Errorf("Locations = %+v", u.Locations)
		}
		if len(u.Extra) != 0 {
			t.Errorf("unexpected extra: %v", u.Extra)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","future_field":42}`
		var u ToolCallUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Extra["future_field"]; !ok {
			t.Error("Extra missing future_field")
		}
	})
}

func TestToolCallUpdateUpdate(t *testing.T) {
	t.Run("CompletedWithContent", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}`
		var u ToolCallUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Status != StatusCompleted {
			t.Errorf("Status = %q", u.Status)
		}
		if len(u.Content) != 1 || u.Content[0].Content.Text != "done" {
			t.Errorf("Content = %+v", u.Content)
		}
		if len(u.Extra) != 0 {
			t.Errorf("unexpected extra: %v", u.Extra)
		}
	})
	t.Run("WithRawOutput", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"failed","rawOutput":{"output":"","error":"permission denied","metadata":{"code":1}}}`
		var u ToolCallUpdateUpdate
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
		var u ToolCallUpdateUpdate
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
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","future":true}`
		var u ToolCallUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Extra["future"]; !ok {
			t.Error("Extra missing future")
		}
	})
}

func TestPlanUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"plan","entries":[{"priority":"medium","status":"completed","content":"step 1"},{"status":"pending","content":"step 2"}]}`
		var u PlanUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Entries) != 2 {
			t.Fatalf("Entries = %d", len(u.Entries))
		}
		if u.Entries[0].Priority != "medium" {
			t.Errorf("Entries[0].Priority = %q", u.Entries[0].Priority)
		}
		if u.Entries[0].Status != PlanStatusCompleted {
			t.Errorf("Entries[0].Status = %q", u.Entries[0].Status)
		}
		if u.Entries[1].Status != PlanStatusPending {
			t.Errorf("Entries[1].Status = %q", u.Entries[1].Status)
		}
	})
	t.Run("CancelledStatus", func(t *testing.T) {
		const input = `{"sessionUpdate":"plan","entries":[{"status":"cancelled","content":"dropped"}]}`
		var u PlanUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if u.Entries[0].Status != PlanStatusCancelled {
			t.Errorf("Status = %q, want cancelled", u.Entries[0].Status)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"plan","entries":[],"extra_flag":true}`
		var u PlanUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Extra["extra_flag"]; !ok {
			t.Error("Extra missing extra_flag")
		}
	})
}

func TestUsageUpdateUpdate(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionUpdate":"usage_update","used":50000,"size":200000,"cost":{"amount":0.42,"currency":"USD"}}`
		var u UsageUpdateUpdate
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
		if len(u.Extra) != 0 {
			t.Errorf("unexpected extra: %v", u.Extra)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"usage_update","used":100,"size":200,"new_metric":99}`
		var u UsageUpdateUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Extra["new_metric"]; !ok {
			t.Error("Extra missing new_metric")
		}
	})
}

func TestContentBlock(t *testing.T) {
	t.Run("Text", func(t *testing.T) {
		const input = `{"type":"text","text":"hello"}`
		var b ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "text" || b.Text != "hello" {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("Image", func(t *testing.T) {
		const input = `{"type":"image","data":"aGVsbG8=","mimeType":"image/png","uri":"file:///tmp/img.png"}`
		var b ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "image" || b.Data != "aGVsbG8=" || b.MimeType != "image/png" || b.URI != "file:///tmp/img.png" {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("ResourceLink", func(t *testing.T) {
		const input = `{"type":"resource_link","uri":"file:///tmp/a.go","name":"a.go","mimeType":"text/x-go"}`
		var b ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if b.Type != "resource_link" || b.URI != "file:///tmp/a.go" || b.Name != "a.go" {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"type":"text","text":"hi","priority":5}`
		var b ContentBlock
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.Extra["priority"]; !ok {
			t.Error("Extra missing priority")
		}
	})
}

func TestInitializeResult(t *testing.T) {
	t.Run("WithCapabilities", func(t *testing.T) {
		const input = `{"protocolVersion":1,"agentCapabilities":{"promptCapabilities":{"image":true,"embeddedContext":true},"loadSession":true},"agentInfo":{"name":"opencode","version":"0.5.0"}}`
		var r initializeResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if r.ProtocolVersion != 1 {
			t.Errorf("ProtocolVersion = %d", r.ProtocolVersion)
		}
		if r.AgentCapabilities.PromptCapabilities == nil {
			t.Fatal("PromptCapabilities = nil")
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
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"protocolVersion":2,"future_cap":"x"}`
		var r initializeResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.Extra["future_cap"]; !ok {
			t.Error("Extra missing future_cap")
		}
	})
}

func TestPromptResult(t *testing.T) {
	t.Run("WithUsage", func(t *testing.T) {
		const input = `{"stopReason":"end_turn","usage":{"totalTokens":5000,"inputTokens":3000,"outputTokens":500,"thoughtTokens":200,"cachedReadTokens":100,"cachedWriteTokens":50}}`
		var r promptResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if r.StopReason != "end_turn" {
			t.Errorf("StopReason = %q", r.StopReason)
		}
		if r.Usage == nil {
			t.Fatal("Usage = nil")
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
		if len(r.Extra) != 0 {
			t.Errorf("unexpected extra: %v", r.Extra)
		}
	})
	t.Run("UsageOverflow", func(t *testing.T) {
		const input = `{"stopReason":"end_turn","usage":{"inputTokens":10,"new_metric":99}}`
		var r promptResult
		if err := json.Unmarshal([]byte(input), &r); err != nil {
			t.Fatal(err)
		}
		if r.Usage == nil {
			t.Fatal("Usage = nil")
		}
		if _, ok := r.Usage.Extra["new_metric"]; !ok {
			t.Error("Usage.Extra missing new_metric")
		}
	})
}

func TestPermissionRequestParams(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"sessionId":"s1","toolCall":{"toolCallId":"c1","title":"bash","kind":"execute","rawInput":{"command":"rm -rf /"}},"options":[{"optionId":"o1","kind":"allow_once","name":"Allow"}]}`
		var p PermissionRequestParams
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
		var u CurrentModeUpdate
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
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"sessionUpdate":"current_mode_update","modeId":"ask","future":true}`
		var u CurrentModeUpdate
		if err := json.Unmarshal([]byte(input), &u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Extra["future"]; !ok {
			t.Error("Extra missing future")
		}
	})
}

func TestToolCallRawOutput(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"output":"success","error":"","metadata":{"exitCode":0}}`
		var o ToolCallRawOutput
		if err := json.Unmarshal([]byte(input), &o); err != nil {
			t.Fatal(err)
		}
		if o.Output != "success" {
			t.Errorf("Output = %q", o.Output)
		}
	})
	t.Run("Overflow", func(t *testing.T) {
		const input = `{"output":"ok","timing_ms":42}`
		var o ToolCallRawOutput
		if err := json.Unmarshal([]byte(input), &o); err != nil {
			t.Fatal(err)
		}
		if _, ok := o.Extra["timing_ms"]; !ok {
			t.Error("Extra missing timing_ms")
		}
	})
}
