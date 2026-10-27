// Tests for Codex CLI message parsing.

package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestParseMessage(t *testing.T) {
	t.Parallel()
	t.Run("ThreadStarted", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","cliVersion":"1.0","createdAt":1771690198,"cwd":"/repo","model":"gpt-5.6-terra","modelProvider":"openai","reasoningEffort":"high","path":"/repo","preview":"fix","source":"user","status":{"type":"idle"},"updatedAt":1771690200}}}`
		msgs, err := parseTestMessage([]byte(input))
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
		if init.ReportedModel != "gpt-5.6-terra" || init.ReportedEffort != "high" {
			t.Errorf("model/effort = %q/%q, want gpt-5.6-terra/high", init.ReportedModel, init.ReportedEffort)
		}
	})
	t.Run("ThreadStartedMissingSettings", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"thread","model":null,"reasoningEffort":null}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		init, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.InitMessage", msgs[0])
		}
		if init.ReportedModel != "" || init.ReportedEffort != "" {
			t.Errorf("model/effort = %q/%q, want empty", init.ReportedModel, init.ReportedEffort)
		}
	})
	t.Run("CaicSession", func(t *testing.T) {
		t.Parallel()
		const input = `{"type":"caic_session","session_id":"thread-1","model":"gpt-5.4","agent_version":"1.2.3"}`
		msgs, err := parseTestMessage([]byte(input))
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
		if init.SessionID != "thread-1" {
			t.Errorf("SessionID = %q, want thread-1", init.SessionID)
		}
		if init.ReportedModel != "gpt-5.4" || init.ReportedEffort != "" || init.Version != "1.2.3" {
			t.Errorf("model/effort/version = %q/%q/%q, want gpt-5.4/empty/1.2.3", init.ReportedModel, init.ReportedEffort, init.Version)
		}
	})
	t.Run("TurnStarted", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (turn/started is suppressed)", len(msgs))
		}
	})
	t.Run("ItemStartedUserMessage", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"text","text":"original prompt","text_elements":[]}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "original prompt" {
			t.Errorf("Text = %q, want original prompt", ui.Text)
		}
	})
	t.Run("ItemStartedUserMessageImage", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"text","text":"the picture","text_elements":[]},{"type":"image","url":"data:image/jpeg;base64,/9j/AA==","detail":null}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "the picture" {
			t.Errorf("Text = %q, want the picture", ui.Text)
		}
		if len(ui.Images) != 1 {
			t.Fatalf("Images = %d, want 1", len(ui.Images))
		}
		if img := ui.Images[0]; img.MediaType != "image/jpeg" || img.Data != "/9j/AA==" {
			t.Errorf("Images[0] = %+v, want image/jpeg /9j/AA==", img)
		}
	})
	t.Run("ItemStartedUserMessageImageOnly", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"image","url":"data:image/png;base64,aW1hZ2U=","detail":null}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "" {
			t.Errorf("Text = %q, want empty", ui.Text)
		}
		if len(ui.Images) != 1 || ui.Images[0].MediaType != "image/png" {
			t.Errorf("Images = %+v, want one image/png", ui.Images)
		}
	})
	t.Run("ItemStartedUserMessageNonImageURL", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"image","url":"picture.jpg","detail":null}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if len(ui.Images) != 0 {
			t.Errorf("Images = %+v, want none for a non-data URL", ui.Images)
		}
	})
	t.Run("ItemCompletedUserMessageSuppressed", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"text","text":"original prompt","text_elements":[]}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (completed userMessage duplicates item/started)", len(msgs))
		}
	})
	t.Run("TurnCompleted", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed","durationMs":532000}}}`
		msgs, err := parseTestMessage([]byte(input))
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
		if rm.DurationMs != 532_000 {
			t.Errorf("DurationMs = %d, want 532000", rm.DurationMs)
		}
	})
	t.Run("TurnFailed", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"failed","error":{"message":"rate limit exceeded"}}}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","cwd":"/repo","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
	t.Run("ItemStartedCommandExecutionInfersSkillReads", func(t *testing.T) {
		t.Parallel()
		// Observed shape: Codex has no Skill tool, so it opens a skill with
		// sed and bundles several reads into one command.
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_2","type":"commandExecution","command":"sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && sed -n '1,220p' /home/user/.agents/skills/go-code-quality/SKILL.md","cwd":"/repo","status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 5 {
			t.Fatalf("msgs = %d, want 5", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ToolUseMessage); !ok {
			t.Fatalf("msgs[0] = %T, want *agent.ToolUseMessage", msgs[0])
		}
		want := []string{"code-quality", "go-code-quality"}
		for i, name := range want {
			read, ok := msgs[1+2*i].(*agent.SkillReadMessage)
			if !ok {
				t.Fatalf("msgs[%d] = %T, want *agent.SkillReadMessage", 1+2*i, msgs[1+2*i])
			}
			if read.Skill != name {
				t.Errorf("Skill = %q, want %q", read.Skill, name)
			}
			if !read.Inferred {
				t.Error("a recovered read must be marked inferred")
			}
			if read.ToolUseID != "" {
				t.Errorf("ToolUseID = %q, want empty so the tool result survives", read.ToolUseID)
			}
			// The load waits for the command's success.
			wantLoad := &agent.SkillLoadMessage{Source: agent.MessageSource{ThreadID: "t1", TurnID: "turn_1", ItemID: "item_2"}, Skill: name, Path: "/home/user/.agents/skills/" + name + "/SKILL.md", SourceToolUseID: "item_2"}
			if load, ok := msgs[2+2*i].(*agent.SkillLoadMessage); !ok || *load != *wantLoad {
				t.Errorf("msgs[%d] = %#v, want %#v", 2+2*i, msgs[2+2*i], wantLoad)
			}
		}
	})
	t.Run("SkillReadPaths", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, cmd, path string }{
			{name: "relative operand", cmd: "cat .agents/skills/review/SKILL.md", path: "/repo/.agents/skills/review/SKILL.md"},
			{name: "changed working directory", cmd: "cd /other && cat .agents/skills/review/SKILL.md"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				raw, err := json.Marshal(map[string]any{
					"jsonrpc": "2.0", "method": "item/started", "params": map[string]any{
						"item":     map[string]any{"id": "call-1", "type": "commandExecution", "command": tc.cmd, "cwd": "/repo", "status": "inProgress"},
						"threadId": "t1", "turnId": "turn_1",
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				msgs, err := parseTestMessage(raw)
				if err != nil {
					t.Fatal(err)
				}
				if len(msgs) != 3 {
					t.Fatalf("messages = %v, want tool, read, load", msgs)
				}
				if load, ok := msgs[2].(*agent.SkillLoadMessage); !ok || load.Skill != "review" || load.Path != tc.path {
					t.Errorf("load = %#v, want review with path %q", msgs[2], tc.path)
				}
			})
		}
	})
	t.Run("ItemCompletedCommandExecution", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc ls","aggregatedOutput":"docs\nsrc\n","exitCode":0,"durationMs":150.5,"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		if tr.DurationMs != 150 {
			t.Errorf("DurationMs = %d, want 150", tr.DurationMs)
		}
	})
	t.Run("ItemCompletedAgentMessage", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Done.","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agentMessage","text":"Here is my answer.","phase":"final_answer","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","summary":["**Scanning...**"],"content":[]},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"fileChange","changes":[{"path":"docs/foo.md","kind":{"type":"add"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_5","type":"fileChange","changes":[{"path":"src/main.go","kind":{"type":"update"},"diff":""}],"status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"dyn_1","type":"dynamicToolCall","tool":"my_tool","arguments":{"key":"val"},"status":"inProgress"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		success := true
		_ = success // used inline in JSON
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"dyn_1","type":"dynamicToolCall","tool":"my_tool","status":"completed","success":true},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"dyn_2","type":"dynamicToolCall","tool":"my_tool","status":"failed","success":false},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"collab_1","type":"collabAgentToolCall","tool":"delegate","status":"inProgress","senderThreadId":"thread-1","prompt":"do the thing"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		// When Tool is empty, Name should default to "collabAgent".
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"collab_2","type":"collabAgentToolCall","status":"inProgress","prompt":"hello"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"collab_1","type":"collabAgentToolCall","tool":"delegate","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"collab_3","type":"collabAgentToolCall","tool":"delegate","status":"failed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"w1","type":"mcpToolCall","server":"widget","tool":"show_widget","status":"inProgress","arguments":{"title":"demo_chart","widget_code":"<p>Hello</p>"}},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"m1","type":"mcpToolCall","server":"fs","tool":"read_file","status":"inProgress","arguments":{"path":"/tmp/a"}},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"cc_1","type":"contextCompaction"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		if sm.Subtype != "compact_boundary" {
			t.Errorf("Subtype = %q, want compact_boundary", sm.Subtype)
		}
	})
	t.Run("ItemCompletedWebSearch", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_6","type":"webSearch","query":"golang generics","status":"completed"},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/updated","params":{"item":{"id":"item_1","type":"commandExecution","aggregatedOutput":"partial..."},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		if raw.Type() != "item/updated" {
			t.Errorf("Type() = %q", raw.Type())
		}
	})
	t.Run("ReasoningSummaryTextDelta", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/reasoning/summaryTextDelta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_0","delta":"Let me think...","summaryIndex":0}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t1","turnId":"turn_1","itemId":"item_3","delta":"Hello "}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","id":1,"result":{"thread":{"id":"t1"}}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"type":"caic_diff_stat","diff_stat":[{"path":"foo.go","added":10,"deleted":2}],"ts":1719500000.5}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"error","params":{"error":{"message":"rate limit"},"willRetry":true,"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0 (error with willRetry=true is suppressed)", len(msgs))
		}
	})
	t.Run("ErrorNotificationFatal", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"error","params":{"error":{"message":"out of quota"},"willRetry":false,"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := parseTestMessage([]byte(input))
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
	t.Run("AccountRateLimitsUpdated", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1735689720.5},"secondary":{"usedPercent":85,"windowDurationMins":10080,"resetsAt":1736294520},"rateLimitReachedType":"rate_limit_reached"}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("msgs = %d, want 2", len(msgs))
		}
		primary, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if primary.Status != agent.RateLimitStatusRejected || primary.RateLimitType != "primary" || primary.QuotaWindow != "5h" || primary.Utilization != 1 {
			t.Errorf("primary = %+v, want rejected 5h primary window", primary)
		}
		if primary.QuotaProvider != agent.QuotaProviderCodex || primary.QuotaLabel != "Codex" {
			t.Errorf("primary quota identity = (%q, %q), want (%q, Codex)", primary.QuotaProvider, primary.QuotaLabel, agent.QuotaProviderCodex)
		}
		if want := time.UnixMilli(1735689720500).UTC(); !primary.ResetsAt.Equal(want) {
			t.Errorf("primary.ResetsAt = %s, want %s", primary.ResetsAt, want)
		}
		secondary, ok := msgs[1].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("msgs[1] type = %T, want *agent.RateLimitMessage", msgs[1])
		}
		if secondary.Status != agent.RateLimitStatusAllowedWarning || secondary.RateLimitType != "secondary" || secondary.QuotaWindow != "7d" || secondary.Utilization != 0.85 {
			t.Errorf("secondary = %+v, want warning 7d secondary window", secondary)
		}
	})
	t.Run("AccountCreditsDepleted", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1735689720},"rateLimitReachedType":"workspace_owner_credits_depleted"}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("msgs = %d, want 2", len(msgs))
		}
		primary, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("msgs[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if primary.Status != agent.RateLimitStatusAllowedWarning {
			t.Errorf("primary.Status = %q, want %q without a formal window rejection", primary.Status, agent.RateLimitStatusAllowedWarning)
		}
		credits, ok := msgs[1].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("msgs[1] type = %T, want *agent.RateLimitMessage", msgs[1])
		}
		if credits.Status != agent.RateLimitStatusRejected || credits.RateLimitType != "workspace_owner_credits_depleted" || credits.QuotaWindow != "credits" {
			t.Errorf("credits = %+v, want rejected credits quota", credits)
		}
	})
	t.Run("ThreadGoalUsageLimited", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"thread/goal/updated","params":{"threadId":"t1","goal":{"threadId":"t1","objective":"work","status":"usageLimited","tokensUsed":1,"timeUsedSeconds":2,"createdAt":3,"updatedAt":4}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != agent.RateLimitStatusRejected || rl.QuotaProvider != agent.QuotaProviderCodex || rl.QuotaWindow != "thread_goal" || !rl.ResetsAt.IsZero() {
			t.Errorf("RateLimitMessage = %+v, want rejected Codex thread goal without reset", rl)
		}
	})
	t.Run("ThreadGoalActive", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"thread/goal/updated","params":{"threadId":"t1","goal":{"threadId":"t1","objective":"work","status":"active","tokensUsed":1,"timeUsedSeconds":2,"createdAt":3,"updatedAt":4}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want no quota event for an active goal", len(msgs))
		}
	})

	t.Run("ThreadGoalCleared", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"method":"thread/goal/cleared","params":{"threadId":"thread-1"}}`)
		msgs, err := parseTestMessage(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("messages = %#v, want none", msgs)
		}
	})
	t.Run("AccountIndividualLimit", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"individualLimit":{"limit":"100","used":"100","remainingPercent":0,"resetsAt":1735689720},"spendControlReached":true}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		limit, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if limit.Status != agent.RateLimitStatusRejected || limit.RateLimitType != "individual" || limit.QuotaWindow != "individual" || limit.Utilization != 1 {
			t.Errorf("individual limit = %+v, want rejected individual quota", limit)
		}
	})
	t.Run("AccountSpendControlReached", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"spendControlReached":true}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		limit, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if limit.Status != agent.RateLimitStatusRejected || limit.RateLimitType != "individual" || limit.QuotaWindow != "individual" {
			t.Errorf("spend control = %+v, want rejected individual quota", limit)
		}
	})
	t.Run("AccountSpendControlRecovered", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"spendControlReached":false}}}`
		msgs, err := parseTestMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("msgs = %d, want 1", len(msgs))
		}
		limit, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if limit.Status != agent.RateLimitStatusAllowed || limit.RateLimitType != "individual" || limit.Utilization != 0 {
			t.Errorf("spend control = %+v, want allowed individual quota recovery", limit)
		}
	})
	t.Run("MalformedRateLimits", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":[]}}`
		if _, err := parseTestMessage([]byte(input)); err == nil {
			t.Fatal("parseMessage succeeded, want malformed params error")
		}
	})
	t.Run("UnknownMethod", func(t *testing.T) {
		t.Parallel()
		const input = `{"jsonrpc":"2.0","method":"future/event","params":{"data":"something"}}`
		msgs, err := parseTestMessage([]byte(input))
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
		t.Parallel()
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
			msgs, err := parseTestMessage([]byte(line))
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

func TestAgentArgs(t *testing.T) {
	t.Parallel()
	t.Run("AppServer", func(t *testing.T) {
		t.Parallel()
		args := New("", nil).AgentArgs(agent.HarnessArgs{Model: "o4-mini"})
		want := []string{
			"codex", "--no-daemon", "app-server",
			"-c", `approval_policy="never"`,
			"-c", `model_auto_compact_token_limit=244800`,
			"-c", `model_auto_compact_token_limit_scope="total"`,
			"-c", `sandbox_mode="danger-full-access"`,
		}
		if !slices.Equal(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})
	t.Run("WidgetMCPConfig", func(t *testing.T) {
		t.Parallel()
		// Widget MCP is disabled for codex; AgentArgs should not include
		// mcp_servers config.
		args := New("", nil).AgentArgs(agent.HarnessArgs{})
		for _, arg := range args {
			if strings.Contains(arg, "mcp_servers") {
				t.Errorf("unexpected widget MCP config in args %v", args)
			}
		}
	})
}

func TestWireFormat(t *testing.T) {
	t.Run("ParseMessage/explicit child evidence without lifecycle cards", func(t *testing.T) {
		t.Parallel()
		for _, metadata := range []string{`"parentThreadId":"parent"`, `"threadSource":"subagent"`} {
			t.Run(metadata, func(t *testing.T) {
				t.Parallel()
				w := New("", nil).NewWire()
				if _, err := w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"child",` + metadata + `}}}`)); err != nil {
					t.Fatal(err)
				}
				msgs, err := w.ParseMessage([]byte(`{"method":"error","params":{"threadId":"child","turnId":"turn","error":{"message":"child failed before root metadata"}}}`))
				if err != nil || len(msgs) != 1 {
					t.Fatalf("child error: %v %v", msgs, err)
				}
				if m, ok := msgs[0].(*agent.SystemMessage); !ok || m.Detail != "child failed before root metadata" || !m.Source.IsChild() || m.Source.AgentID != "codex:thread:child" {
					t.Errorf("explicit child error escaped to root: %+v", msgs[0])
				}
				if _, err := w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"parent","threadSource":"user"}}}`)); err != nil {
					t.Fatal(err)
				}
				msgs, err = w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"child"}}}`))
				if err != nil || len(msgs) != 1 {
					t.Fatalf("repeated child init: %v %v", msgs, err)
				}
				if s := msgs[0].Provenance(); !s.IsChild() || s.RootThreadID != "parent" {
					t.Fatalf("metadata-free child replaced root: %+v", s)
				}
				msgs, err = w.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"parent","turn":{"id":"turn","status":"completed"}}}`))
				if err != nil || len(msgs) != 1 {
					t.Fatalf("parent result suppressed: %v %v", msgs, err)
				}
				// Thread ownership does not prove a native lifecycle/card.
				if wire, ok := w.(*wireFormat); !ok || len(wire.nativeSubagents.spawns) != 0 {
					t.Fatal("thread metadata invented a native lifecycle card")
				}
			})
		}
	})
	t.Run("ParseMessage/root replacement across retained sessions", func(t *testing.T) {
		t.Parallel()
		w := New("", nil).NewWire()
		for _, id := range []string{"original-root", "restarted-root"} {
			if _, err := w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"` + id + `","threadSource":"user"}}}`)); err != nil {
				t.Fatal(err)
			}
			msgs, err := w.ParseMessage([]byte(`{"method":"item/started","params":{"threadId":"` + id + `","turnId":"turn","item":{"type":"commandExecution","id":"local","command":"printf root","status":"inProgress"}}}`))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("root tool: %v %v", msgs, err)
			}
			if source := msgs[0].Provenance(); source.IsChild() || source.RootThreadID != id {
				t.Fatalf("new authoritative root treated as child: %+v", source)
			}
			msgs, err = w.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"` + id + `","turn":{"id":"turn","status":"completed"}}}`))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("new root completion suppressed: %v %v", msgs, err)
			}
		}
	})

	t.Run("ParseMessage/child model and lifecycle isolation", func(t *testing.T) {
		t.Parallel()
		w := New("", nil).NewWire()
		lines := []string{
			`{"method":"thread/started","params":{"thread":{"id":"root","model":"root-model"}}}`,
			`{"method":"thread/started","params":{"thread":{"id":"child","parentThreadId":"root","threadSource":"subagent","model":"child-model"}}}`,
			`{"method":"model/rerouted","params":{"threadId":"child","turnId":"turn","fromModel":"child-model","toModel":"child-rerouted"}}`,
			`{"method":"thread/tokenUsage/updated","params":{"threadId":"child","turnId":"turn","tokenUsage":{"last":{"inputTokens":5,"totalTokens":5},"total":{"inputTokens":5,"totalTokens":5}}}}`,
			`{"method":"error","params":{"threadId":"child","turnId":"turn","error":{"message":"child failure"}}}`,
			`{"method":"thread/tokenUsage/updated","params":{"threadId":"root","turnId":"turn","tokenUsage":{"last":{"inputTokens":7,"totalTokens":7},"total":{"inputTokens":7,"totalTokens":7}}}}`,
		}
		for i, line := range lines {
			msgs, err := w.ParseMessage([]byte(line))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("line %d: %v %v", i, msgs, err)
			}
			if i > 0 && i < 5 && !msgs[0].Provenance().IsChild() {
				t.Fatalf("child origin lost: %T %+v", msgs[0], msgs[0].Provenance())
			}
			if i == 3 {
				m, ok := msgs[0].(*agent.UsageMessage)
				if !ok || m.ReportedModel != "child-rerouted" {
					t.Fatal("child model not preserved")
				}
			}
			if i == 4 {
				if m, ok := msgs[0].(*agent.SystemMessage); !ok || m.Detail != "child failure" {
					t.Fatalf("child error lost: %+v", msgs[0])
				}
			}
			if i == 5 {
				if m, ok := msgs[0].(*agent.UsageMessage); !ok || m.Source.IsChild() || m.Source.RootThreadID != "root" || m.ReportedModel != "root-model" {
					t.Fatalf("child changed root identity/model: %+v", m)
				}
			}
		}
		msgs, err := w.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"root","turn":{"id":"turn","status":"completed"}}}`))
		if err != nil || len(msgs) != 1 {
			t.Fatalf("parent result: %v %v", msgs, err)
		}
		if m, ok := msgs[0].(*agent.ResultMessage); !ok || m.Usage.InputTokens != 12 {
			t.Fatalf("child failure consumed parent usage: %v", msgs)
		}
	})
	t.Run("ParseMessage/known child before root metadata", func(t *testing.T) {
		t.Parallel()
		w := New("", nil).NewWire()
		if _, err := w.ParseMessage([]byte(`{"method":"item/started","params":{"threadId":"parent","turnId":"turn","item":{"type":"subAgentActivity","id":"spawn","agentThreadId":"child","agentPath":"/root/child","kind":"started","model":"reported-child-model"}}}`)); err != nil {
			t.Fatal(err)
		}
		msgs, err := w.ParseMessage([]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"child","turnId":"turn","tokenUsage":{"total":{"inputTokens":1,"totalTokens":1},"last":{"inputTokens":1,"totalTokens":1}}}}`))
		if err != nil || len(msgs) != 1 {
			t.Fatalf("usage: %v %v", msgs, err)
		}
		if m, ok := msgs[0].(*agent.UsageMessage); !ok || !m.Source.IsChild() || m.ReportedModel != "reported-child-model" || m.Source.RootThreadID != "" {
			t.Fatalf("reported ownership/model lost: %+v", m)
		}
		msgs, err = w.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"child","turn":{"id":"turn","status":"completed"}}}`))
		if err != nil || len(msgs) != 0 {
			t.Fatalf("known child completed unknown root: %v %v", msgs, err)
		}
		for _, root := range []string{"first-root", "restarted-root"} {
			if _, err := w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"` + root + `","threadSource":"user"}}}`)); err != nil {
				t.Fatal(err)
			}
			// A child announced before session metadata stays a child when its
			// own init omits optional delegation fields during warm replay.
			msgs, err := w.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"child"}}}`))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("child init: %v %v", msgs, err)
			}
			if s := msgs[0].Provenance(); !s.IsChild() || s.RootThreadID != root {
				t.Fatalf("known child replaced root during replay: %+v", s)
			}
			msgs, err = w.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"` + root + `","turn":{"id":"turn","status":"completed"}}}`))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("root result lost after child init: %v %v", msgs, err)
			}
		}
	})
	t.Parallel()
	// Concurrent provenance reproduces child answer fragments interleaved with
	// root reasoning, including completion and retained raw-log replay.
	t.Run("ParseMessage/concurrent provenance", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"method":"thread/started","params":{"thread":{"id":"parent"}}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"child","turnId":"child-turn","itemId":"answer","delta":"A bow"}}`,
			`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"parent","turnId":"parent-turn","itemId":"reason","summaryIndex":0,"delta":"Evaluating"}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"child","turnId":"child-turn","itemId":"answer","delta":"tie"}}`,
			`{"method":"item/completed","params":{"threadId":"child","turnId":"child-turn","item":{"id":"answer","type":"agentMessage","text":"A bowtie"}}}`,
			`{"method":"item/completed","params":{"threadId":"parent","turnId":"parent-turn","item":{"id":"reason","type":"reasoning","summary":["Evaluating"]}}}`,
		}
		for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
			t.Run(fmt.Sprint(version), func(t *testing.T) {
				t.Parallel()
				w := &wireFormat{}
				parser, err := agent.NewLogRecordParser(agent.LogVersionV3, w.ParseMessage)
				if version != 0 {
					parser, err = agent.NewLogRecordParser(version, w.ParseMessage)
				}
				if err != nil {
					t.Fatal(err)
				}
				for i, line := range lines {
					var msgs []agent.Message
					if version == 0 {
						msgs, err = w.ParseMessage([]byte(line))
					} else {
						record, e := parser.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":` + line + `}`))
						err = e
						for _, m := range record.Messages {
							msgs = append(msgs, m.Message)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						continue
					}
					if len(msgs) != 1 {
						t.Fatalf("line %d messages = %v", i, msgs)
					}
					data, e := json.Marshal(msgs[0])
					if e != nil {
						t.Fatal(e)
					}
					var m struct {
						Source struct {
							ThreadID     string `json:"threadID"`
							TurnID       string `json:"turnID"`
							ItemID       string `json:"itemID"`
							RootThreadID string `json:"rootThreadID"`
						} `json:"source"`
					}
					if e = json.Unmarshal(data, &m); e != nil {
						t.Fatal(e)
					}
					thread, turn, item := "child", "child-turn", "answer"
					if i == 2 || i == 5 {
						thread, turn, item = "parent", "parent-turn", "reason"
					}
					wantAgentID := ""
					if thread == "child" {
						wantAgentID = "codex:thread:child"
					}
					if got := msgs[0].Provenance().AgentID; got != wantAgentID {
						t.Errorf("line %d agentID = %q, want %q", i, got, wantAgentID)
					}
					if m.Source.ThreadID != thread || m.Source.TurnID != turn || m.Source.ItemID != item || m.Source.RootThreadID != "parent" {
						t.Errorf("line %d provenance = %s", i, data)
					}
				}
			})
		}
	})

	t.Run("ChildThreadSkillReadLoadsNothing", func(t *testing.T) {
		t.Parallel()
		// Observed shape (4DLLPGQ6C000): spawned child threads run the same
		// skill read as the root thread on the root session's wire.
		wire := New("", nil).NewWire()
		if _, err := wire.ParseMessage([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"root"}}}`)); err != nil {
			t.Fatal(err)
		}
		for _, line := range []struct {
			thread string
			loads  int
		}{{"child", 0}, {"root", 1}} {
			msgs, err := wire.ParseMessage([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_` + line.thread + `","type":"commandExecution","command":"cat /home/user/.agents/skills/code-quality/SKILL.md","cwd":"/repo","status":"inProgress"},"threadId":"` + line.thread + `","turnId":"turn_1"}}`))
			if err != nil {
				t.Fatal(err)
			}
			var reads, loads int
			for _, m := range msgs {
				switch m.(type) {
				case *agent.SkillReadMessage:
					reads++
				case *agent.SkillLoadMessage:
					loads++
				}
			}
			if reads != 1 || loads != line.loads {
				t.Errorf("%s thread: reads = %d, loads = %d, want 1 and %d", line.thread, reads, loads, line.loads)
			}
		}
	})
	t.Run("WritePromptBasic", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{threadID: "t1"}
		var buf bytes.Buffer
		if err := w.WritePrompt(&buf, agent.Prompt{Text: "fix the bug"}, agent.DiscardLogSink{Version: agent.LogVersionV1}); err != nil {
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
		if params["summary"] != "auto" {
			t.Errorf("summary = %v, want auto", params["summary"])
		}
	})
	t.Run("WritePromptEffort", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{threadID: "t1", requestedEffort: "high"}
		var buf bytes.Buffer
		if err := w.WritePrompt(&buf, agent.Prompt{Text: "fix the bug"}, agent.DiscardLogSink{Version: agent.LogVersionV1}); err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
			t.Fatal(err)
		}
		params, ok := req["params"].(map[string]any)
		if !ok {
			t.Fatal("params not a map")
		}
		if params["summary"] != "auto" {
			t.Errorf("summary = %v, want auto", params["summary"])
		}
		if params["effort"] != "high" {
			t.Errorf("effort = %v, want high", params["effort"])
		}
	})
	t.Run("WritePromptNoThreadID", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{}
		var buf bytes.Buffer
		err := w.WritePrompt(&buf, agent.Prompt{Text: "hello"}, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err == nil {
			t.Fatal("expected error for missing thread ID")
		}
	})
	t.Run("SuppressUserInput", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{suppressUserInput: true}
		const input = `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"u1","type":"userMessage","content":[{"type":"text","text":"original prompt"}]},"threadId":"t1","turnId":"turn_1"}}`
		msgs, err := w.ParseMessage([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("msgs = %d, want 0", len(msgs))
		}
	})
	t.Run("ParseMessageCapturesThreadID", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		w := &wireFormat{}
		w.reportedModel = "gpt-5.6"
		w.threadID = "t1"
		const input = `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"cacheWriteInputTokens":100,"outputTokens":200,"reasoningOutputTokens":0},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"cacheWriteInputTokens":12,"outputTokens":20,"reasoningOutputTokens":5}}}}`
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
		if um.Usage.InputTokens != 18 {
			t.Errorf("InputTokens = %d, want 18", um.Usage.InputTokens)
		}
		if um.Usage.OutputTokens != 20 {
			t.Errorf("OutputTokens = %d, want 20", um.Usage.OutputTokens)
		}
		if um.Usage.CacheReadInputTokens != 50 {
			t.Errorf("CacheReadInputTokens = %d, want 50", um.Usage.CacheReadInputTokens)
		}
		if um.Usage.CacheCreationInputTokens != 12 {
			t.Errorf("CacheCreationInputTokens = %d, want 12", um.Usage.CacheCreationInputTokens)
		}
		if um.Usage.ReasoningOutputTokens != 5 {
			t.Errorf("ReasoningOutputTokens = %d, want 5", um.Usage.ReasoningOutputTokens)
		}
		if um.Usage.CacheTTLSeconds != 0 {
			t.Errorf("CacheTTLSeconds = %d, want unknown", um.Usage.CacheTTLSeconds)
		}
		if um.ReportedModel != "gpt-5.6" {
			t.Errorf("ReportedModel = %q, want session model gpt-5.6", um.ReportedModel)
		}
		if !um.ModelDerived {
			t.Errorf("ModelDerived = false, want true (session model stamp, not harness-reported)")
		}
		// A wire that starts mid-thread can charge only the last call.
		w.mu.Lock()
		total := w.totalUsage
		w.mu.Unlock()
		if total.InputTokens != 18 {
			t.Errorf("totalUsage.InputTokens = %d, want 18", total.InputTokens)
		}
		if total.CacheCreationInputTokens != 12 {
			t.Errorf("totalUsage.CacheCreationInputTokens = %d, want 12", total.CacheCreationInputTokens)
		}
	})
	t.Run("ModelRerouteUpdatesUsageAttribution", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{}
		w.reportedModel = "gpt-5.6"
		reroute := []byte(`{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"t1","turnId":"turn_1","fromModel":"gpt-5.6","toModel":"gpt-5.6-codex"}}`)
		if _, err := w.ParseMessage(reroute); err != nil {
			t.Fatal(err)
		}
		usage := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{},"last":{"totalTokens":10,"inputTokens":10,"outputTokens":0,"reasoningOutputTokens":0}}}}`)
		msgs, err := w.ParseMessage(usage)
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
		if um.ReportedModel != "gpt-5.6-codex" {
			t.Errorf("ReportedModel = %q, want rerouted gpt-5.6-codex", um.ReportedModel)
		}
	})
	t.Run("TokenUsageAccumulates", func(t *testing.T) {
		t.Parallel()
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
		if total.InputTokens != 100 {
			t.Errorf("totalUsage.InputTokens = %d, want 100", total.InputTokens)
		}
		if total.OutputTokens != 40 {
			t.Errorf("totalUsage.OutputTokens = %d, want 40", total.OutputTokens)
		}
		if total.CacheReadInputTokens != 10 {
			t.Errorf("totalUsage.CacheReadInputTokens = %d, want 10", total.CacheReadInputTokens)
		}
	})
	t.Run("RepeatedCumulativeTotalIsNotChargedTwice", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{}
		lines := []string{
			`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":100,"inputTokens":80,"outputTokens":20},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`,
			`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":100,"inputTokens":80,"outputTokens":20},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`,
			`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":160,"inputTokens":130,"outputTokens":30},"last":{"totalTokens":40,"inputTokens":30,"outputTokens":10}}}}`,
		}
		want := []agent.Usage{{InputTokens: 80, OutputTokens: 20}, {}, {InputTokens: 50, OutputTokens: 10}}
		for i, line := range lines {
			msgs, err := w.ParseMessage([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			usage, ok := msgs[0].(*agent.UsageMessage)
			if !ok {
				t.Fatalf("event %d type = %T, want *agent.UsageMessage", i, msgs[0])
			}
			got := usage.Usage
			if got != want[i] {
				t.Errorf("event %d usage = %+v, want %+v", i, got, want[i])
			}
		}
	})
	t.Run("ResumedThreadSnapshotIsBaseline", func(t *testing.T) {
		t.Parallel()
		prior := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"outputTokens":200},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`)
		newCall := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":1120,"inputTokens":900,"outputTokens":220},"last":{"totalTokens":120,"inputTokens":100,"outputTokens":20}}}}`)
		turnComplete := []byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`)
		response := []byte(`{"jsonrpc":"2.0","id":7,"result":{"thread":{"id":"t1"}}}`)
		turnStarted := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_2","status":"inProgress"}}}`)
		for _, live := range []bool{false, true} {
			w := &wireFormat{threadID: "t1", suppressUserInput: live}
			if !live {
				if _, err := w.ParseMessage(prior); err != nil {
					t.Fatal(err)
				}
				if _, err := w.ParseMessage(turnComplete); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.ParseMessage(response); err != nil {
				t.Fatal(err)
			}
			msgs, err := w.ParseMessage(prior)
			if err != nil {
				t.Fatal(err)
			}
			usage, ok := msgs[0].(*agent.UsageMessage)
			if !ok || usage.Usage != (agent.Usage{}) {
				t.Fatalf("live=%v snapshot = %#v, want zero usage", live, msgs[0])
			}
			if _, err := w.ParseMessage(turnStarted); err != nil {
				t.Fatal(err)
			}
			msgs, err = w.ParseMessage(newCall)
			if err != nil {
				t.Fatal(err)
			}
			usage, ok = msgs[0].(*agent.UsageMessage)
			if !ok || usage.Usage.InputTokens != 100 || usage.Usage.OutputTokens != 20 {
				t.Errorf("live=%v new call = %#v, want 100 input and 20 output", live, msgs[0])
			}
			msgs, err = w.ParseMessage(turnComplete)
			if err != nil {
				t.Fatal(err)
			}
			result, ok := msgs[0].(*agent.ResultMessage)
			if !ok || result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 20 {
				t.Errorf("live=%v result = %#v, want 100 input and 20 output", live, msgs[0])
			}
		}
	})
	t.Run("ResumedThreadWithoutSnapshotChargesFirstCall", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{threadID: "t1"}
		for _, line := range []string{
			`{"jsonrpc":"2.0","id":7,"result":{"thread":{"id":"t1"}}}`,
			`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1","status":"inProgress"}}}`,
		} {
			if _, err := w.ParseMessage([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		msgs, err := w.ParseMessage([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":100,"inputTokens":80,"outputTokens":20},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		usage, ok := msgs[0].(*agent.UsageMessage)
		if !ok || usage.Usage.InputTokens != 80 || usage.Usage.OutputTokens != 20 {
			t.Errorf("first call = %#v, want 80 input and 20 output", msgs[0])
		}
	})
	t.Run("MidThreadTailChargesOnlyLastCall", func(t *testing.T) {
		t.Parallel()
		w := &wireFormat{}
		usageLine := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"outputTokens":200},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`)
		if _, err := w.ParseMessage(usageLine); err != nil {
			t.Fatal(err)
		}
		msgs, err := w.ParseMessage([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		result, ok := msgs[0].(*agent.ResultMessage)
		if !ok || result.Usage.InputTokens != 80 || result.Usage.OutputTokens != 20 {
			t.Errorf("result = %#v, want 80 input and 20 output", msgs[0])
		}
	})
	t.Run("TurnCompletedInjectsAndResetsUsage", func(t *testing.T) {
		t.Parallel()
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

func TestReadEditBashFixtureFileChangeHasDetail(t *testing.T) {
	t.Parallel()

	msgs := agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", New("", nil).NewWire().ParseMessage)
	for _, msg := range msgs {
		use, ok := msg.(*agent.ToolUseMessage)
		if !ok || use.Name != "Edit" {
			continue
		}
		if use.Detail != "main.go" {
			t.Fatalf("detail = %q, want main.go", use.Detail)
		}
		if use.InputView.Kind != agent.ToolInputFileChanges {
			t.Fatalf("input view kind = %q, want fileChanges", use.InputView.Kind)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		file := use.InputView.Files[0]
		if file.Path != "/workspace/main.go" || !strings.Contains(file.Patch, "Hi, World!") {
			t.Fatalf("file change = %#v", file)
		}
		var changes []struct {
			Path string `json:"path"`
			Diff string `json:"diff"`
		}
		if err := json.Unmarshal(use.Input, &changes); err != nil {
			t.Fatalf("unmarshal fileChange input: %v", err)
		}
		if len(changes) != 1 || changes[0].Path != "/workspace/main.go" || !strings.Contains(changes[0].Diff, "Hi, World!") {
			t.Fatalf("changes = %#v", changes)
		}
		return
	}
	t.Fatal("no Codex Edit fileChange tool use found")
}

func parseTestMessage(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, err
	}
	msgs, _, err := parseMessage(line, &probe)
	return msgs, err
}

func TestReplayBufferOwnership(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	line := []byte(`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"item","delta":"hello"}}`)
	msgs, err := wire.ParseMessage(line)
	if err != nil {
		t.Fatal(err)
	}
	for i := range line {
		line[i] = 'x'
	}
	msg, ok := msgs[0].(*agent.TextDeltaMessage)
	if !ok || msg.Text != "hello" {
		t.Fatalf("message = %#v", msgs)
	}
}

func TestCodexChildEventOwnership(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	lines := []string{
		`{"method":"thread/started","params":{"thread":{"id":"root","model":"root-model"}}}`,
		`{"method":"item/started","params":{"threadId":"child","turnId":"turn","item":{"type":"commandExecution","id":"command","command":"cat /home/user/.agents/skills/review/SKILL.md","cwd":"/src","status":"inProgress"}}}`,
		`{"method":"item/commandExecution/outputDelta","params":{"threadId":"child","turnId":"turn","itemId":"command","delta":"output"}}`,
		`{"method":"item/completed","params":{"threadId":"child","turnId":"turn","item":{"type":"commandExecution","id":"command","command":"cat /home/user/.agents/skills/review/SKILL.md","cwd":"/src","status":"completed","exitCode":0}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"child","turnId":"turn","tokenUsage":{"total":{"totalTokens":12,"inputTokens":10,"outputTokens":2},"last":{"totalTokens":12,"inputTokens":10,"outputTokens":2}}}}`,
		`{"method":"error","params":{"threadId":"child","turnId":"turn","error":{"message":"child failed"}}}`,
	}
	for i, line := range lines {
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if i == 0 {
			continue
		}
		if len(msgs) == 0 {
			t.Fatalf("line %d lost visible or analytics event", i)
		}
		for _, m := range msgs {
			s := m.Provenance()
			if s.AgentID != "codex:thread:child" || s.ThreadID != "child" || s.TurnID != "turn" || s.RootThreadID != "root" {
				t.Errorf("line %d %T source = %+v", i, m, s)
			}
			if _, ok := m.(*agent.ResultMessage); ok {
				t.Errorf("line %d child event emitted a parent result", i)
			}
		}
	}
}
