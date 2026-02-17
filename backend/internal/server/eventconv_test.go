package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/server/dto"
)

func TestConvertSystemInit(t *testing.T) {
	tt := newToolTimingTracker()
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
	events := tt.convertMessage(msg, now)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.ClaudeEventKindInit {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindInit)
	}
	if ev.Ts != now.UnixMilli() {
		t.Errorf("ts = %d, want %d", ev.Ts, now.UnixMilli())
	}
	if ev.Init == nil {
		t.Fatal("init payload is nil")
	}
	if ev.Init.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want %q", ev.Init.Model, "claude-opus-4-6")
	}
	if ev.Init.AgentVersion != "2.1.34" {
		t.Errorf("version = %q, want %q", ev.Init.AgentVersion, "2.1.34")
	}
	if len(ev.Init.Tools) != 2 {
		t.Errorf("tools = %v, want 2 items", ev.Init.Tools)
	}
}

func TestConvertSystemMessage(t *testing.T) {
	tt := newToolTimingTracker()
	msg := &agent.SystemMessage{
		MessageType: "system",
		Subtype:     "status",
	}
	events := tt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != dto.ClaudeEventKindSystem {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.ClaudeEventKindSystem)
	}
	if events[0].System.Subtype != "status" {
		t.Errorf("subtype = %q, want %q", events[0].System.Subtype, "status")
	}
}

func TestConvertAssistantTextAndToolUse(t *testing.T) {
	tt := newToolTimingTracker()
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Model: "claude-opus-4-6",
			Content: []agent.ContentBlock{
				{Type: "text", Text: "hello world"},
				{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			},
			Usage: agent.Usage{InputTokens: 100, OutputTokens: 50},
		},
	}
	events := tt.convertMessage(msg, time.Now())
	// Expect: text + toolUse + usage = 3 events.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Kind != dto.ClaudeEventKindText {
		t.Errorf("event[0].kind = %q, want %q", events[0].Kind, dto.ClaudeEventKindText)
	}
	if events[0].Text.Text != "hello world" {
		t.Errorf("text = %q, want %q", events[0].Text.Text, "hello world")
	}
	if events[1].Kind != dto.ClaudeEventKindToolUse {
		t.Errorf("event[1].kind = %q, want %q", events[1].Kind, dto.ClaudeEventKindToolUse)
	}
	if events[1].ToolUse.Name != "Bash" {
		t.Errorf("tool name = %q, want %q", events[1].ToolUse.Name, "Bash")
	}
	if events[1].ToolUse.ToolUseID != "tool_1" {
		t.Errorf("toolUseID = %q, want %q", events[1].ToolUse.ToolUseID, "tool_1")
	}
	if events[2].Kind != dto.ClaudeEventKindUsage {
		t.Errorf("event[2].kind = %q, want %q", events[2].Kind, dto.ClaudeEventKindUsage)
	}
	if events[2].Usage.InputTokens != 100 {
		t.Errorf("inputTokens = %d, want 100", events[2].Usage.InputTokens)
	}
	if events[2].Usage.OutputTokens != 50 {
		t.Errorf("outputTokens = %d, want 50", events[2].Usage.OutputTokens)
	}
}

func TestConvertAskUserQuestion(t *testing.T) {
	tt := newToolTimingTracker()
	askInput := `{"questions":[{"question":"Which approach?","header":"Approach","options":[{"label":"A","description":"First"},{"label":"B"}],"multiSelect":false}]}`
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "ask_1", Name: "AskUserQuestion", Input: json.RawMessage(askInput)},
			},
		},
	}
	events := tt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.ClaudeEventKindAsk {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindAsk)
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
	q := ev.Ask.Questions[0]
	if q.Question != "Which approach?" {
		t.Errorf("question = %q", q.Question)
	}
	if len(q.Options) != 2 {
		t.Errorf("options = %d, want 2", len(q.Options))
	}
}

func TestToolTiming(t *testing.T) {
	tt := newToolTimingTracker()
	t0 := time.Now()
	t1 := t0.Add(500 * time.Millisecond)

	// Send tool_use at t0.
	assistant := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: json.RawMessage(`{}`)},
			},
		},
	}
	tt.convertMessage(assistant, t0)

	// Send tool result at t1.
	parentID := "tool_1"
	user := &agent.UserMessage{
		MessageType:     "user",
		ParentToolUseID: &parentID,
		Message:         json.RawMessage(`{}`),
	}
	events := tt.convertMessage(user, t1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ToolResult.DurationMs != 500 {
		t.Errorf("durationMs = %d, want 500", events[0].ToolResult.DurationMs)
	}
	if events[0].ToolResult.ToolUseID != "tool_1" {
		t.Errorf("toolUseID = %q, want %q", events[0].ToolResult.ToolUseID, "tool_1")
	}
}

func TestToolTimingUnknownID(t *testing.T) {
	tt := newToolTimingTracker()
	parentID := "unknown_id"
	user := &agent.UserMessage{
		MessageType:     "user",
		ParentToolUseID: &parentID,
		Message:         json.RawMessage(`{}`),
	}
	events := tt.convertMessage(user, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ToolResult.DurationMs != 0 {
		t.Errorf("durationMs = %d, want 0 for unknown ID", events[0].ToolResult.DurationMs)
	}
}

func TestConvertRawMessageFiltered(t *testing.T) {
	tt := newToolTimingTracker()
	msg := &agent.RawMessage{
		MessageType: "tool_progress",
		Raw:         []byte(`{"type":"tool_progress","data":"progress"}`),
	}
	events := tt.convertMessage(msg, time.Now())
	if events != nil {
		t.Errorf("got %d events for RawMessage, want nil", len(events))
	}
}

func TestConvertStreamEvent(t *testing.T) {
	t.Run("TextDelta", func(t *testing.T) {
		tt := newToolTimingTracker()
		now := time.Now()
		msg := &agent.StreamEvent{
			MessageType: "stream_event",
			Event: agent.StreamEventData{
				Type:  "content_block_delta",
				Index: 0,
				Delta: &agent.StreamDelta{Type: "text_delta", Text: "Hello"},
			},
		}
		events := tt.convertMessage(msg, now)
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		ev := events[0]
		if ev.Kind != dto.ClaudeEventKindTextDelta {
			t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindTextDelta)
		}
		if ev.TextDelta == nil {
			t.Fatal("textDelta payload is nil")
		}
		if ev.TextDelta.Text != "Hello" {
			t.Errorf("text = %q, want %q", ev.TextDelta.Text, "Hello")
		}
		if ev.Ts != now.UnixMilli() {
			t.Errorf("ts = %d, want %d", ev.Ts, now.UnixMilli())
		}
	})

	filtered := []struct {
		name string
		msg  *agent.StreamEvent
	}{
		{
			name: "MessageStart",
			msg: &agent.StreamEvent{
				MessageType: "stream_event",
				Event:       agent.StreamEventData{Type: "message_start"},
			},
		},
		{
			name: "ContentBlockStart",
			msg: &agent.StreamEvent{
				MessageType: "stream_event",
				Event:       agent.StreamEventData{Type: "content_block_start", Index: 0},
			},
		},
		{
			name: "InputJsonDelta",
			msg: &agent.StreamEvent{
				MessageType: "stream_event",
				Event: agent.StreamEventData{
					Type:  "content_block_delta",
					Index: 1,
					Delta: &agent.StreamDelta{Type: "input_json_delta"},
				},
			},
		},
		{
			name: "EmptyTextDelta",
			msg: &agent.StreamEvent{
				MessageType: "stream_event",
				Event: agent.StreamEventData{
					Type:  "content_block_delta",
					Index: 0,
					Delta: &agent.StreamDelta{Type: "text_delta", Text: ""},
				},
			},
		},
	}
	for _, tc := range filtered {
		t.Run(tc.name, func(t *testing.T) {
			tt := newToolTimingTracker()
			events := tt.convertMessage(tc.msg, time.Now())
			if events != nil {
				t.Errorf("got %d events, want nil", len(events))
			}
		})
	}
}

func TestConvertTodoWrite(t *testing.T) {
	tt := newToolTimingTracker()
	todoInput := `{"todos":[{"content":"Fix bug","status":"in_progress","activeForm":"Fixing bug"},{"content":"Write tests","status":"pending","activeForm":"Writing tests"}]}`
	msg := &agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Content: []agent.ContentBlock{
				{Type: "tool_use", ID: "todo_1", Name: "TodoWrite", Input: json.RawMessage(todoInput)},
			},
		},
	}
	events := tt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.ClaudeEventKindTodo {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindTodo)
	}
	if ev.Todo == nil {
		t.Fatal("todo payload is nil")
	}
	if ev.Todo.ToolUseID != "todo_1" {
		t.Errorf("toolUseID = %q, want %q", ev.Todo.ToolUseID, "todo_1")
	}
	if len(ev.Todo.Todos) != 2 {
		t.Fatalf("todos = %d, want 2", len(ev.Todo.Todos))
	}
	if ev.Todo.Todos[0].Content != "Fix bug" {
		t.Errorf("todos[0].content = %q, want %q", ev.Todo.Todos[0].Content, "Fix bug")
	}
	if ev.Todo.Todos[0].Status != "in_progress" {
		t.Errorf("todos[0].status = %q, want %q", ev.Todo.Todos[0].Status, "in_progress")
	}
	if ev.Todo.Todos[1].Status != "pending" {
		t.Errorf("todos[1].status = %q, want %q", ev.Todo.Todos[1].Status, "pending")
	}
}

func TestConvertResult(t *testing.T) {
	tt := newToolTimingTracker()
	msg := &agent.ResultMessage{
		MessageType:   "result",
		Subtype:       "success",
		IsError:       false,
		Result:        "done",
		DiffStat:      agent.DiffStat{{Path: "a.go", Added: 10, Deleted: 3}},
		TotalCostUSD:  0.05,
		DurationMs:    1234,
		DurationAPIMs: 1200,
		NumTurns:      3,
		Usage:         agent.Usage{InputTokens: 100, OutputTokens: 50, ServiceTier: "default"},
	}
	events := tt.convertMessage(msg, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.ClaudeEventKindResult {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindResult)
	}
	r := ev.Result
	if len(r.DiffStat) != 1 || r.DiffStat[0].Path != "a.go" {
		t.Errorf("diffStat = %+v, want file a.go", r.DiffStat)
	}
	if r.TotalCostUSD != 0.05 {
		t.Errorf("cost = %f, want 0.05", r.TotalCostUSD)
	}
	if r.NumTurns != 3 {
		t.Errorf("turns = %d, want 3", r.NumTurns)
	}
	if r.Usage.InputTokens != 100 {
		t.Errorf("inputTokens = %d, want 100", r.Usage.InputTokens)
	}
}

func TestExtractToolError(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"command not found"}],"is_error":true}`)
	err := extractToolError(raw)
	if err != "command not found" {
		t.Errorf("error = %q, want %q", err, "command not found")
	}

	raw = json.RawMessage(`{"content":[{"type":"text","text":"success"}],"is_error":false}`)
	err = extractToolError(raw)
	if err != "" {
		t.Errorf("error = %q, want empty for non-error", err)
	}
}

func TestConvertUserNoParentID_Empty(t *testing.T) {
	tt := newToolTimingTracker()
	user := &agent.UserMessage{
		MessageType: "user",
		Message:     json.RawMessage(`{}`),
	}
	events := tt.convertMessage(user, time.Now())
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0 for empty user message", len(events))
	}
}

func TestConvertUserInput(t *testing.T) {
	tt := newToolTimingTracker()
	user := &agent.UserMessage{
		MessageType: "user",
		Message:     json.RawMessage(`{"role":"user","content":"hello agent"}`),
	}
	events := tt.convertMessage(user, time.Now())
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != dto.ClaudeEventKindUserInput {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.ClaudeEventKindUserInput)
	}
	if ev.UserInput == nil {
		t.Fatal("userInput payload is nil")
	}
	if ev.UserInput.Text != "hello agent" {
		t.Errorf("text = %q, want %q", ev.UserInput.Text, "hello agent")
	}
}
