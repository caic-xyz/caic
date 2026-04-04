package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

func TestParseMessage(t *testing.T) {
	t.Run("ThreadStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","cliVersion":"1.0","createdAt":1771690198,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"fix","source":"user","status":{"type":"idle"},"updatedAt":1771690200}}}`
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
		if init.SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
			t.Errorf("SessionID = %q", init.SessionID)
		}
		if init.Cwd != "/repo" {
			t.Errorf("Cwd = %q", init.Cwd)
		}
		if init.Version != "1.0" {
			t.Errorf("Version = %q, want 1.0", init.Version)
		}
	})
	t.Run("TurnStarted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (turn/started is suppressed)", len(msgs))
		}
	})
	t.Run("TurnCompleted", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`
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
			t.Error("IsError should be false")
		}
	})
	t.Run("TurnFailed", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"rate limit exceeded"}}}}`
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
			t.Error("IsError should be true")
		}
		if rm.Result != "rate limit exceeded" {
			t.Errorf("Result = %q", rm.Result)
		}
	})
	t.Run("ItemStartedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","cwd":"/repo","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", tu.Name)
		}
		if tu.ToolUseID != "item_1" {
			t.Errorf("ToolUseID = %q", tu.ToolUseID)
		}
		var toolInput map[string]string
		if err := json.Unmarshal(tu.Input, &toolInput); err != nil {
			t.Fatalf("unmarshal Input: %v", err)
		}
		if toolInput["command"] != "bash -lc ls" {
			t.Errorf("Input[command] = %q, want %q", toolInput["command"], "bash -lc ls")
		}
		if toolInput["cwd"] != "/repo" {
			t.Errorf("Input[cwd] = %q, want %q", toolInput["cwd"], "/repo")
		}
	})
	t.Run("ItemCompletedCommandExecution", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tr.ToolUseID != "item_1" {
			t.Errorf("ToolUseID = %q", tr.ToolUseID)
		}
	})
	t.Run("ItemCompletedAgentMessage", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tm.Text != "Done." {
			t.Errorf("Text = %q", tm.Text)
		}
	})
	t.Run("ItemCompletedAgentMessagePhase", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Here is my answer.","phase":"final_answer","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tm.Text != "Here is my answer." {
			t.Errorf("Text = %q", tm.Text)
		}
		if tm.Phase != "final_answer" {
			t.Errorf("Phase = %q, want final_answer", tm.Phase)
		}
	})
	t.Run("ItemCompletedReasoning", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tm, ok := msgs[0].(*agent.ThinkingMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ThinkingMessage", msgs[0])
		}
		if tm.Text != "**Scanning...**" {
			t.Errorf("Text = %q", tm.Text)
		}
	})
	t.Run("ItemStartedFileChangeAdd", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.Name != "Write" {
			t.Errorf("Name = %q, want Write (file add)", tu.Name)
		}
		if tu.ToolUseID != "item_4" {
			t.Errorf("ToolUseID = %q, want item_4", tu.ToolUseID)
		}
	})
	t.Run("ItemCompletedFileChangeAdd", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1 (ToolResultMessage only)", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "item_4" {
			t.Errorf("ToolUseID = %q, want item_4", tr.ToolUseID)
		}
	})
	t.Run("ItemStartedFileChangeUpdate", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.Name != "Edit" {
			t.Errorf("Name = %q, want Edit (file update)", tu.Name)
		}
		if tu.ToolUseID != "item_5" {
			t.Errorf("ToolUseID = %q, want item_5", tu.ToolUseID)
		}
	})
	t.Run("ItemCompletedFileChangeUpdate", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1 (ToolResultMessage only)", len(msgs))
		}
		tr, ok := msgs[0].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolResultMessage", msgs[0])
		}
		if tr.ToolUseID != "item_5" {
			t.Errorf("ToolUseID = %q, want item_5", tr.ToolUseID)
		}
	})
	t.Run("ItemStartedDynamicToolCall", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"dyn_1","type":"dynamicToolCall","tool":"my_tool","arguments":{"key":"val"},"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.Name != "my_tool" {
			t.Errorf("Name = %q, want my_tool", tu.Name)
		}
		if tu.ToolUseID != "dyn_1" {
			t.Errorf("ToolUseID = %q, want dyn_1", tu.ToolUseID)
		}
	})
	t.Run("ItemCompletedDynamicToolCallSuccess", func(t *testing.T) {
		success := true
		_ = success // used inline in JSON
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"dyn_1","type":"dynamicToolCall","tool":"my_tool","status":"completed","success":true},"threadId":"t1","turnId":"turn_1"}}`
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
		if tr.ToolUseID != "dyn_1" {
			t.Errorf("ToolUseID = %q, want dyn_1", tr.ToolUseID)
		}
		if tr.Error != "" {
			t.Errorf("Error = %q, want empty", tr.Error)
		}
	})
	t.Run("ItemCompletedDynamicToolCallFailure", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"dyn_2","type":"dynamicToolCall","tool":"my_tool","status":"failed","success":false},"threadId":"t1","turnId":"turn_1"}}`
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
		if tr.Error == "" {
			t.Error("Error should be set for failed dynamic tool call")
		}
	})
	t.Run("ItemStartedCollabAgentToolCall", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"collab_1","type":"collabAgentToolCall","tool":"delegate","status":"inProgress","senderThreadId":"thread-1","prompt":"do the thing"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.ToolUseID != "collab_1" {
			t.Errorf("ToolUseID = %q, want collab_1", tu.ToolUseID)
		}
		if tu.Name != "delegate" {
			t.Errorf("Name = %q, want delegate", tu.Name)
		}
		var input2 map[string]string
		if err := json.Unmarshal(tu.Input, &input2); err != nil {
			t.Fatalf("unmarshal Input: %v", err)
		}
		if input2["prompt"] != "do the thing" {
			t.Errorf("Input[prompt] = %q, want %q", input2["prompt"], "do the thing")
		}
	})
	t.Run("ItemStartedCollabAgentToolCallEmptyTool", func(t *testing.T) {
		// When Tool is empty, Name should default to "collabAgent".
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"collab_2","type":"collabAgentToolCall","status":"inProgress","prompt":"hello"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tu.Name != "collabAgent" {
			t.Errorf("Name = %q, want collabAgent", tu.Name)
		}
	})
	t.Run("ItemCompletedCollabAgentToolCallSuccess", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"collab_1","type":"collabAgentToolCall","tool":"delegate","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tr.ToolUseID != "collab_1" {
			t.Errorf("ToolUseID = %q, want collab_1", tr.ToolUseID)
		}
		if tr.Error != "" {
			t.Errorf("Error = %q, want empty", tr.Error)
		}
	})
	t.Run("ItemCompletedCollabAgentToolCallFailed", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"collab_3","type":"collabAgentToolCall","tool":"delegate","status":"failed"},"threadId":"t1","turnId":"turn_1"}}`
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
		if tr.ToolUseID != "collab_3" {
			t.Errorf("ToolUseID = %q, want collab_3", tr.ToolUseID)
		}
		if tr.Error == "" {
			t.Error("Error should be set for failed collab agent tool call")
		}
	})
	t.Run("ItemStartedMcpToolCallWidget", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"w1","type":"mcpToolCall","server":"widget","tool":"show_widget","status":"inProgress","arguments":{"title":"demo_chart","widget_code":"<p>Hello</p>"}},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		wm, ok := msgs[0].(*agent.WidgetMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.WidgetMessage", msgs[0])
		}
		if wm.ToolUseID != "w1" {
			t.Errorf("ToolUseID = %q, want w1", wm.ToolUseID)
		}
		if wm.Title != "demo_chart" {
			t.Errorf("Title = %q, want demo_chart", wm.Title)
		}
		if wm.HTML != "<p>Hello</p>" {
			t.Errorf("HTML = %q, want <p>Hello</p>", wm.HTML)
		}
	})
	t.Run("ItemStartedMcpToolCallNonWidget", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"m1","type":"mcpToolCall","server":"fs","tool":"read_file","status":"inProgress","arguments":{"path":"/tmp/a"}},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ToolUseMessage (non-widget MCP tool)", msgs[0])
		}
		if tu.Name != "read_file" {
			t.Errorf("Name = %q, want read_file", tu.Name)
		}
	})
	t.Run("ItemCompletedContextCompaction", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"cc_1","type":"contextCompaction"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
		if sm.Subtype != "context_compaction" {
			t.Errorf("Subtype = %q, want context_compaction", sm.Subtype)
		}
	})
	t.Run("ItemCompletedWebSearch", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_6","type":"webSearch","query":"golang generics","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("msgs = %d, want 2 (ToolUseMessage + ToolResultMessage)", len(msgs))
		}
		tu, ok := msgs[0].(*agent.ToolUseMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.ToolUseMessage", msgs[0])
		}
		if tu.Name != "WebSearch" {
			t.Errorf("Name = %q, want WebSearch", tu.Name)
		}
		tr, ok := msgs[1].(*agent.ToolResultMessage)
		if !ok {
			t.Fatalf("msgs[1] type = %T, want *agent.ToolResultMessage", msgs[1])
		}
		if tr.ToolUseID != "item_6" {
			t.Errorf("ToolResultMessage.ToolUseID = %q, want item_6", tr.ToolUseID)
		}
	})
	t.Run("ItemUpdated", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/updated","params":{"item":{"id":"item_1","type":"commandExecution","aggregatedOutput":"partial..."},"threadId":"t1","turnId":"turn_1"}}`
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
		if raw.Type() != string(MethodItemUpdated) {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("ReasoningSummaryTextDelta", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/reasoning/summaryTextDelta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_0","delta":"Let me think...","summaryIndex":0}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		td, ok := msgs[0].(*agent.ThinkingDeltaMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ThinkingDeltaMessage", msgs[0])
		}
		if td.Text != "Let me think..." {
			t.Errorf("Text = %q, want %q", td.Text, "Let me think...")
		}
	})

	t.Run("ItemDelta", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
		if raw.Type() != "jsonrpc_response" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("DiffStat", func(t *testing.T) {
		const input = `{"type":"caic_diff_stat","diff_stat":[{"path":"foo.go","added":10,"deleted":2}],"ts":1719500000.5}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
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
	t.Run("ErrorNotificationWillRetry", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"error","params":{"error":{"message":"rate limit"},"willRetry":true,"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseMessage([]byte(input), &jsonutil.FieldWarner{})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (error with willRetry=true is suppressed)", len(msgs))
		}
	})
	t.Run("ErrorNotificationFatal", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"error","params":{"error":{"message":"out of quota"},"willRetry":false,"threadId":"t1","turnId":"turn_1"}}`
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
			t.Error("IsError should be true")
		}
		if rm.Result != "out of quota" {
			t.Errorf("Result = %q, want %q", rm.Result, "out of quota")
		}
	})
	t.Run("UnknownMethod", func(t *testing.T) {
		const input = `{"jsonrpc":"2.0","method":"future/event","params":{"data":"something"}}`
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
		if raw.Type() != "future/event" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("FullStream", func(t *testing.T) {
		// Parse a full example stream of JSON-RPC notifications in v2 format.
		lines := []string{
			`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","cliVersion":"1.0","createdAt":1771690198,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"fix","source":"user","status":{"type":"idle"},"updatedAt":1771690200}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Done."}}`,
			`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`,
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`,
		}
		// Flatten all messages; fileChange: item/started → ToolUseMessage, item/completed → ToolResultMessage.
		wantTypes := []string{
			"init",        // thread/started → InitMessage
			"thinking",    // reasoning → ThinkingMessage
			"tool_use",    // item/started commandExecution → ToolUseMessage
			"tool_result", // item/completed commandExecution → ToolResultMessage
			"text_delta",  // item/agentMessage/delta → TextDeltaMessage
			"tool_use",    // fileChange item/started → ToolUseMessage
			"tool_result", // fileChange item/completed → ToolResultMessage
			"text",        // agentMessage → TextMessage
			"result",      // turn/completed → ResultMessage
		}
		got := make([]agent.Message, 0, len(wantTypes))
		for i, line := range lines {
			msgs, err := parseMessage([]byte(line), &jsonutil.FieldWarner{})
			if err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			got = append(got, msgs...)
		}
		if len(got) != len(wantTypes) {
			t.Fatalf("got %d messages, want %d", len(got), len(wantTypes))
		}
		for i, msg := range got {
			if msg.Type() != wantTypes[i] {
				t.Errorf("msg[%d]: Type() = %q, want %q", i, msg.Type(), wantTypes[i])
			}
		}
	})
}

func TestBuildArgs(t *testing.T) {
	t.Run("AppServer", func(t *testing.T) {
		args := buildArgs(&agent.Options{InitialPrompt: agent.Prompt{Text: "fix the bug"}, Model: "o4-mini"})
		if len(args) < 2 || args[0] != "codex" || args[1] != "app-server" {
			t.Errorf("args[:2] = %v, want [codex app-server ...]", args)
		}
	})
	t.Run("WidgetMCPConfig", func(t *testing.T) {
		// Widget MCP is disabled for codex; buildArgs should return only
		// the base command without any -c flags.
		args := buildArgs(&agent.Options{})
		for _, a := range args {
			if a == "-c" {
				t.Errorf("unexpected -c flag in args %v; widget MCP is disabled for codex", args)
				break
			}
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
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"captured-id","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
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
	t.Run("TokenUsageUpdatedEmitsUsageMessage", func(t *testing.T) {
		w := &wireFormat{}
		const input = `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"outputTokens":200,"reasoningOutputTokens":0},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"outputTokens":20,"reasoningOutputTokens":5}}}}`
		msgs, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		um, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UsageMessage", msgs[0])
		}
		if um.Usage.InputTokens != 80 {
			t.Errorf("InputTokens = %d, want 80", um.Usage.InputTokens)
		}
		if um.Usage.OutputTokens != 20 {
			t.Errorf("OutputTokens = %d, want 20", um.Usage.OutputTokens)
		}
		if um.Usage.CacheReadInputTokens != 50 {
			t.Errorf("CacheReadInputTokens = %d, want 50", um.Usage.CacheReadInputTokens)
		}
		if um.Usage.ReasoningOutputTokens != 5 {
			t.Errorf("ReasoningOutputTokens = %d, want 5", um.Usage.ReasoningOutputTokens)
		}
		// incremental is accumulated into totalUsage
		w.mu.Lock()
		total := w.totalUsage
		w.mu.Unlock()
		if total.InputTokens != 80 {
			t.Errorf("totalUsage.InputTokens = %d, want 80", total.InputTokens)
		}
	})
	t.Run("TokenUsageAccumulates", func(t *testing.T) {
		w := &wireFormat{}
		usage1 := `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":0,"outputTokens":20,"reasoningOutputTokens":0}}}}`
		usage2 := `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{},"last":{"totalTokens":50,"inputTokens":30,"cachedInputTokens":10,"outputTokens":20,"reasoningOutputTokens":0}}}}`
		for _, line := range []string{usage1, usage2} {
			if _, err := w.ParseMessage([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		w.mu.Lock()
		total := w.totalUsage
		w.mu.Unlock()
		if total.InputTokens != 110 {
			t.Errorf("totalUsage.InputTokens = %d, want 110", total.InputTokens)
		}
		if total.OutputTokens != 40 {
			t.Errorf("totalUsage.OutputTokens = %d, want 40", total.OutputTokens)
		}
		if total.CacheReadInputTokens != 10 {
			t.Errorf("totalUsage.CacheReadInputTokens = %d, want 10", total.CacheReadInputTokens)
		}
	})
	t.Run("TurnCompletedInjectsAndResetsUsage", func(t *testing.T) {
		w := &wireFormat{totalUsage: agent.Usage{InputTokens: 42, OutputTokens: 7}}
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
		// totalUsage must be reset for the next turn
		w.mu.Lock()
		reset := w.totalUsage
		w.mu.Unlock()
		if reset != (agent.Usage{}) {
			t.Errorf("totalUsage not reset after ResultMessage: %+v", reset)
		}
	})
}
