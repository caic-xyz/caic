// Tests for Pi CLI message parsing.

package pi

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestBackendNewParser(t *testing.T) {
	t.Parallel()
	// Regression: replay (relay output during adoption, and on-disk logs) uses
	// NewParser. It must use the stateful wire format so agent_end produces a
	// terminal ResultMessage; without it RestoreMessages cannot infer the
	// waiting state and adopted pi tasks stay stuck as "running".
	t.Run("synthesizes ResultMessage on agent_end", func(t *testing.T) {
		t.Parallel()
		agentEnd := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":10,"output":5}}]}`)
		msgs, err := New("", nil).NewParser()(agentEnd)
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
			t.Fatalf("NewParser produced no ResultMessage for agent_end: %#v", msgs)
		}
	})
}

func TestHandleDoneResultText(t *testing.T) {
	t.Parallel()

	// handleDone is kept for future Pi protocol versions that may emit done
	// deltas. The current Pi does not emit them, but the code path is tested.

	t.Run("done populates Result field with accumulated text", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		w.mu.Lock()
		w.textAccum.WriteString("Hello from Pi")
		w.mu.Unlock()

		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"end_turn"}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("ParseMessage(done) = %d messages, want 2 (text + result)", len(msgs))
		}
		tm, ok := msgs[0].(*agent.TextMessage)
		if !ok {
			t.Fatalf("msg[0] type = %T, want *agent.TextMessage", msgs[0])
		}
		if tm.Text != "Hello from Pi" {
			t.Errorf("Text = %q, want %q", tm.Text, "Hello from Pi")
		}
		rm, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if rm.Result != "Hello from Pi" {
			t.Errorf("Result = %q, want %q", rm.Result, "Hello from Pi")
		}
	})

	t.Run("done with error reason marks ResultMessage", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		w.mu.Lock()
		w.textAccum.WriteString("partial error")
		w.mu.Unlock()

		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"error"}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		rm, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if !rm.IsError {
			t.Error("IsError = false, want true when done delta had reason=error")
		}
		if rm.Result != "partial error" {
			t.Errorf("Result = %q, want %q", rm.Result, "partial error")
		}
	})

	t.Run("done without text deltas falls back to message content", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		doneLine := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"done","reason":"end_turn","message":{"role":"assistant","content":[{"type":"text","text":"Non-streamed result"}]}}}`)
		msgs, err := w.ParseMessage(doneLine)
		if err != nil {
			t.Fatalf("ParseMessage(done): %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("ParseMessage(done) = %d messages, want 2", len(msgs))
		}
		rm, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("msg[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if rm.Result != "Non-streamed result" {
			t.Errorf("Result = %q, want %q", rm.Result, "Non-streamed result")
		}
	})
}

func TestHandleAgentEndResultText(t *testing.T) {
	t.Parallel()
	t.Run("agent_end includes accumulated text from text_delta events", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		w.mu.Lock()
		w.textAccum.WriteString("Session result text")
		w.mu.Unlock()

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
		if rm.Result != "Session result text" {
			t.Errorf("Result = %q, want %q", rm.Result, "Session result text")
		}
		if rm.Usage.InputTokens != 100 || rm.Usage.OutputTokens != 50 {
			t.Errorf("Usage = %+v, want input=100 output=50", rm.Usage)
		}
	})

	t.Run("agent_end falls back to message content when textAccum is empty", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		// No streaming deltas — textAccum is empty.
		agentEndLine := []byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"Fallback text"}],"usage":{"input":10,"output":5}}]}`)
		msgs, err := w.ParseMessage(agentEndLine)
		if err != nil {
			t.Fatalf("ParseMessage(agent_end): %v", err)
		}
		rm, ok := msgs[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("type = %T, want *agent.ResultMessage", msgs[0])
		}
		if rm.Result != "Fallback text" {
			t.Errorf("Result = %q, want %q", rm.Result, "Fallback text")
		}
	})

	t.Run("agent_end with empty textAccum and no assistant message has empty Result", func(t *testing.T) {
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
