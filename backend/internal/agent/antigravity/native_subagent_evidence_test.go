// Tests the minimized agy invoke_subagent evidence: its prompt response settles the card, live, replayed, and across a restart.

package antigravity

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the standardized delegation, minimized
// from the recorded ACP trace. agy acknowledges the spawn at once and reports
// no child lifecycle, so only the prompt response can settle the card; the
// fixture records the prompt request that response answers.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	newWire := func() agent.WireFormat { return New("", nil).NewWire() }
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", newWire)
	for _, test := range []struct {
		name         string
		observations []agent.NativeSubagent
	}{
		{"live", evidence.Live},
		{"v3 replay", evidence.Replay},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if len(test.observations) != 2 || test.observations[0].Status != agent.NativeSubagentStatusRunning {
				t.Fatalf("observations = %#v, want running then settled", test.observations)
			}
			cards, active := agenttest.FoldNativeSubagents(test.observations)
			if len(cards) != 1 || active != 0 {
				t.Fatalf("cards = %#v active = %d, want one settled card", cards, active)
			}
			card := cards[0]
			if card.ID != "antigravity:tool:test-spawn-call" || card.Label != "Comedian" || card.Kind != "self" {
				t.Fatalf("card = %#v, want the invoke_subagent identity, role, and type", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || card.Background {
				t.Fatalf("card = %#v, want a foreground card the end_turn response completed", card)
			}
		})
	}
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		split := evidence.RunningSplit(t)
		live := agenttest.RestartNativeSubagents(t, evidence.Prompt, evidence.Payloads[:split], evidence.Payloads[split:], newWire())
		cards, active := agenttest.FoldNativeSubagents(append(append([]agent.NativeSubagent(nil), evidence.Live[:1]...), live...))
		if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusCompleted || active != 0 {
			t.Fatalf("cards = %#v active = %d, want the restored card completed by the response", cards, active)
		}
	})
}
