// Tests configured backend-set lookup and fresh wire construction.

package agent_test

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestBackendsResolveWire(t *testing.T) {
	t.Parallel()

	backends := agent.Backends{
		harness.Claude: &agenttest.FakeBackend{HarnessName: harness.Claude},
	}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		first, err := backends.ResolveWire(harness.Claude)
		if err != nil {
			t.Fatal(err)
		}
		second, err := backends.ResolveWire(harness.Claude)
		if err != nil {
			t.Fatal(err)
		}
		if first == second {
			t.Error("ResolveWire returned a shared wire")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		if _, err := backends.ResolveWire(harness.Pi); err == nil || !strings.Contains(err.Error(), "unknown harness") {
			t.Fatalf("ResolveWire error = %v, want unknown harness", err)
		}
	})
}
