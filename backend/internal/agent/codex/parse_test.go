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
		msgs, err := ParseMessage([]byte(input))
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
		if init.SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
			t.Errorf("SessionID = %q", init.SessionID)
		}
		if init.Cwd != "/repo" {
			t.Errorf("Cwd = %q", init.Cwd)
		}
	})
	t.Run("TurnStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`
		msgs, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		sm, ok := msgs[0].(*agent.SystemMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.SystemMessage", msgs[0])
		}
		if sm.Subtype != "turn_started" {
			t.Errorf("Subtype = %q", sm.Subtype)
		}
	})
	t.Run("TurnCompleted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`
		msgs, err := ParseMessage([]byte(input))
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
			t.Error("IsError should be false")
		}
	})
	t.Run("TurnFailed", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"rate limit exceeded"}}}}`
		msgs, err := ParseMessage([]byte(input))
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
			t.Error("IsError should be true")
		}
		if rm.Result != "rate limit exceeded" {
			t.Errorf("Result = %q", rm.Result)
		}
	})
	t.Run("ItemStartedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tu.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", tu.Name)
		}
		if tu.ToolUseID != "item_1" {
			t.Errorf("ToolUseID = %q", tu.ToolUseID)
		}
	})
	t.Run("ItemCompletedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tr.ToolUseID != "item_1" {
			t.Errorf("ToolUseID = %q", tr.ToolUseID)
		}
	})
	t.Run("ItemCompletedAgentMessage", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tm.Text != "Done." {
			t.Errorf("Text = %q", tm.Text)
		}
	})
	t.Run("ItemCompletedReasoning", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tm.Text != "**Scanning...**" {
			t.Errorf("Text = %q", tm.Text)
		}
	})
	t.Run("ItemCompletedFileChangeAdd", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tu.Name != "Write" {
			t.Errorf("Name = %q, want Write (file add)", tu.Name)
		}
	})
	t.Run("ItemCompletedFileChangeUpdate", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tu.Name != "Edit" {
			t.Errorf("Name = %q, want Edit (file update)", tu.Name)
		}
	})
	t.Run("ItemCompletedWebSearch", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_6","type":"webSearch","query":"golang generics","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if tu.Name != "WebSearch" {
			t.Errorf("Name = %q, want WebSearch", tu.Name)
		}
	})
	t.Run("ItemUpdated", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/updated","params":{"item":{"id":"item_1","type":"commandExecution","aggregatedOutput":"partial..."},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := ParseMessage([]byte(input))
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
		if raw.Type() != MethodItemUpdated {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("ItemDelta", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}}`
		msgs, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		td, ok := msgs[0].(*agent.TextDeltaMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.TextDeltaMessage", msgs[0])
		}
		if td.Text != "Hello " {
			t.Errorf("Text = %q, want %q", td.Text, "Hello ")
		}
	})
	t.Run("JSONRPCResponse", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"t1"}}}`
		msgs, err := ParseMessage([]byte(input))
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
		if raw.Type() != "jsonrpc_response" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("DiffStat", func(t *testing.T) {
		const input = `{"type":"caic_diff_stat","diff_stat":[{"path":"foo.go","added":10,"deleted":2}]}`
		msgs, err := ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ds, ok := msgs[0].(*agent.DiffStatMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.DiffStatMessage", msgs[0])
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
		msgs, err := ParseMessage([]byte(input))
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
			"init",        // thread/started → InitMessage
			"system",      // turn/started → SystemMessage
			"text",        // reasoning → TextMessage
			"tool_use",    // item/started commandExecution → ToolUseMessage
			"tool_result", // item/completed commandExecution → ToolResultMessage
			"text_delta",  // item/agentMessage/delta → TextDeltaMessage
			"tool_use",    // fileChange → ToolUseMessage
			"text",        // agentMessage → TextMessage
			"result",      // turn/completed → ResultMessage
		}
		for i, line := range lines {
			msgs, err := ParseMessage([]byte(line))
			if err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			if len(msgs) != 1 {
				t.Fatalf("line %d: msgs = %d, want 1", i, len(msgs))
			}
			if msgs[0].Type() != wantTypes[i] {
				t.Errorf("line %d: Type() = %q, want %q", i, msgs[0].Type(), wantTypes[i])
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
		msgs, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.InitMessage); !ok {
			t.Fatalf("type = %T", msgs[0])
		}
		if w.threadID != "captured-id" {
			t.Errorf("threadID = %q, want captured-id", w.threadID)
		}
	})
	t.Run("TokenUsageUpdatedStoresUsage", func(t *testing.T) {
		w := &wireFormat{}
		const input = `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"outputTokens":200,"reasoningOutputTokens":0},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"outputTokens":20,"reasoningOutputTokens":0}}}}`
		msgs, err := w.ParseMessage([]byte(input))
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
		msgs, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T", msgs[0])
		}
		if rm.Usage.InputTokens != 42 {
			t.Errorf("Usage.InputTokens = %d, want 42", rm.Usage.InputTokens)
		}
		if rm.Usage.OutputTokens != 7 {
			t.Errorf("Usage.OutputTokens = %d, want 7", rm.Usage.OutputTokens)
		}
	})
}
