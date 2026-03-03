package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

func TestGenericConvertInitHasHarness(t *testing.T) {
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.InitMessage{
		Model:     "claude-opus-4-6",
		Version:   "2.1.34",
		SessionID: "sess-1",
		Tools:     []string{"Bash", "Read"},
		Cwd:       "/home/user",
	}
	now := time.Now()
	events := gt.convertMessage(msg, now)
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
	gt := newToolTimingTracker(agent.Claude)
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
	events := gt.convertMessage(msg, time.Now())
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
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.TodoMessage{
		ToolUseID: "todo_1",
		Todos: []agent.TodoItem{
			{Content: "Fix bug", Status: "in_progress", ActiveForm: "Fixing bug"},
		},
	}
	events := gt.convertMessage(msg, time.Now())
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
	gt := newToolTimingTracker(agent.Claude)
	t0 := time.Now()
	t1 := t0.Add(500 * time.Millisecond)

	toolUse := &agent.ToolUseMessage{
		ToolUseID: "tool_1",
		Name:      "Bash",
		Input:     json.RawMessage(`{}`),
	}
	gt.convertMessage(toolUse, t0)

	toolResult := &agent.ToolResultMessage{
		ToolUseID: "tool_1",
	}
	events := gt.convertMessage(toolResult, t1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ToolResult.Duration != 0.5 {
		t.Errorf("duration = %f, want 0.5", events[0].ToolResult.Duration)
	}
}

func TestGenericConvertTextAndUsage(t *testing.T) {
	gt := newToolTimingTracker(agent.Gemini)

	textMsg := &agent.TextMessage{Text: "hello"}
	usageMsg := &agent.UsageMessage{
		Usage: agent.Usage{InputTokens: 200, OutputTokens: 100},
		Model: "gemini-2.5-pro",
	}

	now := time.Now()
	events := make([]v1.EventMessage, 0, 2)
	events = append(events, gt.convertMessage(textMsg, now)...)
	events = append(events, gt.convertMessage(usageMsg, now)...)

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
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.ResultMessage{
		MessageType:  "result",
		Subtype:      "success",
		Result:       "done",
		DiffStat:     agent.DiffStat{{Path: "a.go", Added: 10, Deleted: 3}},
		TotalCostUSD: 0.05,
		NumTurns:     3,
		Usage:        agent.Usage{InputTokens: 100, OutputTokens: 50},
	}
	events := gt.convertMessage(msg, time.Now())
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
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.TextDeltaMessage{Text: "Hi"}
	events := gt.convertMessage(msg, time.Now())
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
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.UserInputMessage{
		Text: "hello agent",
	}
	events := gt.convertMessage(msg, time.Now())
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
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.SystemMessage{
		MessageType: "system",
		Subtype:     "status",
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != v1.EventKindSystem {
		t.Errorf("kind = %q, want %q", events[0].Kind, v1.EventKindSystem)
	}
}

func TestGenericConvertRawMessageFiltered(t *testing.T) {
	gt := newToolTimingTracker(agent.Claude)
	msg := &agent.RawMessage{
		MessageType: "tool_progress",
		Raw:         []byte(`{"type":"tool_progress"}`),
	}
	events := gt.convertMessage(msg, time.Now())
	if events != nil {
		t.Errorf("got %d events for RawMessage, want nil", len(events))
	}
}
