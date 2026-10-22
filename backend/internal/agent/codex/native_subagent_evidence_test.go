// Verifies the minimized Codex 0.154.0 native-subagent protocol evidence.

package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
			if !card.Background {
				t.Fatalf("card = %#v, want a detached agent", card)
			}
		})
	}
}

// TestConcurrentChildToolsEvidence pins a seven-record excerpt of task
// 4H9K7LT6U000. The command and IDs are sanitized; the notification ordering,
// enclosing actor, child tool failure and suppressed child completion remain.
func TestConcurrentChildToolsEvidence(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/evidence/concurrent-child-tools-v3.jsonl")
	if err != nil {
		t.Fatal(err)
	}
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
			var messages []agent.Message
			for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
				if version == 0 {
					var raw struct {
						Message json.RawMessage `json:"msg"`
					}
					if err := json.Unmarshal(line, &raw); err != nil {
						t.Fatal(err)
					}
					msgs, err := wire.ParseMessage(raw.Message)
					if err != nil {
						t.Fatal(err)
					}
					messages = append(messages, msgs...)
				} else {
					record, err := parser.ParseRecord(line)
					if err != nil {
						t.Fatal(err)
					}
					for _, timed := range record.Messages {
						messages = append(messages, timed.Message)
					}
				}
			}
			var starts, results, output, usage, observations int
			for _, m := range messages {
				switch m := m.(type) {
				case *agent.InitMessage:
					continue
				case *agent.RawMessage:
					continue
				case *agent.NativeSubagentMessage:
					observations++
					if m.Subagent.ID != "codex:thread:child" || m.Source.IsChild() || m.Source.ThreadID != "root" {
						t.Fatalf("actor confused with subject: %+v", m)
					}
					continue
				case *agent.ToolUseMessage:
					starts++
				case *agent.ToolResultMessage:
					results++
				case *agent.ToolOutputDeltaMessage:
					output++
				case *agent.UsageMessage:
					usage++
					if m.ReportedModel != "" {
						t.Fatalf("unknown child model guessed from parent: %q", m.ReportedModel)
					}
				case *agent.ResultMessage:
					t.Fatal("child completion escaped as parent result")
				}
				s := m.Provenance()
				if s.AgentID != "codex:thread:child" || s.ThreadID != "child" || s.TurnID != "child-turn" || s.RootThreadID != "root" {
					t.Fatalf("lost child source on %T: %+v", m, s)
				}
			}
			if starts != 1 || results != 1 || output != 1 || usage != 1 || observations != 1 {
				t.Fatalf("events starts=%d results=%d output=%d usage=%d cards=%d", starts, results, output, usage, observations)
			}
			// Reusing this warmed wire proves child completion did not consume
			// accumulated usage while the parent is still in flight.
			msgs, err := wire.ParseMessage([]byte(`{"method":"turn/completed","params":{"threadId":"root","turn":{"id":"root-turn","status":"completed"}}}`))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("root result = %v", msgs)
			}
			r, ok := msgs[0].(*agent.ResultMessage)
			if !ok {
				t.Fatalf("root result type = %T", msgs[0])
			}
			if r.Source.IsChild() || r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 2 {
				t.Fatalf("root usage is not inclusive: %+v", r)
			}
		})
	}
}
