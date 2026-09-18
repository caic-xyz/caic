// Normalizes Codex spawn and coordination observations using native receiver identities.

package codex

import (
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// decodedItem is the routed item of an item/started or item/completed
// notification. The canonical conversion keeps the item types it renders, so the
// parser hands the routed item over rather than making the adapter decode the
// notification again.
type decodedItem struct {
	raw json.RawMessage
	typ codex.ItemType
}

type nativeSubagents struct {
	timeline agent.NativeSubagentTimeline
	spawns   map[string]agent.NativeSubagent // canonical identity → card
}

// newNativeSubagents returns an adapter whose card map is ready. A wire is
// created once per session, so the map is allocated with it instead of being
// created by the first record that stores a card.
func newNativeSubagents() nativeSubagents {
	return nativeSubagents{spawns: make(map[string]agent.NativeSubagent)}
}

// codexThreadIdentity is the canonical card identity for a Codex agent thread.
// Both the activity items and the collaboration receiver/state maps address the
// same agent by thread ID, so every writer and reader of spawns must use this key
// or a completion reported by one path would never settle a card created by the
// other.
func codexThreadIdentity(threadID string) string {
	return "codex:thread:" + threadID
}

// known returns the card for a thread and whether the session has seen it.
func (n *nativeSubagents) known(threadID string) (agent.NativeSubagent, bool) {
	s, ok := n.spawns[codexThreadIdentity(threadID)]
	return s, ok
}

// remember stores a card under its canonical identity.
func (n *nativeSubagents) remember(threadID string, s *agent.NativeSubagent) {
	n.spawns[codexThreadIdentity(threadID)] = *s
}

// parse folds one routed item: a spawn proves an agent, and coordination tools
// only update threads the wire has already seen.
func (n *nativeSubagents) parse(item decodedItem) ([]agent.Message, error) {
	switch item.typ {
	case codex.ItemTypeSubAgentActivity:
		return n.parseActivity(item.raw)
	case codex.ItemTypeCollabAgentToolCall:
		return n.parseCollabToolCall(item.raw)
	default:
		return nil, nil
	}
}

// parseActivity maps a sub-agent lifecycle activity item, which is how Codex
// reports a spawned agent's lifecycle. Its identity is the agent thread ID, the
// same identity the collaboration receiver path uses, so both item types settle
// one card per agent.
func (n *nativeSubagents) parseActivity(raw json.RawMessage) ([]agent.Message, error) {
	var item codex.SubAgentActivityItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	if item.AgentThreadID == "" {
		return nil, nil
	}
	s, known := n.known(item.AgentThreadID)
	if !known {
		s = agent.NativeSubagent{ID: codexThreadIdentity(item.AgentThreadID)}
	}
	if s.Label == "" && item.AgentPath != "" {
		// The path is the agent's own hierarchy path; its last segment is the
		// name the harness gave this agent.
		s.Label = path.Base(strings.TrimSuffix(item.AgentPath, "/"))
	}
	if item.Kind == codex.SubAgentActivityKindStarted && item.ID != "" {
		// The started activity's ID is the spawn call that produced this agent.
		s.ToolUseID = item.ID
	}
	s.Status = subAgentActivityStatus(item.Kind)
	n.remember(item.AgentThreadID, &s)
	return n.timeline.Observe(&s), nil
}

// parseCollabToolCall maps collaboration tool calls: a spawn with receiver
// threads proves agents, while coordination tools only update threads the wire
// has already seen.
func (n *nativeSubagents) parseCollabToolCall(raw json.RawMessage) ([]agent.Message, error) {
	var item codex.CollabAgentToolCallItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	var out []agent.Message
	// Only the app-server spawn tool proves a spawn. The remaining
	// collaborative tools (wait, sendInput, resumeAgent, closeAgent, ...) and
	// the receiverless cases handled below update known threads only.
	if item.Tool == codex.CollabAgentToolSpawnAgent {
		for _, id := range item.ReceiverThreadIDs {
			if id == "" {
				continue
			}
			s := agent.NativeSubagent{ID: codexThreadIdentity(id), ToolUseID: item.ID, GroupID: item.ID, Prompt: item.Prompt, Status: agent.NativeSubagentStatusUnknown}
			n.remember(id, &s)
			out = append(out, n.timeline.Observe(&s)...)
		}
	}
	// Sort map keys: semantic event order must be identical in live and replay.
	ids := make([]string, 0, len(item.AgentsStates))
	for id := range item.AgentsStates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s, known := n.known(id)
		if !known {
			continue
		} // wait/resume/close cannot invent a spawn.
		state := item.AgentsStates[id]
		s.Status = codexSubagentStatus(state.Status)
		if state.Message != nil {
			s.Result = *state.Message
		}
		n.remember(id, &s)
		out = append(out, n.timeline.Observe(&s)...)
	}
	return out, nil
}

// subAgentActivityStatus maps a reported activity kind onto the canonical
// lifecycle. An interaction proves the agent is alive, an interruption ends it,
// and an unrecognized kind adds no state.
func subAgentActivityStatus(kind codex.SubAgentActivityKind) agent.NativeSubagentStatus {
	switch kind {
	case codex.SubAgentActivityKindStarted, codex.SubAgentActivityKindInteracted:
		return agent.NativeSubagentStatusRunning
	case codex.SubAgentActivityKindInterrupted:
		return agent.NativeSubagentStatusInterrupted
	case codex.SubAgentActivityKindCompleted:
		return agent.NativeSubagentStatusCompleted
	default:
		return agent.NativeSubagentStatusUnknown
	}
}

func codexSubagentStatus(status codex.CollabAgentStatus) agent.NativeSubagentStatus {
	switch status {
	case codex.CollabAgentStatusRunning:
		return agent.NativeSubagentStatusRunning
	case codex.CollabAgentStatusCompleted:
		return agent.NativeSubagentStatusCompleted
	case codex.CollabAgentStatusErrored:
		return agent.NativeSubagentStatusFailed
	case codex.CollabAgentStatusInterrupted, codex.CollabAgentStatusShutdown:
		return agent.NativeSubagentStatusInterrupted
	default:
		return agent.NativeSubagentStatusUnknown
	}
}
