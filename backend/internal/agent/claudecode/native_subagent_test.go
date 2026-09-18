// Tests native Claude lifecycle correlation from the recorded joke evidence.

package claudecode

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the minimized Claude 2.1.270
// recording of the standardized delegation request ("Use a native subagent to
// tell a joke about README.md...") through the live wire parser and both task
// log versions. The recording carries the Agent tool use, task_started,
// task_updated, task_notification, and the parent tool result.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New().NewWire()
	})

	// task_started proves the native agent runs, task_updated settles it, and the
	// task notification or parent tool result reports the same completed state
	// with the harness summary.
	want := []agent.NativeSubagentStatus{
		agent.NativeSubagentStatusRunning,
		agent.NativeSubagentStatusCompleted,
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
			if card.ID != "claude:task:test-agent" || card.ToolUseID != "test-spawn" {
				t.Fatalf("identity = %#v, want the native task identity with tool correlation", card)
			}
			if card.Scope != "" || card.Label != "Tell a joke about README.md" || !strings.Contains(card.Prompt, "joke about README.md") {
				t.Fatalf("metadata = %#v, want the delegation description and prompt", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || !strings.Contains(card.Result, "documentation lead") {
				t.Fatalf("outcome = %#v, want the subagent's joke as the result", card)
			}
			if active != 0 {
				t.Fatalf("active = %d, want 0 after the recorded completion", active)
			}
		})
	}
}

// TestNativeSubagentRestartSettlesSameCard models a server restart or relay
// adoption between the recorded spawn and the recorded completion: the restored
// history shows the running card, and the live tail continues on a fresh wire
// warmed from that history. It must settle the same identity instead of adding a
// second card and leaving the first one running.
func TestNativeSubagentRestartSettlesSameCard(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New().NewWire()
	})
	// History through task_started; the tail continues with the child stream
	// message, task_updated, task_notification, and the parent tool result.
	history, tail := evidence.Payloads[:2], evidence.Payloads[2:]
	restored, restoredActive := agenttest.FoldNativeSubagents(historyObservations(t, history))
	if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning || restoredActive != 1 {
		t.Fatalf("restored = %#v active = %d, want one running card", restored, restoredActive)
	}
	live := agenttest.RestartNativeSubagents(t, agent.LogVersionV3, history, tail, New().NewWire())

	observations := append(historyObservations(t, history), live...)
	cards, active := agenttest.FoldNativeSubagents(observations)
	if len(cards) != 1 || cardStatus(cards) != agent.NativeSubagentStatusCompleted || active != 0 {
		t.Fatalf("cards = %#v active = %d, want one completed card after restart", cards, active)
	}
	if cards[0].ID != restored[0].ID {
		t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
	}
}

// historyObservations returns the canonical observations of a record prefix.
func historyObservations(t *testing.T, payloads [][]byte) []agent.NativeSubagent {
	t.Helper()
	wire := New().NewWire()
	var out []agent.NativeSubagent
	for index, payload := range payloads {
		messages, err := wire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("history record %d: %v", index, err)
		}
		for _, message := range messages {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				out = append(out, native.Subagent)
			}
		}
	}
	return out
}

func cardStatus(cards []agent.NativeSubagent) agent.NativeSubagentStatus {
	if len(cards) == 0 {
		return ""
	}
	return cards[0].Status
}

// TestClaudeSubagentStatus pins the protocol status mapping, including the
// paused and killed states the wire reports.
func TestClaudeSubagentStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status string
		want   agent.NativeSubagentStatus
	}{
		{"running", agent.NativeSubagentStatusRunning},
		{"in_progress", agent.NativeSubagentStatusRunning},
		{"paused", agent.NativeSubagentStatusPaused},
		{"completed", agent.NativeSubagentStatusCompleted},
		{"failed", agent.NativeSubagentStatusFailed},
		{"killed", agent.NativeSubagentStatusInterrupted},
		{"stopped", agent.NativeSubagentStatusInterrupted},
		{"interrupted", agent.NativeSubagentStatusInterrupted},
		{"pending", agent.NativeSubagentStatusUnknown},
		{"", agent.NativeSubagentStatusUnknown},
		{"reticulating", agent.NativeSubagentStatusUnknown},
	} {
		if got := claudeSubagentStatus(test.status); got != test.want {
			t.Fatalf("claudeSubagentStatus(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}

// TestNativeSubagentChildStreamCannotSettle guards the parent_tool_use_id rule:
// a child stream message is never the parent's Agent tool result, even when it
// carries a tool result for the same tool use ID.
func TestNativeSubagentChildStreamCannotSettle(t *testing.T) {
	t.Parallel()
	wire := New().NewWire()
	feed := func(line string) []agent.Message {
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("ParseMessage(%s): %v", line, err)
		}
		return msgs
	}
	feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"test-spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`)
	feed(`{"type":"system","subtype":"task_started","task_id":"test-agent","tool_use_id":"test-spawn","task_type":"local_agent"}`)

	child := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"test-spawn","type":"tool_result","content":[{"type":"text","text":"child noise"}]}]},"parent_tool_use_id":"test-spawn"}`
	var native []*agent.NativeSubagentMessage
	for _, message := range feed(child) {
		if m, ok := message.(*agent.NativeSubagentMessage); ok {
			native = append(native, m)
		}
	}
	if len(native) != 0 {
		t.Fatalf("child stream produced %d lifecycle updates, want 0", len(native))
	}
}
