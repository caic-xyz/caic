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
