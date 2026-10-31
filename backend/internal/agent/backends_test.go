// Tests configured backend lookup and independent full and metadata parser factories.

package agent_test

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestBackends(t *testing.T) {
	t.Parallel()

	for _, factory := range []string{"ResolveParser", "ResolveMetadataParser"} {
		t.Run(factory, func(t *testing.T) {
			t.Parallel()

			backends := agent.Backends{
				harness.Claude: &agenttest.FakeBackend{HarnessName: harness.Claude},
			}
			resolve := backends.ResolveParser
			if factory == "ResolveMetadataParser" {
				resolve = backends.ResolveMetadataParser
			}
			t.Run("valid", func(t *testing.T) {
				t.Parallel()
				first, err := resolve(harness.Claude)
				if err != nil {
					t.Fatal(err)
				}
				second, err := resolve(harness.Claude)
				if err != nil {
					t.Fatal(err)
				}
				if first == second {
					t.Errorf("%s returned a shared parser", factory)
				}
			})
			t.Run("unknown", func(t *testing.T) {
				t.Parallel()
				if _, err := resolve(harness.Pi); err == nil || !strings.Contains(err.Error(), "unknown harness") {
					t.Fatalf("%s error = %v, want unknown harness", factory, err)
				}
			})
		})
	}
}
