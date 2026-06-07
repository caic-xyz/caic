// Tests for Pi CLI message parsing.

package pi

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestBackendNewWire(t *testing.T) {
	t.Parallel()
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
