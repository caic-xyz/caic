package pi

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestWireFormatDurationTracking(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		w := &piWireFormat{}
		w.mu.Lock()
		w.startTime = time.Now().Add(-500 * time.Millisecond)
		w.mu.Unlock()

		// Pi protocol: turn_end arrives before agent_end.
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
		w := &piWireFormat{}
		// startTime is zero — duration should remain 0.
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
		w := &piWireFormat{}

		// First turn.
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

		// Second turn: simulate WritePrompt resetting state.
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
		rm := endMsgs[0].(*agent.ResultMessage)
		if rm.DurationMs < 200 || rm.DurationMs > 500 {
			t.Errorf("DurationMs = %d, want ~300 (second turn)", rm.DurationMs)
		}
		if rm.NumTurns != 2 {
			t.Errorf("NumTurns = %d, want 2", rm.NumTurns)
		}
	})
}
