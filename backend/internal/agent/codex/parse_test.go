package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/maruel/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Run("ThreadStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}}`
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
	})
	t.Run("TurnStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/started","params":{}}`
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
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}}}`
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
		if rm.Usage.InputTokens != 24763 {
			t.Errorf("InputTokens = %d", rm.Usage.InputTokens)
		}
		if rm.Usage.OutputTokens != 122 {
			t.Errorf("OutputTokens = %d", rm.Usage.OutputTokens)
		}
		if rm.Usage.CacheReadInputTokens != 24448 {
			t.Errorf("CacheReadInputTokens = %d", rm.Usage.CacheReadInputTokens)
		}
	})
	t.Run("TurnFailed", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"failed","error":"rate limit exceeded","usage":{}}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsrc\n","exit_code":0,"status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agent_message","text":"Done.","status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","text":"**Scanning...**","status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"file_change","changes":[{"path":"docs/foo.md","kind":"add"}],"status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_5","type":"file_change","changes":[{"path":"src/main.go","kind":"update"}],"status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_6","type":"web_search","query":"golang generics","status":"completed"}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/updated","params":{"item":{"id":"item_1","type":"command_execution","aggregated_output":"partial..."}}}`
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
		const input = `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"item_id":"item_3","delta":"Hello "}}`
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
		// Parse a full example stream of JSON-RPC notifications.
		lines := []string{
			`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}}`,
			`{"jsonrpc":"2.0","method":"turn/started","params":{}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","text":"**Scanning...**","status":"completed"}}}`,
			`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsrc\n","exit_code":0,"status":"completed"}}}`,
			`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"item_id":"item_3","delta":"Done."}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"file_change","changes":[{"path":"docs/foo.md","kind":"add"}],"status":"completed"}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agent_message","text":"Done.","status":"completed"}}}`,
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}}}`,
		}
		wantTypes := []string{
			"system",       // thread/started → SystemInitMessage
			"system",       // turn/started → SystemMessage
			"assistant",    // reasoning → AssistantMessage
			"assistant",    // item/started command_execution → AssistantMessage (tool_use)
			"user",         // item/completed command_execution → UserMessage (tool result)
			"stream_event", // item/agentMessage/delta → StreamEvent
			"assistant",    // file_change → AssistantMessage (tool_use)
			"assistant",    // agent_message → AssistantMessage
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
		args := buildArgs(&agent.Options{Prompt: "fix the bug", Model: "o4-mini"})
		if len(args) != 2 || args[0] != "codex" || args[1] != "app-server" {
			t.Errorf("args = %v, want [codex app-server]", args)
		}
	})
}

func TestWireFormat(t *testing.T) {
	t.Run("WritePromptBasic", func(t *testing.T) {
		w := &wireFormat{threadID: "t1"}
		var buf bytes.Buffer
		if err := w.WritePrompt(&buf, "fix the bug", nil, nil); err != nil {
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
		if params["thread_id"] != "t1" {
			t.Errorf("thread_id = %v", params["thread_id"])
		}
		if params["input"] != "fix the bug" {
			t.Errorf("input = %v", params["input"])
		}
	})
	t.Run("WritePromptNoThreadID", func(t *testing.T) {
		w := &wireFormat{}
		var buf bytes.Buffer
		err := w.WritePrompt(&buf, "hello", nil, nil)
		if err == nil {
			t.Fatal("expected error for missing thread ID")
		}
	})
	t.Run("ParseMessageCapturesThreadID", func(t *testing.T) {
		w := &wireFormat{}
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"captured-id"}}}`
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
}
