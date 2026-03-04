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
	t.Run("KnownThreadInfoFields", func(t *testing.T) {
		const input = `{"thread":{"id":"t1","cliVersion":"0.1.0","createdAt":1771690198,"cwd":"/repo","ephemeral":false,"gitInfo":{"branch":"main"},"modelProvider":"openai","path":"/repo","preview":"fix the bug","source":"user","status":{"type":"idle"},"turns":[],"updatedAt":1771690200}}`
		var p ThreadStartedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Thread.ID != "t1" {
			t.Errorf("Thread.ID = %q", p.Thread.ID)
		}
		if p.Thread.Status.Type != "idle" {
			t.Errorf("Thread.Status.Type = %q, want idle", p.Thread.Status.Type)
		}
		if len(p.Thread.Extra) != 0 {
			t.Errorf("unexpected extra fields in ThreadInfo: %v", p.Thread.Extra)
		}
	})
	t.Run("ThreadStatusActive", func(t *testing.T) {
		const input = `{"thread":{"id":"t1","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}`
		var p ThreadStartedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Thread.Status.Type != "active" {
			t.Errorf("Thread.Status.Type = %q, want active", p.Thread.Status.Type)
		}
		if len(p.Thread.Status.ActiveFlags) != 1 || p.Thread.Status.ActiveFlags[0] != "waitingOnApproval" {
			t.Errorf("Thread.Status.ActiveFlags = %v", p.Thread.Status.ActiveFlags)
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
		const input = `{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}`
		var p TurnCompletedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.ThreadID != "t1" {
			t.Errorf("ThreadID = %q", p.ThreadID)
		}
		if p.Turn.ID != "turn_1" {
			t.Errorf("Turn.ID = %q", p.Turn.ID)
		}
		if p.Turn.Status != "completed" {
			t.Errorf("Status = %q", p.Turn.Status)
		}
		if p.Turn.Error != nil {
			t.Errorf("Error = %v, want nil", p.Turn.Error)
		}
	})
	t.Run("Failed", func(t *testing.T) {
		const input = `{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"something went wrong"}}}`
		var p TurnCompletedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Turn.Status != "failed" {
			t.Errorf("Status = %q", p.Turn.Status)
		}
		if p.Turn.Error == nil {
			t.Fatal("Error = nil, want non-nil")
		}
		if p.Turn.Error.Message != "something went wrong" {
			t.Errorf("Error.Message = %q", p.Turn.Error.Message)
		}
	})
}

func TestItemParams(t *testing.T) {
	t.Run("RawItem", func(t *testing.T) {
		const input = `{"item":{"id":"item_1","type":"commandExecution","command":"ls"},"threadId":"t1","turnId":"turn_1"}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.ThreadID != "t1" {
			t.Errorf("ThreadID = %q", p.ThreadID)
		}
		if p.TurnID != "turn_1" {
			t.Errorf("TurnID = %q", p.TurnID)
		}
		if len(p.Item) == 0 {
			t.Fatal("Item is empty")
		}
		var h ItemHeader
		if err := json.Unmarshal(p.Item, &h); err != nil {
			t.Fatalf("unmarshal ItemHeader from raw: %v", err)
		}
		if h.ID != "item_1" {
			t.Errorf("ItemHeader.ID = %q", h.ID)
		}
		if h.Type != ItemTypeCommandExecution {
			t.Errorf("ItemHeader.Type = %q", h.Type)
		}
	})
	t.Run("UnknownFields", func(t *testing.T) {
		const input = `{"item":{"id":"x","type":"agentMessage"},"threadId":"t1","turnId":"turn_1","surprise":true}`
		var p ItemParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 unknown field", p.Extra)
		}
		if _, ok := p.Extra["surprise"]; !ok {
			t.Error("expected 'surprise' in Extra")
		}
	})
}

func TestItemDeltaParams(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}`
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
		if p.ThreadID != "t1" {
			t.Errorf("ThreadID = %q", p.ThreadID)
		}
		if p.TurnID != "turn_1" {
			t.Errorf("TurnID = %q", p.TurnID)
		}
	})
	t.Run("UnknownFields", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"x","delta":"y","new_field":42}`
		var p ItemDeltaParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 unknown field", p.Extra)
		}
	})
}

func TestPerItemTypeStructs(t *testing.T) {
	t.Run("AgentMessage", func(t *testing.T) {
		const input = `{"id":"item_3","type":"agentMessage","text":"Done.","phase":"response","status":"completed"}`
		var item AgentMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != "item_3" {
			t.Errorf("ID = %q", item.ID)
		}
		if item.Type != ItemTypeAgentMessage {
			t.Errorf("Type = %q", item.Type)
		}
		if item.Text != "Done." {
			t.Errorf("Text = %q", item.Text)
		}
		if item.Phase != "response" {
			t.Errorf("Phase = %q", item.Phase)
		}
		if item.Status != "completed" {
			t.Errorf("Status = %q", item.Status)
		}
		if len(item.Extra) != 0 {
			t.Errorf("unexpected extra: %v", item.Extra)
		}
	})
	t.Run("AgentMessageUnknown", func(t *testing.T) {
		const input = `{"id":"x","type":"agentMessage","text":"hi","query":"oops"}`
		var item AgentMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if len(item.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 field", item.Extra)
		}
		if _, ok := item.Extra["query"]; !ok {
			t.Error("expected 'query' in Extra")
		}
	})
	t.Run("Plan", func(t *testing.T) {
		const input = `{"id":"p1","type":"plan","text":"Step 1: read code","status":"completed"}`
		var item PlanItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Text != "Step 1: read code" {
			t.Errorf("Text = %q", item.Text)
		}
	})
	t.Run("Reasoning", func(t *testing.T) {
		const input = `{"id":"r1","type":"reasoning","summary":["**Scanning...**"],"content":[],"status":"completed"}`
		var item ReasoningItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if len(item.Summary) != 1 || item.Summary[0] != "**Scanning...**" {
			t.Errorf("Summary = %v", item.Summary)
		}
	})
	t.Run("CommandExecution", func(t *testing.T) {
		const input = `{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed","cwd":"/repo","durationMs":150}`
		var item CommandExecutionItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Command != "bash -lc ls" {
			t.Errorf("Command = %q", item.Command)
		}
		if item.AggregatedOutput == nil || *item.AggregatedOutput != "docs\nsrc\n" {
			t.Errorf("AggregatedOutput = %v", item.AggregatedOutput)
		}
		if item.ExitCode == nil || *item.ExitCode != 0 {
			t.Errorf("ExitCode = %v", item.ExitCode)
		}
		if item.Cwd != "/repo" {
			t.Errorf("Cwd = %q", item.Cwd)
		}
		if item.DurationMs == nil || *item.DurationMs != 150 {
			t.Errorf("DurationMs = %v", item.DurationMs)
		}
		if len(item.Extra) != 0 {
			t.Errorf("unexpected extra: %v", item.Extra)
		}
	})
	t.Run("CommandExecutionUnknown", func(t *testing.T) {
		const input = `{"id":"x","type":"commandExecution","command":"ls","text":"wrong field"}`
		var item CommandExecutionItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if len(item.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 field", item.Extra)
		}
		if _, ok := item.Extra["text"]; !ok {
			t.Error("expected 'text' in Extra")
		}
	})
	t.Run("FileChange", func(t *testing.T) {
		const input = `{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"}`
		var item FileChangeItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if len(item.Changes) != 1 {
			t.Fatalf("Changes = %d, want 1", len(item.Changes))
		}
		if item.Changes[0].Path != "docs/foo.md" {
			t.Errorf("Path = %q", item.Changes[0].Path)
		}
		if item.Changes[0].Kind.Type != "add" {
			t.Errorf("Kind.Type = %q", item.Changes[0].Kind.Type)
		}
	})
	t.Run("McpToolCall", func(t *testing.T) {
		const input = `{"id":"m1","type":"mcpToolCall","server":"fs","tool":"read_file","status":"completed","arguments":{"path":"/tmp/a"},"durationMs":42}`
		var item McpToolCallItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Server != "fs" {
			t.Errorf("Server = %q", item.Server)
		}
		if item.Tool != "read_file" {
			t.Errorf("Tool = %q", item.Tool)
		}
		if item.Arguments == nil {
			t.Fatal("Arguments = nil")
		}
	})
	t.Run("McpToolCallError", func(t *testing.T) {
		const input = `{"id":"m2","type":"mcpToolCall","server":"fs","tool":"read_file","status":"failed","error":{"message":"not found"}}`
		var item McpToolCallItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Error == nil {
			t.Fatal("Error = nil")
		}
		if item.Error.Message != "not found" {
			t.Errorf("Error.Message = %q", item.Error.Message)
		}
	})
	t.Run("WebSearch", func(t *testing.T) {
		const input = `{"id":"w1","type":"webSearch","query":"golang generics","action":"search","status":"completed"}`
		var item WebSearchItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Query != "golang generics" {
			t.Errorf("Query = %q", item.Query)
		}
		if item.Action != "search" {
			t.Errorf("Action = %q", item.Action)
		}
	})
	t.Run("ImageView", func(t *testing.T) {
		const input = `{"id":"i1","type":"imageView","path":"/tmp/img.png","status":"completed"}`
		var item ImageViewItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Path != "/tmp/img.png" {
			t.Errorf("Path = %q", item.Path)
		}
	})
	t.Run("ContextCompaction", func(t *testing.T) {
		const input = `{"id":"cc1","type":"contextCompaction"}`
		var item ContextCompactionItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != "cc1" {
			t.Errorf("ID = %q", item.ID)
		}
		if len(item.Extra) != 0 {
			t.Errorf("unexpected extra: %v", item.Extra)
		}
	})
	t.Run("UserMessage", func(t *testing.T) {
		const input = `{"id":"u1","type":"userMessage","content":[{"type":"text","text":"hello"}],"status":"completed"}`
		var item UserMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Content == nil {
			t.Fatal("Content = nil")
		}
	})
	t.Run("DynamicToolCall", func(t *testing.T) {
		const input = `{"id":"d1","type":"dynamicToolCall","tool":"my_tool","arguments":{"a":1},"status":"completed","success":true,"durationMs":100}`
		var item DynamicToolCallItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Tool != "my_tool" {
			t.Errorf("Tool = %q", item.Tool)
		}
		if item.Success == nil || !*item.Success {
			t.Errorf("Success = %v", item.Success)
		}
	})
	t.Run("CollabAgentToolCall", func(t *testing.T) {
		const input = `{"id":"ca1","type":"collabAgentToolCall","tool":"delegate","status":"completed","senderThreadId":"st1","prompt":"do this"}`
		var item CollabAgentToolCallItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.SenderThreadID != "st1" {
			t.Errorf("SenderThreadID = %q", item.SenderThreadID)
		}
		if item.Prompt != "do this" {
			t.Errorf("Prompt = %q", item.Prompt)
		}
	})
	t.Run("EnteredReviewMode", func(t *testing.T) {
		const input = `{"id":"er1","type":"enteredReviewMode","review":{"state":"pending"}}`
		var item EnteredReviewModeItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Review == nil {
			t.Fatal("Review = nil")
		}
	})
	t.Run("ExitedReviewMode", func(t *testing.T) {
		const input = `{"id":"xr1","type":"exitedReviewMode","review":{"state":"approved"}}`
		var item ExitedReviewModeItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Review == nil {
			t.Fatal("Review = nil")
		}
	})
}

func TestDeltaNotificationParams(t *testing.T) {
	t.Run("CommandOutputDelta", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","delta":"output line\n"}`
		var p CommandOutputDeltaParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Delta != "output line\n" {
			t.Errorf("Delta = %q", p.Delta)
		}
	})
	t.Run("TerminalInteraction", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","processId":"p1","stdin":"yes\n"}`
		var p TerminalInteractionParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.ProcessID != "p1" {
			t.Errorf("ProcessID = %q", p.ProcessID)
		}
		if p.Stdin != "yes\n" {
			t.Errorf("Stdin = %q", p.Stdin)
		}
	})
	t.Run("ReasoningSummaryTextDelta", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","delta":"thinking...","summaryIndex":0}`
		var p ReasoningSummaryTextDeltaParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.SummaryIndex != 0 {
			t.Errorf("SummaryIndex = %d", p.SummaryIndex)
		}
	})
	t.Run("McpToolCallProgress", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","message":"processing..."}`
		var p McpToolCallProgressParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Message != "processing..." {
			t.Errorf("Message = %q", p.Message)
		}
	})
	t.Run("ThreadStatusChanged", func(t *testing.T) {
		const input = `{"threadId":"t1","status":{"type":"idle"}}`
		var p ThreadStatusChangedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Status.Type != "idle" {
			t.Errorf("Status.Type = %q, want idle", p.Status.Type)
		}
	})
	t.Run("ModelRerouted", func(t *testing.T) {
		const input = `{"threadId":"t1","turnId":"turn_1","fromModel":"gpt-4","toModel":"gpt-3.5","reason":"rate limit"}`
		var p ModelReroutedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.FromModel != "gpt-4" {
			t.Errorf("FromModel = %q", p.FromModel)
		}
		if p.ToModel != "gpt-3.5" {
			t.Errorf("ToModel = %q", p.ToModel)
		}
	})
	t.Run("ErrorNotification", func(t *testing.T) {
		const input = `{"error":{"message":"rate limit"},"willRetry":true,"threadId":"t1","turnId":"turn_1"}`
		var p ErrorNotificationParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Error == nil || p.Error.Message != "rate limit" {
			t.Errorf("Error = %v", p.Error)
		}
		if !p.WillRetry {
			t.Error("WillRetry = false")
		}
	})
	t.Run("UnknownField", func(t *testing.T) {
		const input = `{"threadId":"t1","status":{"type":"idle"},"surprise":1}`
		var p ThreadStatusChangedParams
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Extra) != 1 {
			t.Fatalf("Extra = %v, want 1 field", p.Extra)
		}
	})
}
