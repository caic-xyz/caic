// Tests the minimized dsh subagent evidence: its prompt response settles the card, live, replayed, across a restart, and per session.

package dsh

import (
	"bytes"
	"os"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the standardized delegation, minimized
// from the recorded ACP trace. dsh reports the child only by the name in its
// spawn output, so the prompt response settles the card; the fixture records
// the prompt request that response answers.
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
			if card.ID != "dsh:subagent:test-parent-session/agent-1" || card.ToolUseID != "test-spawn-call" || card.Label != "Tell README.md joke" {
				t.Fatalf("card = %#v, want the spawned child's session-scoped name and description", card)
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
	t.Run("cleared context", func(t *testing.T) {
		t.Parallel()
		// A cleared context starts a new dsh session in the same task log, and
		// dsh names that session's first child agent-1 again.
		data, err := os.ReadFile("testdata/evidence/native-subagent-joke-v3.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		records := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
		for _, record := range records[1:] {
			if bytes.Contains(record, []byte(`"msg":`)) {
				records = append(records, bytes.ReplaceAll(record, []byte(`"test-parent-session"`), []byte(`"test-cleared-session"`)))
			}
		}
		parser, err := agent.NewLogRecordParser(newWire())
		if err != nil {
			t.Fatal(err)
		}
		var observations []agent.NativeSubagent
		for _, line := range records {
			record, err := parser.ParseRecord(line)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range record.Messages {
				if m, ok := event.Message.(*agent.NativeSubagentMessage); ok {
					observations = append(observations, m.Subagent)
				}
			}
		}
		cards, active := agenttest.FoldNativeSubagents(observations)
		if len(cards) != 2 || cards[0].ID == cards[1].ID || active != 0 {
			t.Fatalf("cards = %#v active = %d, want one settled card per session", cards, active)
		}
	})
}
