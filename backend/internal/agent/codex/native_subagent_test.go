// Tests Codex native-subagent adapter correlation over app-server wire events.

package codex

import (
	"encoding/json"
	"testing"

	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// collabItem builds one app-server item notification for a collab tool call.
func collabItem(method, tool, itemID string, receivers []string, prompt string, states map[string]codexAgentState) []byte {
	item := map[string]any{
		"id":     itemID,
		"type":   "collabAgentToolCall",
		"tool":   tool,
		"prompt": prompt,
	}
	item["senderThreadId"] = "parent"
	if len(receivers) > 0 {
		item["receiverThreadIds"] = receivers
	}
	if len(states) > 0 {
		item["agentsStates"] = states
	}
	data, err := json.Marshal(map[string]any{"method": method, "params": map[string]any{"item": item, "threadId": "t"}})
	if err != nil {
		panic(err)
	}
	return data
}

// activityItem builds one app-server sub-agent activity notification.
func activityItem(kind, itemID, agentThreadID string) []byte {
	item := map[string]any{
		"id":   itemID,
		"type": "subAgentActivity",
	}
	if kind != "" {
		item["kind"] = kind
	}
	if agentThreadID != "" {
		item["agentThreadId"] = agentThreadID
	}
	item["agentPath"] = "/root/work"
	data, err := json.Marshal(map[string]any{"method": "item/started", "params": map[string]any{"item": item, "threadId": "parent"}})
	if err != nil {
		panic(err)
	}
	return data
}

type codexAgentState struct {
	Status  string  `json:"status"`
	Message *string `json:"message,omitempty"`
}

func collectNative(t *testing.T, wire agent.WireFormat, lines ...[]byte) []agent.NativeSubagent {
	t.Helper()
	var got []agent.NativeSubagent
	for _, line := range lines {
		msgs, err := wire.ParseMessage(line)
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		for _, m := range msgs {
			if n, ok := m.(*agent.NativeSubagentMessage); ok {
				got = append(got, n.Subagent)
			}
		}
	}
	return got
}

func TestCodexNativeSubagentAdapter(t *testing.T) {
	t.Parallel()
	t.Run("spawn maps receivers and states settle known threads", func(t *testing.T) {
		t.Parallel()
		wire := New("", nil).NewWire()
		spawn := collabItem("item/started", "spawnAgent", "item1", []string{"th1", "th2"}, "Do work", nil)
		msg := "child finished"
		done := collabItem("item/completed", "spawnAgent", "item1", nil, "", map[string]codexAgentState{
			"th1": {Status: "completed", Message: &msg},
			"th2": {Status: "errored"},
		})
		got := collectNative(t, wire, spawn, done)
		byThread := map[string]agent.NativeSubagent{}
		for _, s := range got {
			byThread[s.ID] = s
		}
		if len(byThread) != 2 {
			t.Fatalf("lifecycles = %#v", got)
		}
		if s := byThread["codex:thread:th1"]; s.Status != agent.NativeSubagentStatusCompleted || s.Result != "child finished" || s.Prompt != "Do work" || s.GroupID != "item1" {
			t.Fatalf("th1 = %#v", s)
		}
		if s := byThread["codex:thread:th2"]; s.Status != agent.NativeSubagentStatusFailed {
			t.Fatalf("th2 = %#v", s)
		}
	})
	t.Run("wait and close never invent spawns", func(t *testing.T) {
		t.Parallel()
		wire := New("", nil).NewWire()
		wait := collabItem("item/started", "wait", "item2", nil, "", map[string]codexAgentState{
			"unknown-thread": {Status: "completed"},
		})
		if got := collectNative(t, wire, wait); len(got) != 0 {
			t.Fatalf("wait produced %d cards, want 0: %#v", len(got), got)
		}
	})
	t.Run("restart settles the restored receiver identity", func(t *testing.T) {
		t.Parallel()
		// A restart or relay adoption replays the spawn through the wire before
		// the live stream continues, so the later agent state settles the same
		// thread identity instead of leaving the card running.
		history := collabItem("item/started", "spawnAgent", "item1", []string{"th1"}, "Do work", nil)
		tail := collabItem("item/completed", "wait", "item2", []string{"th1"}, "", map[string]codexAgentState{
			"th1": {Status: "completed", Message: new("done")},
		})
		warmWire := New("", nil).NewWire()
		restored := collectNative(t, warmWire, history)
		if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusUnknown {
			t.Fatalf("restored = %#v, want one spawned receiver", restored)
		}
		live := agenttest.RestartNativeSubagents(t, agent.LogVersionV3, [][]byte{history}, [][]byte{tail}, New("", nil).NewWire())
		observations := make([]agent.NativeSubagent, 0, len(restored)+len(live))
		observations = append(observations, restored...)
		observations = append(observations, live...)
		cards, active := agenttest.FoldNativeSubagents(observations)
		if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusCompleted || active != 0 {
			t.Fatalf("cards = %#v active = %d, want one completed card after restart", cards, active)
		}
		if cards[0].ID != restored[0].ID {
			t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
		}
	})
	t.Run("activity kinds map to the reported lifecycle", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			kind codex.SubAgentActivityKind
			want agent.NativeSubagentStatus
		}{
			{codex.SubAgentActivityKindStarted, agent.NativeSubagentStatusRunning},
			{codex.SubAgentActivityKindInteracted, agent.NativeSubagentStatusRunning},
			{codex.SubAgentActivityKindInterrupted, agent.NativeSubagentStatusInterrupted},
			{codex.SubAgentActivityKindCompleted, agent.NativeSubagentStatusCompleted},
			{"", agent.NativeSubagentStatusUnknown},
			{"reticulating", agent.NativeSubagentStatusUnknown},
		} {
			if got := subAgentActivityStatus(test.kind); got != test.want {
				t.Fatalf("subAgentActivityStatus(%q) = %q, want %q", test.kind, got, test.want)
			}
		}
	})
	t.Run("activity observations report only changed facts", func(t *testing.T) {
		t.Parallel()
		wire := New("", nil).NewWire()
		started := activityItem("started", "call_1", "th1")
		interacted := activityItem("interacted", "msg_1", "th1")
		interrupted := activityItem("interrupted", "msg_2", "th1")
		completed := activityItem("completed", "subagent-completed-1", "th1")
		got := collectNative(t, wire, started, interacted, interrupted, completed)
		// interacted repeats running, and a terminal interruption cannot be
		// reopened by a later completion.
		want := []agent.NativeSubagentStatus{
			agent.NativeSubagentStatusRunning,
			agent.NativeSubagentStatusInterrupted,
		}
		if len(got) != len(want) {
			t.Fatalf("observations = %#v, want %d", got, len(want))
		}
		for i, observation := range got {
			if observation.Status != want[i] || observation.ID != "codex:thread:th1" {
				t.Fatalf("observation %d = %#v, want %q", i, observation, want[i])
			}
			if observation.Label != "work" {
				t.Fatalf("observation %d label = %q, want the harness agent name", i, observation.Label)
			}
		}
		// An unknown kind creates the agent without inventing a state.
		if got := collectNative(t, New("", nil).NewWire(), activityItem("", "call_1", "th1")); len(got) != 1 || got[0].Status != agent.NativeSubagentStatusUnknown {
			t.Fatalf("unknown kind = %#v, want one unknown agent", got)
		}
		// An activity without an agent thread identity cannot be correlated.
		if got := collectNative(t, New("", nil).NewWire(), activityItem("started", "call_1", "")); len(got) != 0 {
			t.Fatalf("identity-less activity = %#v, want none", got)
		}
	})
	t.Run("restart settles the restored activity identity", func(t *testing.T) {
		t.Parallel()
		// Activities carry the agent thread ID on every kind, so a completion
		// after a restart settles the identity the history already shows.
		history := activityItem("started", "call_1", "th1")
		tail := activityItem("completed", "subagent-completed-1", "th1")
		restored := collectNative(t, New("", nil).NewWire(), history)
		if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning {
			t.Fatalf("restored = %#v, want one running agent", restored)
		}
		live := agenttest.RestartNativeSubagents(t, agent.LogVersionV3, [][]byte{history}, [][]byte{tail}, New("", nil).NewWire())
		observations := make([]agent.NativeSubagent, 0, len(restored)+len(live))
		observations = append(observations, restored...)
		observations = append(observations, live...)
		cards, active := agenttest.FoldNativeSubagents(observations)
		if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusCompleted || active != 0 {
			t.Fatalf("cards = %#v active = %d, want one completed agent after restart", cards, active)
		}
		if cards[0].ID != restored[0].ID {
			t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
		}
	})
	t.Run("activity spawns are settled by collaboration states", func(t *testing.T) {
		t.Parallel()
		// Both item types address the same agent by thread ID, so a wait that
		// reports agent state must settle a card the activity path created, and
		// vice versa.
		wire := New("", nil).NewWire()
		spawn := activityItem("started", "call_1", "th1")
		settle := collabItem("item/completed", "wait", "item2", []string{"th1"}, "", map[string]codexAgentState{
			"th1": {Status: "completed", Message: new("joke delivered")},
		})
		got := collectNative(t, wire, spawn, settle)
		if len(got) != 2 || got[0].Status != agent.NativeSubagentStatusRunning || got[1].Status != agent.NativeSubagentStatusCompleted {
			t.Fatalf("observations = %#v, want running then completed", got)
		}
		if got[1].ID != got[0].ID || got[1].Result != "joke delivered" {
			t.Fatalf("settled = %#v, want the same identity with the reported message", got[1])
		}
		cards, active := agenttest.FoldNativeSubagents(got)
		if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusCompleted || active != 0 {
			t.Fatalf("cards = %#v active = %d, want one settled agent", cards, active)
		}
		// The reverse order must correlate the same way.
		reversed := collectNative(t, New("", nil).NewWire(), collabItem("item/completed", "spawnAgent", "item1", []string{"th2"}, "Do work", nil),
			activityItem("completed", "subagent-completed-1", "th2"))
		if len(reversed) != 2 || reversed[1].Status != agent.NativeSubagentStatusCompleted || reversed[0].ID != reversed[1].ID {
			t.Fatalf("reversed observations = %#v, want one settled agent", reversed)
		}
	})
	t.Run("unknown agent states produce no cards", func(t *testing.T) {
		t.Parallel()
		// A coordination call only settles a thread the wire has already seen as a
		// spawn, so a completed state for an unknown thread produces no card.
		got := collectNative(t, New("", nil).NewWire(), []byte(`{"method":"item/completed","params":{"item":{"id":"i","type":"collabAgentToolCall","tool":"spawnAgent","agentsStates":{"ghost":{"status":"completed"}}}}}`))
		if len(got) != 0 {
			t.Fatalf("unknown thread produced %d cards, want 0", len(got))
		}
	})
}
