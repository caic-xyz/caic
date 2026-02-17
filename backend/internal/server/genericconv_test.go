package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/server/dto"
)

func TestGenericConvertInitHasHarness(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
	msg := &agent.SystemInitMessage{
		MessageType: "system",
		Subtype:     "init",
		Model:       "claude-opus-4-6",
		Version:     "2.1.34",
		SessionID:   "sess-1",
		Tools:       []string{"Bash", "Read"},
		Cwd:         "/home/user",
	}
	now := time.Now()
	events := gt.convertMessage(msg, now)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.EventKindInit {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindInit)
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
	gt := newGenericToolTimingTracker(agent.Claude)
	askInput := `{"questions":[{"question":"Which approach?","header":"Approach","options":[{"label":"A"},{"label":"B"}]}]}`
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "ask_1", Name: "AskUserQuestion", Input: json.RawMessage(askInput)},
			},
		},
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.EventKindAsk {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindAsk)
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
	gt := newGenericToolTimingTracker(agent.Claude)
	todoInput := `{"todos":[{"content":"Fix bug","status":"in_progress","activeForm":"Fixing bug"}]}`
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "todo_1", Name: "TodoWrite", Input: json.RawMessage(todoInput)},
			},
		},
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.EventKindTodo {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindTodo)
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
	gt := newGenericToolTimingTracker(agent.Claude)
	t0 := time.Now()
	t1 := t0.Add(500 * time.Millisecond)

	assistant := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: json.RawMessage(`{}`)},
			},
		},
	}
	gt.convertMessage(assistant, t0)

	parentID := "tool_1"
	user := &agent.UserMessage{
		MessageType:     "user",
		ParentToolUseID: &parentID,
		Message:         json.RawMessage(`{}`),
	}
	events := gt.convertMessage(user, t1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ToolResult.DurationMs != 500 {
		t.Errorf("durationMs = %d, want 500", events[0].ToolResult.DurationMs)
	}
}

func TestGenericConvertTextAndUsage(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Gemini)
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Model: "gemini-2.5-pro",
			Content: []agent.ContentBlock{
				{Type: "text", Text: "hello"},
			},
			Usage: agent.Usage{InputTokens: 200, OutputTokens: 100},
		},
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Kind != dto.EventKindText {
		t.Errorf("event[0].kind = %q, want %q", events[0].Kind, dto.EventKindText)
	}
	if events[1].Kind != dto.EventKindUsage {
		t.Errorf("event[1].kind = %q, want %q", events[1].Kind, dto.EventKindUsage)
	}
	if events[1].Usage.Model != "gemini-2.5-pro" {
		t.Errorf("model = %q, want %q", events[1].Usage.Model, "gemini-2.5-pro")
	}
}

func TestGenericConvertResult(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
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
	if events[0].Kind != dto.EventKindResult {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.EventKindResult)
	}
	if events[0].Result.NumTurns != 3 {
		t.Errorf("numTurns = %d, want 3", events[0].Result.NumTurns)
	}
}

func TestGenericConvertStreamEvent(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
	msg := &agent.StreamEvent{
		MessageType: "stream_event",
		Event: agent.StreamEventData{
			Type:  "content_block_delta",
			Index: 0,
			Delta: &agent.StreamDelta{Type: "text_delta", Text: "Hi"},
		},
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != dto.EventKindTextDelta {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.EventKindTextDelta)
	}
	if events[0].TextDelta.Text != "Hi" {
		t.Errorf("text = %q, want %q", events[0].TextDelta.Text, "Hi")
	}
}

func TestGenericConvertUserInput(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
	user := &agent.UserMessage{
		MessageType: "user",
		Message:     json.RawMessage(`{"role":"user","content":"hello agent"}`),
	}
	events := gt.convertMessage(user, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != dto.EventKindUserInput {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.EventKindUserInput)
	}
	if events[0].UserInput.Text != "hello agent" {
		t.Errorf("text = %q, want %q", events[0].UserInput.Text, "hello agent")
	}
}

func TestGenericConvertSystemMessage(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
	msg := &agent.SystemMessage{
		MessageType: "system",
		Subtype:     "status",
	}
	events := gt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != dto.EventKindSystem {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.EventKindSystem)
	}
}

func TestGenericConvertRawMessageFiltered(t *testing.T) {
	gt := newGenericToolTimingTracker(agent.Claude)
	msg := &agent.RawMessage{
		MessageType: "tool_progress",
		Raw:         []byte(`{"type":"tool_progress"}`),
	}
	events := gt.convertMessage(msg, time.Now())
	if events != nil {
		t.Errorf("got %d events for RawMessage, want nil", len(events))
	}
}
