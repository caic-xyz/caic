package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Run("ThreadStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","cliVersion":"1.0","createdAt":1771690198,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"fix","source":"user","updatedAt":1771690200}}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		init, ok := msg.(*agent.SystemInitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.SystemInitMessage", msg)
		}
		if init.SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
			t.Errorf("SessionID = %q", init.SessionID)
		}
		if init.Cwd != "/repo" {
			t.Errorf("Cwd = %q", init.Cwd)
		}
	})
	t.Run("TurnStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		sm, ok := msg.(*agent.SystemMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.SystemMessage", msg)
		}
		if sm.Subtype != "turn_started" {
			t.Errorf("Subtype = %q", sm.Subtype)
		}
	})
	t.Run("TurnCompleted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msg.(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msg)
		}
		if rm.IsError {
			t.Error("IsError should be false")
		}
	})
	t.Run("TurnFailed", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"rate limit exceeded"}}}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msg.(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msg)
		}
		if !rm.IsError {
			t.Error("IsError should be true")
		}
		if rm.Result != "rate limit exceeded" {
			t.Errorf("Result = %q", rm.Result)
		}
	})
	t.Run("ItemStartedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
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
		if cb.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", cb.Name)
		}
		if cb.ID != "item_1" {
			t.Errorf("ID = %q", cb.ID)
		}
	})
	t.Run("ItemCompletedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		um, ok := msg.(*agent.UserMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserMessage", msg)
		}
		if um.ParentToolUseID == nil || *um.ParentToolUseID != "item_1" {
			t.Errorf("ParentToolUseID = %v", um.ParentToolUseID)
		}
	})
	t.Run("ItemCompletedAgentMessage", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if am.Message.Content[0].Text != "Done." {
			t.Errorf("text = %q", am.Message.Content[0].Text)
		}
	})
	t.Run("ItemCompletedReasoning", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		if am.Message.Content[0].Type != "text" {
			t.Errorf("content type = %q", am.Message.Content[0].Type)
		}
		if am.Message.Content[0].Text != "**Scanning...**" {
			t.Errorf("text = %q", am.Message.Content[0].Text)
		}
	})
	t.Run("ItemCompletedFileChangeAdd", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		cb := am.Message.Content[0]
		if cb.Name != "Write" {
			t.Errorf("Name = %q, want Write (file add)", cb.Name)
		}
	})
	t.Run("ItemCompletedFileChangeUpdate", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		cb := am.Message.Content[0]
		if cb.Name != "Edit" {
			t.Errorf("Name = %q, want Edit (file update)", cb.Name)
		}
	})
	t.Run("ItemCompletedWebSearch", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_6","type":"webSearch","query":"golang generics","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		am, ok := msg.(*agent.AssistantMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.AssistantMessage", msg)
		}
		cb := am.Message.Content[0]
		if cb.Name != "WebSearch" {
			t.Errorf("Name = %q, want WebSearch", cb.Name)
		}
	})
	t.Run("ItemUpdated", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/updated","params":{"item":{"id":"item_1","type":"commandExecution","aggregatedOutput":"partial..."},"threadId":"t1","turnId":"turn_1"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := msg.(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msg)
		}
		if raw.Type() != MethodItemUpdated {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("ItemDelta", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		se, ok := msg.(*agent.StreamEvent)
		if !ok {
			t.Fatalf("type = %T, want *agent.StreamEvent", msg)
		}
		if se.Event.Type != "content_block_delta" {
			t.Errorf("Event.Type = %q", se.Event.Type)
		}
		if se.Event.Delta == nil || se.Event.Delta.Text != "Hello " {
			t.Errorf("Delta.Text = %v", se.Event.Delta)
		}
	})
	t.Run("JSONRPCResponse", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"t1"}}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := msg.(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msg)
		}
		if raw.Type() != "jsonrpc_response" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("DiffStat", func(t *testing.T) {
		const input = `{"type":"caic_diff_stat","diff_stat":[{"path":"foo.go","added":10,"deleted":2}]}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		ds, ok := msg.(*agent.DiffStatMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.DiffStatMessage", msg)
		}
		if len(ds.DiffStat) != 1 {
			t.Fatalf("DiffStat = %d, want 1", len(ds.DiffStat))
		}
		if ds.DiffStat[0].Path != "foo.go" {
			t.Errorf("Path = %q", ds.DiffStat[0].Path)
		}
	})
	t.Run("UnknownMethod", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"future/event","params":{"data":"something"}}`
		msg, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := msg.(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msg)
		}
		if raw.Type() != "future/event" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("FullStream", func(t *testing.T) {
		// Parse a full example stream of JSON-RPC notifications in v2 format.
		lines := []string{
			`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","cliVersion":"1.0","createdAt":1771690198,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"fix","source":"user","updatedAt":1771690200}}}`,
			`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Done."}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`,
		}
		wantTypes := []string{
			"system",       // thread/started → SystemInitMessage
			"system",       // turn/started → SystemMessage
			"assistant",    // reasoning → AssistantMessage
			"assistant",    // item/started commandExecution → AssistantMessage (tool_use)
			"user",         // item/completed commandExecution → UserMessage (tool result)
			"stream_event", // item/agentMessage/delta → StreamEvent
			"assistant",    // fileChange → AssistantMessage (tool_use)
			"assistant",    // agentMessage → AssistantMessage
			"result",       // turn/completed → ResultMessage
		}
		for i, line := range lines {
			msg, err := ParseMessage([]byte(line))
			if err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			if msg.Type() != wantTypes[i] {
				t.Errorf("line %d: Type() = %q, want %q", i, msg.Type(), wantTypes[i])
			}
		}
	})
}

func TestBuildArgs(t *testing.T) {
	t.Run("AppServer", func(t *testing.T) {
		args := buildArgs(&agent.Options{InitialPrompt: agent.Prompt{Text: "fix the bug"}, Model: "o4-mini"})
		if len(args) != 2 || args[0] != "codex" || args[1] != "app-server" {
			t.Errorf("args = %v, want [codex app-server]", args)
		}
	})
}

func TestWireFormat(t *testing.T) {
	t.Run("WritePromptBasic", func(t *testing.T) {
		w := &wireFormat{threadID: "t1"}
		var buf bytes.Buffer
		if err := w.WritePrompt(&buf, agent.Prompt{Text: "fix the bug"}, nil); err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
			t.Fatal(err)
		}
		if req["method"] != "turn/start" {
			t.Errorf("method = %v", req["method"])
		}
		params, ok := req["params"].(map[string]any)
		if !ok {
			t.Fatal("params not a map")
		}
		if params["threadId"] != "t1" {
			t.Errorf("threadId = %v", params["threadId"])
		}
		input, ok := params["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input = %v, want a 1-element array", params["input"])
		}
		elem, ok := input[0].(map[string]any)
		if !ok {
			t.Fatalf("input[0] = %T, want map", input[0])
		}
		if elem["type"] != "text" {
			t.Errorf("input[0].type = %v, want text", elem["type"])
		}
		if elem["text"] != "fix the bug" {
			t.Errorf("input[0].text = %v", elem["text"])
		}
	})
	t.Run("WritePromptNoThreadID", func(t *testing.T) {
		w := &wireFormat{}
		var buf bytes.Buffer
		err := w.WritePrompt(&buf, agent.Prompt{Text: "hello"}, nil)
		if err == nil {
			t.Fatal("expected error for missing thread ID")
		}
	})
	t.Run("ParseMessageCapturesThreadID", func(t *testing.T) {
		w := &wireFormat{}
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"captured-id","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","updatedAt":2}}}`
		msg, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := msg.(*agent.SystemInitMessage); !ok {
			t.Fatalf("type = %T", msg)
		}
		if w.threadID != "captured-id" {
			t.Errorf("threadID = %q, want captured-id", w.threadID)
		}
	})
	t.Run("TokenUsageUpdatedStoresUsage", func(t *testing.T) {
		w := &wireFormat{}
		const input = `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"outputTokens":200,"reasoningOutputTokens":0},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"outputTokens":20,"reasoningOutputTokens":0}}}}`
		msg, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := msg.(*agent.RawMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RawMessage", msg)
		}
		if raw.Type() != MethodTokenUsageUpdated {
			t.Errorf("Type() = %q", raw.Type())
		}
		w.mu.Lock()
		usage := w.lastUsage
		w.mu.Unlock()
		if usage.InputTokens != 80 {
			t.Errorf("InputTokens = %d, want 80", usage.InputTokens)
		}
		if usage.OutputTokens != 20 {
			t.Errorf("OutputTokens = %d, want 20", usage.OutputTokens)
		}
		if usage.CacheReadInputTokens != 50 {
			t.Errorf("CacheReadInputTokens = %d, want 50", usage.CacheReadInputTokens)
		}
	})
	t.Run("TurnCompletedInjectsUsage", func(t *testing.T) {
		w := &wireFormat{lastUsage: agent.Usage{InputTokens: 42, OutputTokens: 7}}
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`
		msg, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := msg.(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T", msg)
		}
		if rm.Usage.InputTokens != 42 {
			t.Errorf("Usage.InputTokens = %d, want 42", rm.Usage.InputTokens)
		}
		if rm.Usage.OutputTokens != 7 {
			t.Errorf("Usage.OutputTokens = %d, want 7", rm.Usage.OutputTokens)
		}
	})
}
