// Verifies the minimized Codex 0.154.0 native-subagent protocol evidence.

package codex

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the minimized Codex 0.154.0 recording
// of the standardized delegation request. The recording carries the spawn as a
// sub-agent activity item (started then completed) plus the receiverless
// collaboration wait that must never produce a card, so the canonical result is
// one agent that runs and completes.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})

	// The started activity proves the agent runs; the completed activity settles
	// it. The receiverless wait in between is not evidence of anything.
	want := []agent.NativeSubagentStatus{
		agent.NativeSubagentStatusRunning,
		agent.NativeSubagentStatusCompleted,
	}
	for _, test := range []struct {
		name         string
		observations []agent.NativeSubagent
	}{
		{"live", evidence.Live},
		{"v2 replay", evidence.V2},
		{"v3 replay", evidence.V3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if len(test.observations) != len(want) {
				t.Fatalf("observations = %#v, want %d", test.observations, len(want))
			}
			for i, observation := range test.observations {
				if observation.Status != want[i] {
					t.Fatalf("observation %d status = %q, want %q", i, observation.Status, want[i])
				}
			}
			cards, active := agenttest.FoldNativeSubagents(test.observations)
			if len(cards) != 1 {
				t.Fatalf("cards = %#v, want one native agent", cards)
			}
			card := cards[0]
			if card.ID != "codex:thread:test-agent-thread" {
				t.Fatalf("identity = %#v, want the reported agent thread", card)
			}
			if card.Label != "readme_joke" || card.ToolUseID != "test-spawn-call" {
				t.Fatalf("metadata = %#v, want the harness-provided agent name and spawn call", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || active != 0 {
				t.Fatalf("outcome = %#v active = %d, want a settled agent", card, active)
			}
		})
	}
}
