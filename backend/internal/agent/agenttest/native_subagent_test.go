// Tests the harness-neutral contract projection used to compare live and fixture native-subagent evidence.

package agenttest

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestContract(t *testing.T) {
	t.Parallel()
	observations := []agent.NativeSubagent{
		{
			ID:        "claude:task:agent-1",
			ToolUseID: "spawn-1",
			Label:     "Tell a joke",
			Prompt:    "Tell a joke about README.md",
			Status:    agent.NativeSubagentStatusRunning,
		},
		// The completion upgrades the interim observation and carries the result.
		{ID: "claude:task:agent-1", Status: agent.NativeSubagentStatusCompleted, Result: "nobody reads it"},
		// An identity-less observation cannot be correlated and is dropped.
		{ID: "", Status: agent.NativeSubagentStatusRunning},
	}

	t.Run("SettledCard", func(t *testing.T) {
		t.Parallel()
		want := NativeSubagentContract{
			Cards: []NativeSubagentContractCard{{
				Identity:  "claude:task",
				Status:    agent.NativeSubagentStatusCompleted,
				ToolUseID: true,
				Label:     true,
				Prompt:    true,
				Result:    true,
			}},
			Statuses: []agent.NativeSubagentStatus{
				agent.NativeSubagentStatusRunning,
				agent.NativeSubagentStatusCompleted,
			},
		}
		got := Contract(observations)
		if !got.Equal(want) {
			t.Fatalf("Contract() = %s, want %s", got, want)
		}
	})

	t.Run("RunningCardDiffersFromSettledCard", func(t *testing.T) {
		t.Parallel()
		// One observation in, the card is still running with no result yet.
		running := Contract(observations[:1])
		if running.Equal(Contract(observations)) {
			t.Fatalf("Contract() = %s, want a running card to differ from the settled card", running)
		}
		if running.Active != 1 || running.Cards[0].Status != agent.NativeSubagentStatusRunning || running.Cards[0].Result {
			t.Fatalf("Contract() = %s, want one active card without a result", running)
		}
	})

	t.Run("IdentityScheme", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct{ id, want string }{
			{"claude:task:agent-1", "claude:task"},
			{"codex:thread:0197", "codex:thread"},
			{"opencode:tool:call_1", "opencode:tool"},
			{"pi:run:run-1", "pi:run"},
			// A CAIC task ID carries no harness-native scheme, so it can never
			// project to a fixture's identity.
			{"0123456789ABCDEF", "0123456789ABCDEF"},
		} {
			card := Contract([]agent.NativeSubagent{{ID: test.id, Status: agent.NativeSubagentStatusRunning}})
			if len(card.Cards) != 1 || card.Cards[0].Identity != test.want {
				t.Errorf("Contract(%q) identity = %s, want %q", test.id, card, test.want)
			}
		}
	})
}

func TestRunningSplit(t *testing.T) {
	t.Parallel()
	evidence := NativeSubagentEvidence{
		PerPayload: [][]agent.NativeSubagent{
			// The spawn tool use alone proves nothing.
			nil,
			{{ID: "claude:task:agent-1", Status: agent.NativeSubagentStatusRunning}},
			{{ID: "claude:task:agent-1", Status: agent.NativeSubagentStatusCompleted}},
		},
	}
	if got := evidence.RunningSplit(t); got != 2 {
		t.Fatalf("RunningSplit() = %d, want the payload that first reports a running card", got)
	}
}
