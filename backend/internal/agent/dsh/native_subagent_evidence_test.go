// Tests the minimized dsh subagent evidence: its prompt response settles the card, live, replayed, and across a restart.

package dsh

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentSillyEvidence replays a delegation minimized from the
// recorded ACP trace. dsh reports the child only by its spawn output, so the
// prompt response settles the card; the fixture records the prompt request
// that response answers.
func TestNativeSubagentSillyEvidence(t *testing.T) {
	t.Parallel()
	newWire := func() agent.WireFormat { return New("", nil).NewWire() }
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-silly-v3.jsonl", newWire)
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
			if card.ID != "test-child-session" || card.ToolUseID != "test-spawn-call" || card.Label != "Silly duck task" {
				t.Fatalf("card = %#v, want the spawned child identity and description", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted {
				t.Fatalf("card = %#v, want the end_turn response to complete it", card)
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
