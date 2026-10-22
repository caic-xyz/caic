// Tests native OpenCode ACP task delegation and the absence of child streams in the recorded parent transport.

package opencode

import (
	"fmt"
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
			if card.Status != agent.NativeSubagentStatusCompleted {
				t.Fatalf("outcome = %#v, want a completed card", card)
			}
			if want := "README.md is the project\u2019s autobiography, revised only after a crisis."; card.Result != want {
				t.Fatalf("result = %q, want the unwrapped task_result text %q", card.Result, want)
			}
			if card.Background {
				t.Fatalf("card = %#v, want a synchronous delegation", card)
			}
			if active != 0 {
				t.Fatalf("active = %d, want 0 after the recorded completion", active)
			}
		})
	}
}

// TestTaskResultText unwraps the task tool's session envelope and preserves an
// unrecognized payload instead of dropping it.
func TestTaskResultText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "wrapped",
			input: "<task id=\"child\" state=\"completed\">\n<task_result>\na joke\n</task_result>\n</task>",
			want:  "a joke",
		},
		{name: "bare output", input: "a joke", want: "a joke"},
		{name: "opening tag only", input: "<task_result>a joke", want: "<task_result>a joke"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := taskResultText(test.input); got != test.want {
				t.Fatalf("taskResultText(%q) = %q, want %q", test.input, got, test.want)
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

// TestDelegationTransportOwnership pins what the retained parent transport
// actually exposes: delegation tools/results and native lifecycle, never the
// child's internal tool stream or ownership inferred from result prose.
func TestDelegationTransportOwnership(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat { return New("", nil).NewWire() })
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			wire := New("", nil).NewWire()
			parser, err := agent.NewLogRecordParser(agent.LogVersionV3, wire.ParseMessage)
			if version != 0 {
				parser, err = agent.NewLogRecordParser(version, wire.ParseMessage)
			}
			if err != nil {
				t.Fatal(err)
			}
			var calls, results, outputs, cards int
			for _, line := range evidence.Payloads {
				var messages []agent.Message
				if version == 0 {
					messages, err = wire.ParseMessage(line)
				} else {
					record, e := parser.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":` + string(line) + `}`))
					err = e
					for _, timed := range record.Messages {
						messages = append(messages, timed.Message)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range messages {
					if m.Provenance().IsChild() {
						t.Fatalf("invented child stream on %T: %+v", m, m.Provenance())
					}
					switch m := m.(type) {
					case *agent.ToolUseMessage:
						calls++
					case *agent.ToolResultMessage:
						results++
					case *agent.ToolOutputDeltaMessage:
						outputs++
					case *agent.NativeSubagentMessage:
						cards++
						if m.Subagent.Result != "" {
							outputs++
						}
					}
				}
			}
			if calls == 0 || results == 0 || outputs == 0 || cards == 0 {
				t.Fatalf("lost exposed paths: calls=%d results=%d output=%d cards=%d", calls, results, outputs, cards)
			}
		})
	}
}
