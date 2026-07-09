// Tests for the Pi CLI backend process configuration.

package pi

import (
	"bufio"
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestWaitForResponse(t *testing.T) {
	t.Parallel()

	t.Run("caic_exit surfaces relay stderr", func(t *testing.T) {
		t.Parallel()
		r := bufio.NewReader(bytes.NewBufferString(`{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}` + "\n"))
		_, err := waitForResponse(r, "set_model", nil)
		if err == nil {
			t.Fatal("waitForResponse returned nil error")
		}
		if !strings.Contains(err.Error(), "Unknown option: --approve") {
			t.Fatalf("err = %v, want relay stderr", err)
		}
	})
}

func TestPiWireFormat(t *testing.T) {
	t.Parallel()
	t.Run("MessageStartIncludesSessionMetadata", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{sessionID: "ses-1", agentVersion: "pi 1.2.3"}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_start","message":{"role":"assistant","provider":"openai","model":"gpt-5"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		init, ok := msgs[0].(*agent.InitMessage)
		if !ok {
			t.Fatalf("message type = %T, want *agent.InitMessage", msgs[0])
		}
		if init.SessionID != "ses-1" || init.Model != "openai/gpt-5" || init.Version != "pi 1.2.3" {
			t.Fatalf("InitMessage = %+v", init)
		}
	})

	t.Run("MessageEndQuotaErrorEmitsRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Codex error: The usage limit has been reached"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("len(msgs) = %d, want 2", len(msgs))
		}
		rl, ok := msgs[0].(*agent.RateLimitMessage)
		if !ok {
			t.Fatalf("message[0] type = %T, want *agent.RateLimitMessage", msgs[0])
		}
		if rl.Status != "rejected" {
			t.Fatalf("RateLimitMessage.Status = %q, want rejected", rl.Status)
		}
		res, ok := msgs[1].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("message[1] type = %T, want *agent.ResultMessage", msgs[1])
		}
		if !res.IsError || res.Subtype != "error" || !strings.Contains(res.Result, "usage limit") {
			t.Fatalf("ResultMessage = %+v", res)
		}
	})

	t.Run("MessageEndGenericErrorEmitsNoRateLimit", func(t *testing.T) {
		t.Parallel()
		w := &piWireFormat{}
		msgs, err := w.ParseMessage([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"connection refused"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ResultMessage); !ok {
			t.Fatalf("message[0] type = %T, want *agent.ResultMessage", msgs[0])
		}
	})
}

func TestAgentArgs(t *testing.T) {
	t.Parallel()

	t.Run("approves project-local inputs in rpc mode", func(t *testing.T) {
		t.Parallel()
		args := New("", nil).AgentArgs(agent.HarnessArgs{})
		want := []string{"pi", "--mode", "rpc", "--approve"}
		if !slices.Equal(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
	})
}
