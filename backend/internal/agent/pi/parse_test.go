// Tests for Pi CLI message parsing.

package pi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("edit tool display", func(t *testing.T) {
		t.Parallel()

		use := newToolUseMessage("edit-1", "edit", "Edit", json.RawMessage(`{"path":"gomode/docs/SERVER_LIBRARY.md","edits":[{"oldText":"before","newText":"after"}]}`))
		if use.Detail != "SERVER_LIBRARY.md" {
			t.Fatalf("detail = %q", use.Detail)
		}
		if use.InputView.Kind != "edit" || use.InputView.Edit.Path != "gomode/docs/SERVER_LIBRARY.md" || len(use.InputView.Edit.Edits) != 1 {
			t.Fatalf("input view = %#v", use.InputView)
		}
	})
	t.Run("read edit bash fixture normalizes edit", func(t *testing.T) {
		t.Parallel()

		msgs := agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", New("", nil).NewWire().ParseMessage)
		var use *agent.ToolUseMessage
		for _, msg := range msgs {
			candidate, ok := msg.(*agent.ToolUseMessage)
			if ok && candidate.Name == "Edit" && candidate.InputView.Kind == agent.ToolInputEdit {
				use = candidate
				break
			}
		}
		if use == nil {
			t.Fatal("no normalized Edit tool use found")
		}
		if use.Detail != "main.go" {
			t.Fatalf("detail = %q, want main.go", use.Detail)
		}
		edit := use.InputView.Edit
		if edit.Path != "/workspace/main.go" || len(edit.Edits) != 1 {
			t.Fatalf("edit view = %#v", edit)
		}
		if edit.Edits[0].OldText != `fmt.Println("Hello, World!")` || edit.Edits[0].NewText != `fmt.Println("Hi, World!")` {
			t.Fatalf("edit replacement = %#v", edit.Edits[0])
		}
	})
	t.Run("edit args", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			edit, ok := parseEditArgs(json.RawMessage(`{"path":"gomode/docs/SERVER_LIBRARY.md","edits":[{"oldText":"before","newText":"after"}]}`))
			if !ok {
				t.Fatal("parseEditArgs returned false")
			}
			if edit.Path != "gomode/docs/SERVER_LIBRARY.md" || len(edit.Edits) != 1 || edit.Edits[0].OldText != "before" || edit.Edits[0].NewText != "after" {
				t.Fatalf("edit = %+v", edit)
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, ok := parseEditArgs(json.RawMessage(`{"path":"x","edits":[{"newText":"after"}]}`)); !ok {
				return
			}
			t.Fatal("parseEditArgs accepted malformed edit")
		})
	})
	t.Run("subagent args", func(t *testing.T) {
		t.Parallel()
		t.Run("single", func(t *testing.T) {
			t.Parallel()
			info := parseSubagentArgs([]byte(`{"agent":"reviewer","task":"Review the last commit","cwd":"/w"}`))
			if info.Kind != "single" {
				t.Fatalf("kind = %q, want single", info.Kind)
			}
			if len(info.Spawns) != 1 || info.Spawns[0].Agent != "reviewer" || info.Spawns[0].Task != "Review the last commit" {
				t.Fatalf("spawns = %+v", info.Spawns)
			}
		})
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()
			info := parseSubagentArgs([]byte(`{"tasks":[{"agent":"reviewer","task":"a"},{"agent":"reviewer","task":"b"}],"concurrency":2}`))
			if info.Kind != "parallel" {
				t.Fatalf("kind = %q, want parallel", info.Kind)
			}
			if len(info.Spawns) != 2 {
				t.Fatalf("got %d spawns, want 2", len(info.Spawns))
			}
		})
		t.Run("chain preserves order", func(t *testing.T) {
			t.Parallel()
			info := parseSubagentArgs([]byte(`{"chain":[{"parallel":[{"agent":"reviewer","label":"plan-a","phase":"Planning","task":"p1"},{"agent":"reviewer","label":"plan-b","phase":"Planning","task":"p2"}]},{"agent":"worker","phase":"Impl","task":"do it"}]}`))
			if info.Kind != "chain" {
				t.Fatalf("kind = %q, want chain", info.Kind)
			}
			got := []string{info.Spawns[0].Agent, info.Spawns[1].Agent, info.Spawns[2].Agent}
			if got[0] != "reviewer" || got[1] != "reviewer" || got[2] != "worker" {
				t.Fatalf("order = %v, want [reviewer reviewer worker]", got)
			}
			if info.Spawns[0].Phase != "Planning" || info.Spawns[0].Label != "plan-a" {
				t.Fatalf("first spawn metadata = %+v", info.Spawns[0])
			}
		})
		t.Run("action list spawns nothing", func(t *testing.T) {
			t.Parallel()
			info := parseSubagentArgs([]byte(`{"action":"list"}`))
			if info.Kind != "action" || info.Action != "list" || len(info.Spawns) != 0 {
				t.Fatalf("info = %+v, want action/list/no-spawns", info)
			}
		})
		t.Run("empty", func(t *testing.T) {
			t.Parallel()
			if info := parseSubagentArgs(nil); info.Kind != "" || len(info.Spawns) != 0 {
				t.Fatalf("info = %+v, want zero", info)
			}
		})
	})
	t.Run("subagent description", func(t *testing.T) {
		t.Parallel()
		t.Run("single with label", func(t *testing.T) {
			t.Parallel()
			got := subagentDescription("single", []agent.SubagentSpawn{{Agent: "reviewer", Label: "security pass", Task: "ignored"}})
			if got != "reviewer — security pass" {
				t.Fatalf("got %q", got)
			}
		})
		t.Run("single falls back to task first line", func(t *testing.T) {
			t.Parallel()
			got := subagentDescription("single", []agent.SubagentSpawn{{Agent: "planner", Task: "\n  Plan the work  \nmore"}})
			if got != "planner — Plan the work" {
				t.Fatalf("got %q", got)
			}
		})
		t.Run("multiple counts by agent", func(t *testing.T) {
			t.Parallel()
			got := subagentDescription("chain", []agent.SubagentSpawn{{Agent: "reviewer"}, {Agent: "reviewer"}, {Agent: "reviewer"}, {Agent: "worker"}})
			if got != "chain · reviewer ×3, worker" {
				t.Fatalf("got %q", got)
			}
		})
	})
	t.Run("subagent status", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			if got := subagentStatus(false, "2/2 succeeded"); got != "completed" {
				t.Fatalf("got %q, want completed", got)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if got := subagentStatus(true, ""); got != "failed" {
				t.Fatalf("got %q, want failed", got)
			}
			if got := subagentStatus(false, "❌ Chain failed at step 2"); got != "failed" {
				t.Fatalf("got %q, want failed", got)
			}
		})
	})
	t.Run("subagent start", func(t *testing.T) {
		t.Parallel()
		t.Run("subagent spawn emits start and tool use", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_start","toolCallId":"c1","toolName":"subagent","args":{"agent":"reviewer","task":"Review"}}`)
			msgs, err := parseToolExecStart(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 {
				t.Fatalf("got %d messages, want 2: %#v", len(msgs), msgs)
			}
			start, ok := msgs[0].(*agent.SubagentStartMessage)
			if !ok {
				t.Fatalf("msgs[0] = %T, want SubagentStartMessage", msgs[0])
			}
			if start.TaskID != "c1" || start.Description != "reviewer — Review" {
				t.Fatalf("start = %+v", start)
			}
			use, ok := msgs[1].(*agent.ToolUseMessage)
			if !ok || use.Name != "Agent" {
				t.Fatalf("msgs[1] = %#v, want ToolUseMessage named Agent", msgs[1])
			}
			if use.Detail != "reviewer — Review" || use.InputView.Kind != agent.ToolInputSubagents || len(use.InputView.Subagents) != 1 {
				t.Fatalf("tool display = detail %q, view %#v", use.Detail, use.InputView)
			}
		})
		t.Run("subagent introspection emits only tool use", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_start","toolCallId":"c2","toolName":"subagent","args":{"action":"list"}}`)
			msgs, err := parseToolExecStart(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			if _, ok := msgs[0].(*agent.ToolUseMessage); !ok {
				t.Fatalf("msgs[0] = %T, want ToolUseMessage", msgs[0])
			}
		})
		t.Run("regular tool emits only tool use", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_start","toolCallId":"c3","toolName":"bash","args":{"command":"ls"}}`)
			msgs, err := parseToolExecStart(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
		})
	})
	t.Run("subagent end", func(t *testing.T) {
		t.Parallel()
		t.Run("subagent success emits end, output, and result", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"subagent","result":{"content":[{"type":"text","text":"2/2 succeeded\n\nfindings"}]},"isError":false}`)
			msgs, err := parseToolExecEnd(line)
			if err != nil {
				t.Fatal(err)
			}
			end, ok := msgs[0].(*agent.SubagentEndMessage)
			if !ok || end.TaskID != "c1" || end.Status != "completed" {
				t.Fatalf("msgs[0] = %#v, want completed SubagentEndMessage", msgs[0])
			}
			out, ok := msgs[1].(*agent.ToolOutputDeltaMessage)
			if !ok || out.Delta == "" {
				t.Fatalf("msgs[1] = %#v, want non-empty ToolOutputDeltaMessage", msgs[1])
			}
			if _, ok := msgs[2].(*agent.ToolResultMessage); !ok {
				t.Fatalf("msgs[2] = %T, want ToolResultMessage", msgs[2])
			}
		})
		t.Run("subagent failure emits end and error result without output", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"subagent","result":{"content":[{"type":"text","text":"❌ failed"}]},"isError":true}`)
			msgs, err := parseToolExecEnd(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 {
				t.Fatalf("got %d messages, want 2 (end, result): %#v", len(msgs), msgs)
			}
			end, ok := msgs[0].(*agent.SubagentEndMessage)
			if !ok || end.Status != "failed" {
				t.Fatalf("msgs[0] = %#v, want failed SubagentEndMessage", msgs[0])
			}
			res, ok := msgs[1].(*agent.ToolResultMessage)
			if !ok || res.Error == "" {
				t.Fatalf("msgs[1] = %#v, want ToolResultMessage with error", msgs[1])
			}
		})
		t.Run("regular tool emits only result", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"ok"}]},"isError":false}`)
			msgs, err := parseToolExecEnd(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
		})
	})
	t.Run("subagent output", func(t *testing.T) {
		t.Parallel()
		t.Run("suppresses running placeholder", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_update","toolCallId":"c1","toolName":"subagent","partialResult":{"content":[{"type":"text","text":"(running...)"}]}}`)
			msgs, err := parseToolExecUpdate(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 0 {
				t.Fatalf("got %d messages, want 0 (placeholder suppressed)", len(msgs))
			}
		})
		t.Run("emits real output", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello"}]}}`)
			msgs, err := parseToolExecUpdate(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
		})
	})
	t.Run("subagent wire", func(t *testing.T) {
		t.Parallel()
		wire := New("", nil).NewWire()
		feed := func(line string) []agent.Message {
			msgs, err := wire.ParseMessage([]byte(line))
			if err != nil {
				t.Fatalf("ParseMessage(%s): %v", line, err)
			}
			return msgs
		}

		start := feed(`{"type":"tool_execution_start","toolCallId":"c1","toolName":"subagent","args":{"agent":"reviewer","task":"Review"}}`)
		if _, ok := start[0].(*agent.SubagentStartMessage); !ok {
			t.Fatalf("start[0] = %T, want SubagentStartMessage", start[0])
		}

		if msgs := feed(`{"type":"tool_execution_update","toolCallId":"c1","toolName":"subagent","partialResult":{"content":[{"type":"text","text":"(running...)"}]}}`); len(msgs) != 0 {
			t.Fatalf("running placeholder produced %d messages, want 0", len(msgs))
		}

		end := feed(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"subagent","result":{"content":[{"type":"text","text":"1/1 succeeded\n\nLooks good."}]},"isError":false}`)
		var gotEnd, gotResult bool
		var output string
		for _, m := range end {
			switch v := m.(type) {
			case *agent.SubagentEndMessage:
				gotEnd = true
			case *agent.ToolOutputDeltaMessage:
				output = v.Delta
			case *agent.ToolResultMessage:
				gotResult = true
			}
		}
		if !gotEnd || !gotResult {
			t.Fatalf("end messages = %#v, want SubagentEnd + ToolResult", end)
		}
		if output != "1/1 succeeded\n\nLooks good." {
			t.Fatalf("output = %q, want full result text", output)
		}
	})
	t.Run("subagent args round trip", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{
			`{"agent":"reviewer","task":"x"}`,
			`{"tasks":[{"agent":"a","task":"t"}]}`,
			`{"chain":[{"parallel":[{"agent":"a","task":"t"}]}]}`,
		} {
			var args pi.SubagentToolArgs
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if len(subagentSpawns(&args)) == 0 {
				t.Fatalf("no spawns for %s", raw)
			}
		}
	})
}

func TestBackendNewWire(t *testing.T) {
	t.Parallel()
	msgUpdateWithAccumulatedMessage := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello"},"message":{"role":"assistant","content":[{"type":"text","text":"ignored accumulated text"}]}}`
	// Regression: replay (relay output during adoption, and on-disk logs) uses
	// NewWire().ParseMessage. It must use the stateful wire format so agent_end
	// produces a terminal ResultMessage; without it RestoreMessages cannot infer
	// the waiting state and adopted pi tasks stay stuck as "running".
	t.Run("synthesizes ResultMessage on agent_end", func(t *testing.T) {
		t.Parallel()
		agentEnd := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":10,"output":5}}]}`)
		msgs, err := New("", nil).NewWire().ParseMessage(agentEnd)
		if err != nil {
			t.Fatal(err)
		}
		var hasResult bool
		for _, m := range msgs {
			if _, ok := m.(*agent.ResultMessage); ok {
				hasResult = true
			}
		}
		if !hasResult {
			t.Fatalf("NewWire().ParseMessage produced no ResultMessage for agent_end: %#v", msgs)
		}
	})

	t.Run("message_update ignores accumulated message payload", func(t *testing.T) {
		t.Parallel()
		msgs, err := New("", nil).NewWire().ParseMessage([]byte(msgUpdateWithAccumulatedMessage))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		delta, ok := msgs[0].(*agent.TextDeltaMessage)
		if !ok {
			t.Fatalf("got %T, want TextDeltaMessage", msgs[0])
		}
		if delta.Text != "hello" {
			t.Errorf("delta = %q, want hello", delta.Text)
		}
	})

	t.Run("message_end emits consolidated text", func(t *testing.T) {
		t.Parallel()
		msgs, err := New("", nil).NewWire().ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"final text"}],"stopReason":"stop"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		text, ok := msgs[0].(*agent.TextMessage)
		if !ok {
			t.Fatalf("got %T, want TextMessage", msgs[0])
		}
		if text.Text != "final text" {
			t.Errorf("text = %q, want final text", text.Text)
		}
	})

	t.Run("tool_execution_update emits incremental deltas", func(t *testing.T) {
		t.Parallel()
		parser := New("", nil).NewWire().ParseMessage

		// Simulate two tool_execution_update events with accumulated text.
		// The second event contains the full accumulated text (first + new).
		update1 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello"}]}}`)
		update2 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello world"}]}}`)

		msgs1, err := parser(update1)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs1) != 1 {
			t.Fatalf("update1: got %d messages, want 1", len(msgs1))
		}
		tod1, ok := msgs1[0].(*agent.ToolOutputDeltaMessage)
		if !ok {
			t.Fatalf("update1: got %T, want ToolOutputDeltaMessage", msgs1[0])
		}
		if tod1.Delta != "hello" {
			t.Errorf("update1 delta: got %q, want %q", tod1.Delta, "hello")
		}

		msgs2, err := parser(update2)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs2) != 1 {
			t.Fatalf("update2: got %d messages, want 1", len(msgs2))
		}
		tod2, ok := msgs2[0].(*agent.ToolOutputDeltaMessage)
		if !ok {
			t.Fatalf("update2: got %T, want ToolOutputDeltaMessage", msgs2[0])
		}
		// Should be only the incremental part, not the full accumulated text.
		if tod2.Delta != " world" {
			t.Errorf("update2 delta: got %q, want %q", tod2.Delta, " world")
		}

		// Third update with same text should produce empty delta (filtered out).
		update3 := []byte(`{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"hello world"}]}}`)
		msgs3, err := parser(update3)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs3) != 0 {
			t.Fatalf("update3: got %d messages, want 0 (empty delta filtered)", len(msgs3))
		}
	})

	t.Run("caic_exit carries relay stderr", func(t *testing.T) {
		t.Parallel()
		parser := New("", nil).NewWire().ParseMessage
		line := []byte(`{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}`)
		msgs, err := parser(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		exit, ok := msgs[0].(*agent.ExitMessage)
		if !ok {
			t.Fatalf("got %T, want ExitMessage", msgs[0])
		}
		if exit.ExitCode != 2 || exit.Error != "Unknown option: --approve" {
			t.Errorf("exit = %+v, want code 2 with stderr", exit)
		}
	})

	t.Run("prompt command parses to UserInputMessage", func(t *testing.T) {
		t.Parallel()
		parser := New("", nil).NewWire().ParseMessage
		line := []byte(`{"type":"prompt","message":"run rpi/446b-camera/build-annotate-cv.sh without docker","streamingBehavior":"steer"}`)
		msgs, err := parser(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("got %T, want UserInputMessage", msgs[0])
		}
		want := "run rpi/446b-camera/build-annotate-cv.sh without docker"
		if ui.Text != want {
			t.Errorf("text: got %q, want %q", ui.Text, want)
		}
	})
}

func TestHandleDone(t *testing.T) {
	t.Parallel()

	// handleDone is kept for future Pi protocol versions that may emit done
	// deltas. The current Pi does not emit them, but the code path is tested.

	t.Run("done emits empty ResultMessage", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"end_turn"}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("ParseMessage(done) = %d messages, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
	})

	t.Run("done with error reason marks ResultMessage", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"error"}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
		if !rm.IsError {
			t.Error("IsError = false, want true when done delta had reason=error")
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
	})

	t.Run("done does not copy message content into Result", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"end_turn","message":{"role":"assistant","content":[{"type":"text","text":"Non-streamed result"}]}}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("ParseMessage(done) = %d messages, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
	})
}

func TestHandleAgentEnd(t *testing.T) {
	t.Parallel()
	t.Run("agent_end emits usage without result text", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		agentEndLine := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[],"usage":{"input":100,"output":50}}]}`)
		msgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("ParseMessage(agent_end) = %d messages, want 1", len(msgs))
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
		if rm.Usage.InputTokens != 100 || rm.Usage.OutputTokens != 50 {
			t.Errorf("Usage = %+v, want input=100 output=50", rm.Usage)
		}
	})

	t.Run("agent_end does not copy message content into Result", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		agentEndLine := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"Fallback text"}],"usage":{"input":10,"output":5}}]}`)
		msgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
	})

	t.Run("agent_end without assistant message has empty Result", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		agentEndLine := []byte(`{"type":"agent_end","messages":[]}`)
		msgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "" {
			t.Errorf("Result = %q, want empty", rm.Result)
		}
	})
}

func TestCaicModelInfo(t *testing.T) {
	t.Parallel()
	// Verify caic_model_info sets modelCtxWindow on the wire format,
	// which is then used by handleTurnEnd to emit the correct ContextWindow
	// in UsageMessage.

	t.Run("sets modelCtxWindow from caic_model_info line", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		// Parse the synthetic caic_model_info line.
		infoLine := []byte(`{"type":"caic_model_info","context_window":1000000}`)
		msgs, err := w.ParseMessage(infoLine)
		if err != nil {
			t.Fatalf("ParseMessage(caic_model_info): %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("expected 0 messages, got %d", len(msgs))
		}
		if w.modelCtxWindow != 1000000 {
			t.Errorf("modelCtxWindow = %d, want 1000000", w.modelCtxWindow)
		}
	})

	t.Run("turn_end after caic_model_info has correct ContextWindow", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		// Simulate replay order: caic_model_info first, then turn_end.
		if _, err := w.ParseMessage([]byte(`{"type":"caic_model_info","context_window":1000000}`)); err != nil {
			t.Fatal(err)
		}
		turnEndLine := []byte(`{"type":"turn_end","message":{"role":"assistant","content":[],"usage":{"input":100,"output":50,"totalTokens":150}}}`)
		msgs, err := w.ParseMessage(turnEndLine)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		um, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("expected *agent.UsageMessage, got %T", msgs[0])
		}
		if um.ContextWindow != 1000000 {
			t.Errorf("ContextWindow = %d, want 1000000", um.ContextWindow)
		}
	})

	t.Run("turn_end without caic_model_info defaults to 0", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		turnEndLine := []byte(`{"type":"turn_end","message":{"role":"assistant","content":[],"usage":{"input":100,"output":50,"totalTokens":150}}}`)
		msgs, err := w.ParseMessage(turnEndLine)
		if err != nil {
			t.Fatal(err)
		}
		um, ok := msgs[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("expected *agent.UsageMessage, got %T", msgs[0])
		}
		if um.ContextWindow != 0 {
			t.Errorf("ContextWindow = %d, want 0 (no caic_model_info)", um.ContextWindow)
		}
	})

	t.Run("caic_model_info with zero context_window is ignored", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		if _, err := w.ParseMessage([]byte(`{"type":"caic_model_info","context_window":0}`)); err != nil {
			t.Fatal(err)
		}
		if w.modelCtxWindow != 0 {
			t.Errorf("modelCtxWindow = %d, want 0 (zero should be ignored)", w.modelCtxWindow)
		}
	})
}

func TestWireFormatDurationTracking(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		w.mu.Lock()
		w.startTime = time.Now().Add(-500 * time.Millisecond)
		w.mu.Unlock()

		turnEndLine := []byte(`{"type":"turn_end","message":{"role":"assistant","content":[],"usage":{"input":100,"output":50,"totalTokens":150}}}`)
		if _, err := w.ParseMessage(turnEndLine); err != nil {
			t.Fatalf("ParseMessage(turn_end): %v", err)
		}

		agentEndLine := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[],"usage":{"input":100,"output":50}}]}`)
		endMsgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		if len(endMsgs) != 1 {
			t.Fatalf("ParseMessage(agent_end) = %d messages, want 1", len(endMsgs))
		}
		rm, ok := endMsgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", endMsgs[0])
		}
		if rm.DurationMs < 400 || rm.DurationMs > 700 {
			t.Errorf("DurationMs = %d, want ~500", rm.DurationMs)
		}
		if rm.NumTurns != 1 {
			t.Errorf("NumTurns = %d, want 1", rm.NumTurns)
		}
		if rm.Usage.InputTokens != 100 || rm.Usage.OutputTokens != 50 {
			t.Errorf("Usage = %+v, want input=100 output=50", rm.Usage)
		}
	})

	t.Run("no WritePrompt before agent_end", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		agentEndLine := []byte(`{"type":"agent_end","messages":[]}`)
		endMsgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		rm, ok := endMsgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", endMsgs[0])
		}
		if rm.DurationMs != 0 {
			t.Errorf("DurationMs = %d, want 0 when startTime was never set", rm.DurationMs)
		}
	})

	t.Run("WritePrompt resets state between turns", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}

		w.mu.Lock()
		w.startTime = time.Now().Add(-200 * time.Millisecond)
		w.mu.Unlock()
		turnEnd := []byte(`{"type":"turn_end","message":{"role":"assistant","content":[],"usage":{"input":10,"output":5,"totalTokens":15}}}`)
		if _, err := w.ParseMessage(turnEnd); err != nil {
			t.Fatal(err)
		}
		agentEnd := []byte(`{"type":"agent_end","messages":[]}`)
		if _, err := w.ParseMessage(agentEnd); err != nil {
			t.Fatal(err)
		}

		w.mu.Lock()
		w.startTime = time.Now().Add(-300 * time.Millisecond)
		w.numTurns = 0
		w.mu.Unlock()
		if _, err := w.ParseMessage(turnEnd); err != nil {
			t.Fatal(err)
		}
		if _, err := w.ParseMessage(turnEnd); err != nil {
			t.Fatal(err)
		}

		endMsgs, err := w.ParseMessage(agentEnd)
		if err != nil {
			t.Fatal(err)
		}
		rm, ok := endMsgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *agent.ResultMessage", endMsgs[0])
		}
		if rm.DurationMs < 200 || rm.DurationMs > 500 {
			t.Errorf("DurationMs = %d, want ~300 (second turn)", rm.DurationMs)
		}
		if rm.NumTurns != 2 {
			t.Errorf("NumTurns = %d, want 2", rm.NumTurns)
		}
	})
}

func TestParsePromptCmd(t *testing.T) {
	t.Parallel()

	t.Run("text prompt", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"prompt","message":"fix the bug"}`)
		msgs, err := parseMessage(line, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "fix the bug" {
			t.Errorf("Text = %q, want %q", ui.Text, "fix the bug")
		}
	})

	t.Run("prompt with images", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"prompt","message":"describe this","images":[{"type":"image","mimeType":"image/png","data":"abc123"}]}`)
		msgs, err := parseMessage(line, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		ui, ok := msgs[0].(*agent.UserInputMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.UserInputMessage", msgs[0])
		}
		if ui.Text != "describe this" {
			t.Errorf("Text = %q, want %q", ui.Text, "describe this")
		}
		if len(ui.Images) != 1 {
			t.Fatalf("got %d images, want 1", len(ui.Images))
		}
		if ui.Images[0].MediaType != "image/png" {
			t.Errorf("MediaType = %q, want %q", ui.Images[0].MediaType, "image/png")
		}
		if ui.Images[0].Data != "abc123" {
			t.Errorf("Data = %q, want %q", ui.Images[0].Data, "abc123")
		}
	})

	t.Run("compact command is skipped", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"compact","customInstructions":"summarize"}`)
		msgs, err := parseMessage(line, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("got %d messages, want 0 (compact skipped)", len(msgs))
		}
	})
}
