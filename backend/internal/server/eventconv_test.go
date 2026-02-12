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
	if ev.Kind != dto.EventKindInit {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindInit)
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
	if ev.Init.ClaudeCodeVersion != "2.1.34" {
		t.Errorf("version = %q, want %q", ev.Init.ClaudeCodeVersion, "2.1.34")
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
	if events[0].Kind != dto.EventKindSystem {
		t.Errorf("kind = %q, want %q", events[0].Kind, dto.EventKindSystem)
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
	if events[0].Kind != dto.EventKindText {
		t.Errorf("event[0].kind = %q, want %q", events[0].Kind, dto.EventKindText)
	}
	if events[0].Text.Text != "hello world" {
		t.Errorf("text = %q, want %q", events[0].Text.Text, "hello world")
	}
	if events[1].Kind != dto.EventKindToolUse {
		t.Errorf("event[1].kind = %q, want %q", events[1].Kind, dto.EventKindToolUse)
	}
	if events[1].ToolUse.Name != "Bash" {
		t.Errorf("tool name = %q, want %q", events[1].ToolUse.Name, "Bash")
	}
	if events[1].ToolUse.ToolUseID != "tool_1" {
		t.Errorf("toolUseID = %q, want %q", events[1].ToolUse.ToolUseID, "tool_1")
	}
	if events[2].Kind != dto.EventKindUsage {
		t.Errorf("event[2].kind = %q, want %q", events[2].Kind, dto.EventKindUsage)
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
		MessageType: "stream_event",
		Raw:         []byte(`{"type":"stream_event","event":{"type":"message_start"}}`),
	}
	events := tt.convertMessage(msg, time.Now())
	if events != nil {
		t.Errorf("got %d events for RawMessage, want nil", len(events))
	}
}

func TestConvertResult(t *testing.T) {
	tt := newToolTimingTracker()
	msg := &agent.ResultMessage{
		MessageType:   "result",
		Subtype:       "success",
		IsError:       false,
		Result:        "done",
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
	if ev.Kind != dto.EventKindResult {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindResult)
	}
	r := ev.Result
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
	if ev.Kind != dto.EventKindUserInput {
		t.Errorf("kind = %q, want %q", ev.Kind, dto.EventKindUserInput)
	}
	if ev.UserInput == nil {
		t.Fatal("userInput payload is nil")
	}
	if ev.UserInput.Text != "hello agent" {
		t.Errorf("text = %q, want %q", ev.UserInput.Text, "hello agent")
	}
}
