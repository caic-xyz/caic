// Tests native Pi subagent-extension lifecycle from the recorded joke evidence.

package pi

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the minimized Pi 0.85.1 recording of
// the standardized delegation request with the installed pi-subagents
// extension: an introspection action:list (never a spawn), one async `delegate`
// spawn, and the subagent_wait completion that settles it.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})

	// The tool end that reports the extension's run ID creates the card and
	// acknowledges dispatch (running); the wait completion settles it.
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
				if observation.ID != "pi:run:test-run" || observation.ToolUseID != "test-spawn" {
					t.Fatalf("observation %d identity = %q, want the run identity with tool correlation", i, observation.ID)
				}
			}
			cards, active := agenttest.FoldNativeSubagents(test.observations)
			if len(cards) != 1 {
				t.Fatalf("cards = %#v, want one native agent", cards)
			}
			card := cards[0]
			if card.Scope != "" || !strings.HasPrefix(card.Label, "delegate — ") || !strings.Contains(card.Prompt, "joke about README.md") {
				t.Fatalf("metadata = %#v, want the single delegated agent", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || active != 0 {
				t.Fatalf("outcome = %#v active = %d, want a settled single agent", card, active)
			}
			if !card.Background {
				t.Fatalf("card = %#v, want a detached single agent", card)
			}
			// The extension's completion carries the artifact trail, not the
			// output text, so the card must reference the artifact rather than
			// claim a result it never received.
			if !strings.Contains(card.Result, "Output file:") || strings.Contains(card.Result, "No result") {
				t.Fatalf("result = %q, want the reported output artifact", card.Result)
			}
		})
	}
}

// TestNativeSubagentRestartSettlesSameCard models a restart or relay adoption
// between the recorded async spawn acknowledgement and the recorded wait
// completion. The warmed wire must settle the restored identity instead of
// leaving it running and adding a run-keyed duplicate.
func TestNativeSubagentRestartSettlesSameCard(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})
	// History through the async acknowledgement; the tail is the wait.
	history, tail := evidence.Payloads[:4], evidence.Payloads[4:]
	restoredWire := New("", nil).NewWire()
	var restoredObservations []agent.NativeSubagent
	for index, payload := range history {
		messages, err := restoredWire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("history record %d: %v", index, err)
		}
		for _, message := range messages {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				restoredObservations = append(restoredObservations, native.Subagent)
			}
		}
	}
	restored, restoredActive := agenttest.FoldNativeSubagents(restoredObservations)
	if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning || restoredActive != 1 {
		t.Fatalf("restored = %#v active = %d, want one running card", restored, restoredActive)
	}
	live := agenttest.RestartNativeSubagents(t, agent.LogVersionV3, history, tail, New("", nil).NewWire())

	cards, active := agenttest.FoldNativeSubagents(append(restoredObservations, live...))
	if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusCompleted || active != 0 {
		t.Fatalf("cards = %#v active = %d, want one completed card after restart", cards, active)
	}
	if cards[0].ID != restored[0].ID {
		t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
	}
}

// TestNativeSubagentWorkflowEvidence replays the minimized Pi 0.85.1 recording
// of one workflowScript orchestration. The installed extension removed the
// legacy top-level parallel and chain arguments, so a workflowScript run is the
// current batch shape; it stays an explicit batch card because the wire reports
// no per-agent lifecycle for its steps.
func TestNativeSubagentWorkflowEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-workflow-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})

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
				if observation.ID != "pi:run:test-workflow-run" {
					t.Fatalf("observation %d identity = %q, want the workflow run identity", i, observation.ID)
				}
			}
			cards, active := agenttest.FoldNativeSubagents(test.observations)
			if len(cards) != 1 {
				t.Fatalf("cards = %#v, want one batch card", cards)
			}
			card := cards[0]
			if card.Scope != agent.NativeSubagentScopeBatch || card.Label != "workflow" {
				t.Fatalf("metadata = %#v, want an explicit batch orchestration", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || active != 0 {
				t.Fatalf("outcome = %#v active = %d, want a settled batch", card, active)
			}
			if !card.Background {
				t.Fatalf("card = %#v, want a detached batch", card)
			}
			if !strings.Contains(card.Result, "Output file:") {
				t.Fatalf("result = %q, want the workflow's reported output artifact", card.Result)
			}
		})
	}
}
