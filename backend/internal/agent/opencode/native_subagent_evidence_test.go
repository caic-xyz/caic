// Tests native OpenCode ACP task delegation from the recorded joke evidence.

package opencode

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the minimized OpenCode 1.18.31 ACP
// recording of the standardized delegation request. The recording carries the
// native `task` tool call (raw title plus structured delegation input), its
// in-progress update, and its completion with the ACP-reported output.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})

	// Pending proves nothing, so the first observation is the in-progress
	// delegation, which the completion settles once.
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
			if card.ID != "opencode:tool:test-task-call" || card.ToolUseID != "test-task-call" {
				t.Fatalf("identity = %#v, want the ACP task tool call identity", card)
			}
			if card.Scope != "" || card.Label != "README joke" || !strings.Contains(card.Prompt, "joke about README.md") {
				t.Fatalf("metadata = %#v, want the structured delegation input", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || !strings.Contains(card.Result, "task_result") {
				t.Fatalf("outcome = %#v, want the ACP-reported task output", card)
			}
			if active != 0 {
				t.Fatalf("active = %d, want 0 after the recorded completion", active)
			}
		})
	}
}

// TestNativeSubagentRestartSettlesSameCard models a restart or relay adoption
// between the recorded in-progress delegation and its completion. The warmed
// wire must settle the restored identity instead of leaving it running.
func TestNativeSubagentRestartSettlesSameCard(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New("", nil).NewWire()
	})
	history, tail := evidence.Payloads[:2], evidence.Payloads[2:]
	restoredObservations, restored := liveSubagents(t, history)
	if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning {
		t.Fatalf("restored = %#v, want one running card", restored)
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

// liveSubagents parses native payloads on one wire and returns the observations.
func liveSubagents(t *testing.T, payloads [][]byte) (observations, cards []agent.NativeSubagent) {
	t.Helper()
	wire := New("", nil).NewWire()
	for index, payload := range payloads {
		messages, err := wire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("payload %d: %v", index, err)
		}
		for _, message := range messages {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				observations = append(observations, native.Subagent)
			}
		}
	}
	cards, _ = agenttest.FoldNativeSubagents(observations)
	return observations, cards
}

// TestNativeSubagentRequiresDelegationEvidence guards the adapter's trust
// rules: a pending task tool call with no delegation input, a normalized
// display name, and unrelated tools never fabricate a native agent.
func TestNativeSubagentRequiresDelegationEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		lines []string
	}{
		{
			name: "pending task without structured input",
			lines: []string{
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"task","kind":"think","status":"pending","rawInput":{}}}}`,
			},
		},
		{
			name: "normalized display name is not proof",
			lines: []string{
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Agent","kind":"think","status":"pending","rawInput":{}}}}`,
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":"Agent","rawInput":{"description":"Joke","subagent_type":"general","prompt":"Tell a joke"}}}}`,
			},
		},
		{
			name: "unrelated tool completion",
			lines: []string{
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","kind":"execute","status":"pending","rawInput":{}}}}`,
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":"bash","rawInput":{"command":"joke"}}}}`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wire := New("", nil).NewWire()
			for _, line := range test.lines {
				msgs, err := wire.ParseMessage([]byte(line))
				if err != nil {
					t.Fatalf("ParseMessage(%s): %v", line, err)
				}
				for _, message := range msgs {
					if native, ok := message.(*agent.NativeSubagentMessage); ok {
						t.Fatalf("unexpected native subagent %#v", native.Subagent)
					}
				}
			}
		})
	}
}
