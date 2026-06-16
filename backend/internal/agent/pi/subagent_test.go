// Tests for Pi subagent tool parsing and lifecycle message emission.

package pi

import (
	"encoding/json"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseSubagentArgs(t *testing.T) {
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
}

func TestSubagentDescription(t *testing.T) {
	t.Parallel()
	t.Run("single with label", func(t *testing.T) {
		t.Parallel()
		got := subagentDescription("single", []subagentSpawn{{Agent: "reviewer", Label: "security pass", Task: "ignored"}})
		if got != "reviewer — security pass" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("single falls back to task first line", func(t *testing.T) {
		t.Parallel()
		got := subagentDescription("single", []subagentSpawn{{Agent: "planner", Task: "\n  Plan the work  \nmore"}})
		if got != "planner — Plan the work" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("multiple counts by agent", func(t *testing.T) {
		t.Parallel()
		got := subagentDescription("chain", []subagentSpawn{{Agent: "reviewer"}, {Agent: "reviewer"}, {Agent: "reviewer"}, {Agent: "worker"}})
		if got != "chain · reviewer ×3, worker" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestSubagentStatus(t *testing.T) {
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
}

func TestParseToolExecStart(t *testing.T) {
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
}

func TestParseToolExecEnd(t *testing.T) {
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
}

func TestParseToolExecUpdate(t *testing.T) {
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
}

// TestWireFormatSubagent drives a full subagent tool call through the stateful
// wire format to verify the running-placeholder is suppressed and the success
// result is surfaced whole (not truncated by the incremental output accounting).
func TestWireFormatSubagent(t *testing.T) {
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
}

// Ensure marshaling round-trips for the args shapes the parser must accept.
func TestSubagentArgsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"agent":"reviewer","task":"x"}`,
		`{"tasks":[{"agent":"a","task":"t"}]}`,
		`{"chain":[{"parallel":[{"agent":"a","task":"t"}]}]}`,
	} {
		var args subagentArgs
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if len(args.spawns()) == 0 {
			t.Fatalf("no spawns for %s", raw)
		}
	}
}
