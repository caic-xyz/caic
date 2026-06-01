// Tests for Codex CLI wire protocol message types.

package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	cx "github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("empty_without_cache", func(t *testing.T) {
		t.Parallel()
		b := New("", nil)
		if len(b.Models()) != 0 {
			t.Fatalf("Models() = %v, want empty", b.Models())
		}
	})
	t.Run("loads_cached_models", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		envVars := []string{"OPENAI_API_KEY=secret"}
		cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
		cache.SetModels(agent.Codex, []string{"z-model", "a-model"}, agent.APIKeyHash(envVars))

		b := New(dir, envVars)
		if got, want := b.Models(), []string{"z-model", "a-model"}; !slices.Equal(got, want) {
			t.Fatalf("Models() = %v, want %v", got, want)
		}
	})
	t.Run("persists_discovered_models", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		envVars := []string{"OPENAI_API_KEY=secret"}
		b := New(dir, envVars)

		b.setDiscoveredModels([]string{"z-model", "a-model"})

		cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
		got, fresh := cache.Models(agent.Codex, agent.APIKeyHash(envVars))
		if !fresh {
			t.Fatal("cached models are not fresh")
		}
		want := []string{"z-model", "a-model"}
		if !slices.Equal(got, want) {
			t.Fatalf("cached models = %v, want %v", got, want)
		}
	})
}

func TestJSONRPCMessage(t *testing.T) {
	t.Parallel()
	t.Run("Notification", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"t1"}}}`
		var msg cx.JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method != cx.MethodThreadStarted {
			t.Errorf("Method = %q, want %q", msg.Method, cx.MethodThreadStarted)
		}
		if msg.IsResponse() {
			t.Error("IsResponse() = true, want false for notification")
		}
	})
	t.Run("Response", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"t1"}}}`
		var msg cx.JSONRPCMessage
		if err := json.Unmarshal([]byte(input), &msg); err != nil {
			t.Fatal(err)
		}
		if !msg.IsResponse() {
			t.Error("IsResponse() = false, want true for response")
		}
	})
	t.Run("ErrorResponse", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"invalid request"}}`
		var msg cx.JSONRPCMessage
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

func TestFetchModels(t *testing.T) {
	t.Parallel()
	t.Run("protocol", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"},{"id":"gpt-5.3-codex"}],"nextCursor":null}}
`
		var stdin bytes.Buffer
		var nextID atomic.Int64
		models, err := fetchModelsFromAppServer(t.Context(), &stdin, bufio.NewReader(strings.NewReader(responses)), &nextID)
		if err != nil {
			t.Fatal(err)
		}
		wantModels := []string{"gpt-5.4", "gpt-5.3-codex"}
		if !slices.Equal(models, wantModels) {
			t.Errorf("models = %v, want %v", models, wantModels)
		}

		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatalf("wrote %d requests, want 3:\n%s", len(lines), stdin.String())
		}
		var req cx.JSONRPCRequest
		if err := json.Unmarshal(lines[2], &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "model/list" {
			t.Fatalf("method = %q, want model/list", req.Method)
		}
	})
}

func TestHandshake(t *testing.T) {
	t.Parallel()
	t.Run("model_list_params", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"},{"id":"gpt-5.3-codex"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1"}}}
`
		var stdin bytes.Buffer
		w, models, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(responses)), &agent.Options{Dir: "/repo", Model: "gpt-5.4"})
		if err != nil {
			t.Fatal(err)
		}
		if w.threadID != "thread_1" {
			t.Errorf("threadID = %q, want thread_1", w.threadID)
		}
		wantModels := []string{"gpt-5.4", "gpt-5.3-codex"}
		if !slices.Equal(models, wantModels) {
			t.Errorf("models = %v, want %v", models, wantModels)
		}

		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if len(lines) != 4 {
			t.Fatalf("wrote %d requests, want 4:\n%s", len(lines), stdin.String())
		}
		var req cx.JSONRPCRequest
		if err := json.Unmarshal(lines[2], &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "model/list" {
			t.Fatalf("method = %q, want model/list", req.Method)
		}
		if req.Params == nil {
			t.Fatal("model/list params = nil, want empty object")
		}
		data, err := json.Marshal(req.Params)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "{}" {
			t.Fatalf("model/list params = %s, want {}", data)
		}
	})
}

func TestThreadStartedNotification(t *testing.T) {
	t.Parallel()
	t.Run("Basic", func(t *testing.T) {
		t.Parallel()
		const input = `{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}`
		var p cx.ThreadStartedNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Thread.ID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
			t.Errorf("Thread.ID = %q", p.Thread.ID)
		}
	})
	t.Run("KnownThreadFields", func(t *testing.T) {
		t.Parallel()
		const input = `{"thread":{"id":"t1","cliVersion":"0.1.0","createdAt":1771690198,"cwd":"/repo","ephemeral":false,"gitInfo":{"branch":"main"},"modelProvider":"openai","path":"/repo","preview":"fix the bug","source":"user","status":{"type":"idle"},"turns":[],"updatedAt":1771690200}}`
		var p cx.ThreadStartedNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Thread.ID != "t1" {
			t.Errorf("Thread.ID = %q", p.Thread.ID)
		}
		if p.Thread.Status.Type != "idle" {
			t.Errorf("Thread.Status.Type = %q, want idle", p.Thread.Status.Type)
		}
	})
	t.Run("ThreadStatusActive", func(t *testing.T) {
		t.Parallel()
		const input = `{"thread":{"id":"t1","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}`
		var p cx.ThreadStartedNotification
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
}

func TestTurnCompletedNotification(t *testing.T) {
	t.Parallel()
	t.Run("Completed", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}`
		var p cx.TurnCompletedNotification
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
		t.Parallel()
		const input = `{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"something went wrong"}}}`
		var p cx.TurnCompletedNotification
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

func TestItemStartedNotification(t *testing.T) {
	t.Parallel()
	t.Run("RawItem", func(t *testing.T) {
		t.Parallel()
		const input = `{"item":{"id":"item_1","type":"commandExecution","command":"ls"},"threadId":"t1","turnId":"turn_1"}`
		var p cx.ItemStartedNotification
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
		var h cx.ItemHeader
		if err := json.Unmarshal(p.Item, &h); err != nil {
			t.Fatalf("unmarshal ItemHeader from raw: %v", err)
		}
		if h.ID != "item_1" {
			t.Errorf("ItemHeader.ID = %q", h.ID)
		}
		if h.Type != cx.ItemTypeCommandExecution {
			t.Errorf("ItemHeader.Type = %q", h.Type)
		}
	})
}

func TestAgentMessageDeltaNotification(t *testing.T) {
	t.Parallel()
	t.Run("Basic", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}`
		var p cx.AgentMessageDeltaNotification
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
}

func TestPerItemTypeStructs(t *testing.T) {
	t.Parallel()
	t.Run("AgentMessage", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"item_3","type":"agentMessage","text":"Done.","phase":"response","status":"completed"}`
		var item cx.AgentMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != "item_3" {
			t.Errorf("ID = %q", item.ID)
		}
		if item.Type != cx.ItemTypeAgentMessage {
			t.Errorf("Type = %q", item.Type)
		}
		if item.Text != "Done." {
			t.Errorf("Text = %q", item.Text)
		}
		if item.Phase != "response" {
			t.Errorf("Phase = %q", item.Phase)
		}
	})
	t.Run("Plan", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"p1","type":"plan","text":"Step 1: read code","status":"completed"}`
		var item cx.PlanItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Text != "Step 1: read code" {
			t.Errorf("Text = %q", item.Text)
		}
	})
	t.Run("Reasoning", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"r1","type":"reasoning","summary":["**Scanning...**"],"content":[],"status":"completed"}`
		var item cx.ReasoningItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if len(item.Summary) != 1 || item.Summary[0] != "**Scanning...**" {
			t.Errorf("Summary = %v", item.Summary)
		}
	})
	t.Run("CommandExecution", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"status":"completed","cwd":"/repo","durationMs":150}`
		var item cx.CommandExecutionItem
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
	})
	t.Run("FileChange", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"}`
		var item cx.FileChangeItem
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
		t.Parallel()
		const input = `{"id":"m1","type":"mcpToolCall","server":"fs","tool":"read_file","status":"completed","arguments":{"path":"/tmp/a"},"durationMs":42}`
		var item cx.McpToolCallItem
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
		t.Parallel()
		const input = `{"id":"m2","type":"mcpToolCall","server":"fs","tool":"read_file","status":"failed","error":{"message":"not found"}}`
		var item cx.McpToolCallItem
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
		t.Parallel()
		const input = `{"id":"w1","type":"webSearch","query":"golang generics","action":{"type":"search","url":"https://example.com"},"status":"completed"}`
		var item cx.WebSearchItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Query != "golang generics" {
			t.Errorf("Query = %q", item.Query)
		}
		if item.Action == nil {
			t.Fatal("Action = nil")
		}
		if item.Action.Type != "search" {
			t.Errorf("Action.Type = %q, want search", item.Action.Type)
		}
		if item.Action.URL != "https://example.com" {
			t.Errorf("Action.URL = %q, want https://example.com", item.Action.URL)
		}
	})
	t.Run("ImageView", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"i1","type":"imageView","path":"/tmp/img.png","status":"completed"}`
		var item cx.ImageViewItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Path != "/tmp/img.png" {
			t.Errorf("Path = %q", item.Path)
		}
	})
	t.Run("ContextCompaction", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"cc1","type":"contextCompaction"}`
		var item cx.ContextCompactionItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != "cc1" {
			t.Errorf("ID = %q", item.ID)
		}
	})
	t.Run("UserMessage", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"u1","type":"userMessage","content":[{"type":"text","text":"hello"}],"status":"completed"}`
		var item cx.UserMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Content == nil {
			t.Fatal("Content = nil")
		}
	})
	t.Run("DynamicToolCall", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"d1","type":"dynamicToolCall","tool":"my_tool","arguments":{"a":1},"status":"completed","success":true,"durationMs":100}`
		var item cx.DynamicToolCallItem
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
		t.Parallel()
		const input = `{"id":"ca1","type":"collabAgentToolCall","tool":"delegate","status":"completed","senderThreadId":"st1","prompt":"do this"}`
		var item cx.CollabAgentToolCallItem
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
		t.Parallel()
		const input = `{"id":"er1","type":"enteredReviewMode","review":{"state":"pending"}}`
		var item cx.EnteredReviewModeItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Review == nil {
			t.Fatal("Review = nil")
		}
	})
	t.Run("ExitedReviewMode", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"xr1","type":"exitedReviewMode","review":{"state":"approved"}}`
		var item cx.ExitedReviewModeItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Review == nil {
			t.Fatal("Review = nil")
		}
	})
	t.Run("ImageGeneration", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"ig1","type":"imageGeneration","status":"completed","revisedPrompt":"a cat","result":"data:image/png;base64,abc","savedPath":"/tmp/cat.png"}`
		var item cx.ImageGenerationItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.RevisedPrompt != "a cat" {
			t.Errorf("RevisedPrompt = %q", item.RevisedPrompt)
		}
		if item.Result != "data:image/png;base64,abc" {
			t.Errorf("Result = %q", item.Result)
		}
		if item.SavedPath != "/tmp/cat.png" {
			t.Errorf("SavedPath = %q", item.SavedPath)
		}
	})
	t.Run("HookPrompt", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"hp1","type":"hookPrompt","fragments":[{"text":"approve?"}]}`
		var item cx.HookPromptItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Fragments == nil {
			t.Fatal("Fragments = nil")
		}
	})
	t.Run("CommandExecutionSource", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"ce1","type":"commandExecution","command":"ls","source":"userShell","status":"completed"}`
		var item cx.CommandExecutionItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Source != "userShell" {
			t.Errorf("Source = %q", item.Source)
		}
	})
	t.Run("AgentMessageMemoryCitation", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"am1","type":"agentMessage","text":"hello","memoryCitation":{"entries":[{"path":"/m","lineStart":1,"lineEnd":2,"note":"n"}],"threadIds":["t1"]}}`
		var item cx.AgentMessageItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.MemoryCitation == nil {
			t.Fatal("MemoryCitation = nil")
		}
	})
	t.Run("CollabAgentModelEffort", func(t *testing.T) {
		t.Parallel()
		const input = `{"id":"ca2","type":"collabAgentToolCall","tool":"ask","status":"inProgress","model":"gpt-5.4","reasoningEffort":"high","senderThreadId":"s1","prompt":"help"}`
		var item cx.CollabAgentToolCallItem
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		if item.Model != "gpt-5.4" {
			t.Errorf("Model = %q", item.Model)
		}
		if item.ReasoningEffort != "high" {
			t.Errorf("ReasoningEffort = %q", item.ReasoningEffort)
		}
	})
}

func TestDeltaNotifications(t *testing.T) {
	t.Parallel()
	t.Run("CommandOutputDelta", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","delta":"output line\n"}`
		var p cx.CommandExecutionOutputDeltaNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Delta != "output line\n" {
			t.Errorf("Delta = %q", p.Delta)
		}
	})
	t.Run("TerminalInteraction", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","processId":"p1","stdin":"yes\n"}`
		var p cx.TerminalInteractionNotification
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
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","delta":"thinking...","summaryIndex":0}`
		var p cx.ReasoningSummaryTextDeltaNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.SummaryIndex != 0 {
			t.Errorf("SummaryIndex = %d", p.SummaryIndex)
		}
	})
	t.Run("McpToolCallProgress", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","itemId":"i1","message":"processing..."}`
		var p cx.McpToolCallProgressNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Message != "processing..." {
			t.Errorf("Message = %q", p.Message)
		}
	})
	t.Run("ThreadStatusChanged", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","status":{"type":"idle"}}`
		var p cx.ThreadStatusChangedNotification
		if err := json.Unmarshal([]byte(input), &p); err != nil {
			t.Fatal(err)
		}
		if p.Status.Type != "idle" {
			t.Errorf("Status.Type = %q, want idle", p.Status.Type)
		}
	})
	t.Run("ModelRerouted", func(t *testing.T) {
		t.Parallel()
		const input = `{"threadId":"t1","turnId":"turn_1","fromModel":"gpt-4","toModel":"gpt-3.5","reason":"rate limit"}`
		var p cx.ModelReroutedNotification
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
		t.Parallel()
		const input = `{"error":{"message":"rate limit"},"willRetry":true,"threadId":"t1","turnId":"turn_1"}`
		var p cx.ErrorNotification
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
}
