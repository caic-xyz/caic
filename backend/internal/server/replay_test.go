// Tests for task SSE history replay filtering.

package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
)

func TestNewReplayFilter(t *testing.T) {
	t.Parallel()
	// eventreplay.NewFilter is the streaming form of filterHistoryForReplay; it
	// must produce the same surviving messages, in order, for any sequence.
	td := func() agent.Message { return &agent.TextDeltaMessage{} }
	tf := func() agent.Message { return &agent.TextMessage{} }
	hd := func() agent.Message { return &agent.ThinkingDeltaMessage{} }
	hf := func() agent.Message { return &agent.ThinkingMessage{} }
	tool := func() agent.Message { return &agent.ToolUseMessage{} }
	tod := func() agent.Message { return &agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "x"} }
	tr := func() agent.Message { return &agent.ToolResultMessage{ToolUseID: "t1"} }

	cases := map[string][]agent.Message{
		"deltas then final":              {td(), td(), tf()},
		"deltas no final":                {td(), td()},
		"thinking run":                   {hd(), hf()},
		"mixed delta kinds":              {td(), hd(), hf()},
		"tool breaks run":                {td(), tool(), tf()},
		"final without deltas":           {tf()},
		"empty":                          {},
		"trailing run then text":         {tf(), td(), td()},
		"tool output deltas then result": {tod(), tod(), tr()},
		"tool output deltas no result":   {tod(), tod()},
		"tool result without deltas":     {tr()},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := filterHistoryForReplay(in)
			var got []agent.Message
			push, flush := eventreplay.NewFilter(func(m agent.Message) { got = append(got, m) })
			for _, m := range in {
				push(m)
			}
			flush()
			if len(got) != len(want) {
				t.Fatalf("got %d messages, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("message %d: got %T, want %T", i, got[i], want[i])
				}
			}
		})
	}
}

func TestGenericConvertInitHasHarness(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.InitMessage{
		Model:     "claude-opus-4-6",
		Version:   "2.1.34",
		SessionID: "sess-1",
		Tools:     []string{"Bash", "Read"},
		Cwd:       "/home/user",
	}
	now := time.Now()
	events := gt.ConvertMessage(msg, now)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != v1.EventKindInit {
		t.Errorf("kind = %q, want %q", ev.Kind, v1.EventKindInit)
	}
	if ev.Init == nil {
		t.Fatal("init payload is nil")
	}
	if ev.Init.Harness != "claude" {
		t.Errorf("harness = %q, want %q", ev.Init.Harness, "claude")
	}
	if ev.Init.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want %q", ev.Init.Model, "claude-opus-4-6")
	}
	if ev.Init.AgentVersion != "2.1.34" {
		t.Errorf("version = %q, want %q", ev.Init.AgentVersion, "2.1.34")
	}
}

func TestGenericAskUserQuestionIsAsk(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.AskMessage{
		ToolUseID: "ask_1",
		Questions: []agent.AskQuestion{
			{
				Question: "Which approach?",
				Header:   "Approach",
				Options:  []agent.AskOption{{Label: "A"}, {Label: "B"}},
			},
		},
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != v1.EventKindAsk {
		t.Errorf("kind = %q, want %q", ev.Kind, v1.EventKindAsk)
	}
	if ev.Ask == nil {
		t.Fatal("ask payload is nil")
	}
	if ev.Ask.ToolUseID != "ask_1" {
		t.Errorf("toolUseID = %q, want %q", ev.Ask.ToolUseID, "ask_1")
	}
	if len(ev.Ask.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(ev.Ask.Questions))
	}
	if ev.Ask.Questions[0].Question != "Which approach?" {
		t.Errorf("question = %q", ev.Ask.Questions[0].Question)
	}
}

func TestGenericTodoWriteIsTodo(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.TodoMessage{
		ToolUseID: "todo_1",
		Todos: []agent.TodoItem{
			{Content: "Fix bug", Status: "in_progress", ActiveForm: "Fixing bug"},
		},
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != v1.EventKindTodo {
		t.Errorf("kind = %q, want %q", ev.Kind, v1.EventKindTodo)
	}
	if ev.Todo == nil {
		t.Fatal("todo payload is nil")
	}
	if ev.Todo.ToolUseID != "todo_1" {
		t.Errorf("toolUseID = %q, want %q", ev.Todo.ToolUseID, "todo_1")
	}
	if len(ev.Todo.Todos) != 1 {
		t.Fatalf("todos = %d, want 1", len(ev.Todo.Todos))
	}
	if ev.Todo.Todos[0].Content != "Fix bug" {
		t.Errorf("content = %q, want %q", ev.Todo.Todos[0].Content, "Fix bug")
	}
}

func TestGenericToolTiming(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	t0 := time.Now()
	t1 := t0.Add(500 * time.Millisecond)

	toolUse := &agent.ToolUseMessage{
		ToolUseID: "tool_1",
		Name:      "Bash",
		Input:     json.RawMessage(`{}`),
	}
	gt.ConvertMessage(toolUse, t0)

	toolResult := &agent.ToolResultMessage{
		ToolUseID: "tool_1",
	}
	events := gt.ConvertMessage(toolResult, t1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ToolResult.Duration != 0.5 {
		t.Errorf("duration = %f, want 0.5", events[0].ToolResult.Duration)
	}
}

func TestGenericConvertTextAndUsage(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Codex, FormatToolOutput)

	textMsg := &agent.TextMessage{Text: "hello"}
	usageMsg := &agent.UsageMessage{
		Usage: agent.Usage{InputTokens: 200, OutputTokens: 100},
		Model: "gemini-2.5-pro",
	}

	now := time.Now()
	events := make([]v1.EventMessage, 0, 2)
	events = append(events, gt.ConvertMessage(textMsg, now)...)
	events = append(events, gt.ConvertMessage(usageMsg, now)...)

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Kind != v1.EventKindText {
		t.Errorf("event[0].kind = %q, want %q", events[0].Kind, v1.EventKindText)
	}
	if events[1].Kind != v1.EventKindUsage {
		t.Errorf("event[1].kind = %q, want %q", events[1].Kind, v1.EventKindUsage)
	}
	if events[1].Usage.Model != "gemini-2.5-pro" {
		t.Errorf("model = %q, want %q", events[1].Usage.Model, "gemini-2.5-pro")
	}
}

func TestGenericConvertResult(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.ResultMessage{
		MessageType:  "result",
		Subtype:      "success",
		Result:       "done",
		DiffStat:     agent.DiffStat{{Path: "a.go", Added: 10, Deleted: 3}},
		TotalCostUSD: 0.05,
		NumTurns:     3,
		Usage:        agent.Usage{InputTokens: 100, OutputTokens: 50},
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindResult {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindResult)
	}
	if events[0].Result.NumTurns != 3 {
		t.Errorf("numTurns = %d, want 3", events[0].Result.NumTurns)
	}
}

func TestGenericConvertStreamEvent(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.TextDeltaMessage{Text: "Hi"}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindTextDelta {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindTextDelta)
	}
	if events[0].TextDelta.Text != "Hi" {
		t.Errorf("text = %q, want %q", events[0].TextDelta.Text, "Hi")
	}
}

func TestGenericConvertUserInput(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.UserInputMessage{
		Text: "hello agent",
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindUserInput {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindUserInput)
	}
	if events[0].UserInput.Text != "hello agent" {
		t.Errorf("text = %q, want %q", events[0].UserInput.Text, "hello agent")
	}
}

func TestGenericConvertSystemMessage(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.SystemMessage{
		MessageType: "system",
		Subtype:     "status",
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindSystem {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindSystem)
	}
}

func TestGenericConvertThinking(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.ThinkingMessage{Text: "let me think..."}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindThinking {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindThinking)
	}
	if events[0].Thinking == nil {
		t.Fatal("thinking payload is nil")
	}
	if events[0].Thinking.Text != "let me think..." {
		t.Errorf("text = %q, want %q", events[0].Thinking.Text, "let me think...")
	}
}

func TestGenericConvertSubagentEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		msg   agent.Message
		kind  v1.EventKind
		check func(t *testing.T, ev v1.EventMessage)
	}{
		{
			name: "start",
			msg:  &agent.SubagentStartMessage{TaskID: "task-1", Description: "Explore code"},
			kind: v1.EventKindSubagentStart,
			check: func(t *testing.T, ev v1.EventMessage) {
				if ev.SubagentStart == nil {
					t.Fatal("subagentStart payload is nil")
				}
				if ev.SubagentStart.TaskID != "task-1" {
					t.Errorf("taskID = %q, want %q", ev.SubagentStart.TaskID, "task-1")
				}
				if ev.SubagentStart.Description != "Explore code" {
					t.Errorf("description = %q, want %q", ev.SubagentStart.Description, "Explore code")
				}
			},
		},
		{
			name: "end",
			msg:  &agent.SubagentEndMessage{TaskID: "task-1", Status: "completed"},
			kind: v1.EventKindSubagentEnd,
			check: func(t *testing.T, ev v1.EventMessage) {
				if ev.SubagentEnd == nil {
					t.Fatal("subagentEnd payload is nil")
				}
				if ev.SubagentEnd.TaskID != "task-1" {
					t.Errorf("taskID = %q, want %q", ev.SubagentEnd.TaskID, "task-1")
				}
				if ev.SubagentEnd.Status != "completed" {
					t.Errorf("status = %q, want %q", ev.SubagentEnd.Status, "completed")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
			events := gt.ConvertMessage(tt.msg, time.Now())
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			if events[0].Kind != tt.kind {
				t.Errorf("kind = %q, want %q", events[0].Kind, tt.kind)
			}
			tt.check(t, events[0])
		})
	}
}

func TestGenericConvertRawMessageFiltered(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.RawMessage{
		MessageType: "tool_progress",
		Raw:         []byte(`{"type":"tool_progress"}`),
	}
	events := gt.ConvertMessage(msg, time.Now())
	if events != nil {
		t.Errorf("got %d events for RawMessage, want nil", len(events))
	}
}

func TestToolInputTruncation(t *testing.T) {
	t.Parallel()
	t.Run("SmallInputPassedThrough", func(t *testing.T) {
		t.Parallel()
		gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
		msg := &agent.ToolUseMessage{ToolUseID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/etc/hosts"}`)}
		events := gt.ConvertMessage(msg, time.Now())
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].ToolUse.InputTruncated {
			t.Error("small input should not be truncated")
		}
		if events[0].ToolUse.Input == nil {
			t.Error("small input should be present")
		}
	})
	t.Run("LargeInputTruncated", func(t *testing.T) {
		t.Parallel()
		gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
		largeContent := make([]byte, v1conv.InputTruncateThreshold+1)
		for i := range largeContent {
			largeContent[i] = 'x'
		}
		bigInput := json.RawMessage(`{"content":"` + string(largeContent) + `"}`)
		msg := &agent.ToolUseMessage{ToolUseID: "t2", Name: "Write", Input: bigInput}
		events := gt.ConvertMessage(msg, time.Now())
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if !events[0].ToolUse.InputTruncated {
			t.Error("large input should be truncated")
		}
		if events[0].ToolUse.Input != nil {
			t.Error("truncated input should be nil")
		}
	})
	t.Run("BackgroundTrue", func(t *testing.T) {
		t.Parallel()
		gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
		msg := &agent.ToolUseMessage{ToolUseID: "t3", Name: "Bash", Input: json.RawMessage(`{"command":"sleep 60","run_in_background":true}`)}
		events := gt.ConvertMessage(msg, time.Now())
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if !events[0].ToolUse.Background {
			t.Error("background should be true")
		}
	})
	t.Run("BackgroundAbsent", func(t *testing.T) {
		t.Parallel()
		gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
		msg := &agent.ToolUseMessage{ToolUseID: "t4", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}
		events := gt.ConvertMessage(msg, time.Now())
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].ToolUse.Background {
			t.Error("background should be false when absent")
		}
	})
	t.Run("BackgroundAgentTool", func(t *testing.T) {
		t.Parallel()
		gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
		msg := &agent.ToolUseMessage{ToolUseID: "t5", Name: "Agent", Input: json.RawMessage(`{"prompt":"search","run_in_background":true}`)}
		events := gt.ConvertMessage(msg, time.Now())
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if !events[0].ToolUse.Background {
			t.Error("background should be true for Agent tool")
		}
	})
}

func TestGenericConvertWidget(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.WidgetMessage{
		ToolUseID: "wid_1",
		Title:     "Chart",
		HTML:      "<canvas>chart</canvas>",
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != v1.EventKindWidget {
		t.Errorf("kind = %q, want %q", ev.Kind, v1.EventKindWidget)
	}
	if ev.Widget == nil {
		t.Fatal("widget payload is nil")
	}
	if ev.Widget.ToolUseID != "wid_1" {
		t.Errorf("toolUseID = %q, want %q", ev.Widget.ToolUseID, "wid_1")
	}
	if ev.Widget.Title != "Chart" {
		t.Errorf("title = %q, want %q", ev.Widget.Title, "Chart")
	}
	if ev.Widget.HTML != "<canvas>chart</canvas>" {
		t.Errorf("html = %q, want %q", ev.Widget.HTML, "<canvas>chart</canvas>")
	}
}

func TestGenericConvertWidgetDelta(t *testing.T) {
	t.Parallel()
	gt := v1conv.NewToolTimingTracker(harness.Claude, FormatToolOutput)
	msg := &agent.WidgetDeltaMessage{
		ToolUseID: "wid_2",
		Delta:     "<h1>Hel",
	}
	events := gt.ConvertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != v1.EventKindWidgetDelta {
		t.Errorf("kind = %q, want %q", ev.Kind, v1.EventKindWidgetDelta)
	}
	if ev.WidgetDelta == nil {
		t.Fatal("widgetDelta payload is nil")
	}
	if ev.WidgetDelta.ToolUseID != "wid_2" {
		t.Errorf("toolUseID = %q, want %q", ev.WidgetDelta.ToolUseID, "wid_2")
	}
	if ev.WidgetDelta.Delta != "<h1>Hel" {
		t.Errorf("delta = %q, want %q", ev.WidgetDelta.Delta, "<h1>Hel")
	}
}

func TestFilterHistoryForReplay(t *testing.T) {
	t.Parallel()
	t.Run("RemovesTextDeltasBeforeText", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.TextDeltaMessage{Text: "hel"},
			&agent.TextDeltaMessage{Text: "lo"},
			&agent.TextMessage{Text: "hello"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if _, ok := got[0].(*agent.TextMessage); !ok {
			t.Errorf("expected TextMessage, got %T", got[0])
		}
	})
	t.Run("RemovesThinkingDeltasBeforeThinking", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.ThinkingDeltaMessage{Text: "think..."},
			&agent.ThinkingMessage{Text: "think...done"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if _, ok := got[0].(*agent.ThinkingMessage); !ok {
			t.Errorf("expected ThinkingMessage, got %T", got[0])
		}
	})
	t.Run("KeepsDeltasWithoutFinalMessage", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.TextDeltaMessage{Text: "hel"},
			&agent.TextDeltaMessage{Text: "lo"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
	})
	t.Run("PreservesOtherMessages", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.ToolUseMessage{ToolUseID: "t1", Name: "Read", Input: json.RawMessage(`{}`)},
			&agent.TextDeltaMessage{Text: "hi"},
			&agent.TextMessage{Text: "hi"},
			&agent.ToolResultMessage{ToolUseID: "t1"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 3 {
			t.Fatalf("got %d messages, want 3", len(got))
		}
		if _, ok := got[0].(*agent.ToolUseMessage); !ok {
			t.Errorf("[0] expected ToolUseMessage, got %T", got[0])
		}
		if _, ok := got[1].(*agent.TextMessage); !ok {
			t.Errorf("[1] expected TextMessage, got %T", got[1])
		}
		if _, ok := got[2].(*agent.ToolResultMessage); !ok {
			t.Errorf("[2] expected ToolResultMessage, got %T", got[2])
		}
	})
	t.Run("RemovesWidgetDeltasBeforeWidget", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.WidgetDeltaMessage{ToolUseID: "w1", Delta: "<h1>"},
			&agent.WidgetDeltaMessage{ToolUseID: "w1", Delta: "Hi</h1>"},
			&agent.WidgetMessage{ToolUseID: "w1", Title: "Test", HTML: "<h1>Hi</h1>"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if _, ok := got[0].(*agent.WidgetMessage); !ok {
			t.Errorf("expected WidgetMessage, got %T", got[0])
		}
	})
	t.Run("MultipleTextBlocks", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.TextDeltaMessage{Text: "a"},
			&agent.TextMessage{Text: "a"},
			&agent.ToolUseMessage{ToolUseID: "t1", Name: "Bash", Input: json.RawMessage(`{}`)},
			&agent.TextDeltaMessage{Text: "b"},
			&agent.TextMessage{Text: "b"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 3 {
			t.Fatalf("got %d messages, want 3", len(got))
		}
	})

	t.Run("RemovesToolOutputDeltasBeforeToolResult", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "a"},
			&agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "b"},
			&agent.ToolResultMessage{ToolUseID: "t1"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if _, ok := got[0].(*agent.ToolResultMessage); !ok {
			t.Errorf("got %T, want ToolResultMessage", got[0])
		}
	})

	t.Run("KeepsToolOutputDeltasWithoutToolResult", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "a"},
			&agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "b"},
		}
		got := filterHistoryForReplay(msgs)
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
	})

	t.Run("ToolOutputDeltasMatchedByToolUseID", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.ToolOutputDeltaMessage{ToolUseID: "t1", Delta: "a"},
			&agent.ToolOutputDeltaMessage{ToolUseID: "t2", Delta: "b"},
			&agent.ToolResultMessage{ToolUseID: "t1"},
		}
		got := filterHistoryForReplay(msgs)
		// Only consecutive deltas immediately before the result are removed.
		// The t2 delta breaks the run, so the t1 delta at index 0 is kept.
		if len(got) != 3 {
			t.Fatalf("got %d messages, want 3", len(got))
		}
	})
}
