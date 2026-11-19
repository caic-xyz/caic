// Normalizes Codex spawn and coordination observations using native receiver identities, and processes that outlive their turn.

package codex

import (
	"encoding/json"
	"path"
	"slices"
	"strings"

	taskslogv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// decodedItem is the routed item of an item/started or item/completed
// notification. The canonical conversion keeps the item types it renders, so the
// parser hands the routed item over rather than making the adapter decode the
// notification again. The thread-level records are carried the same way: the
// adapter is the only reader of the child-thread lifecycle they report.
type decodedItem struct {
	raw           json.RawMessage
	activity      *codex.SubAgentActivityItem
	collaboration *codex.CollabAgentToolCallItem
	typ           codex.ItemType
	statusChanged *codex.ThreadStatusChangedNotification
	turnCompleted *codex.TurnCompletedNotification
	source        taskslogv3.MessageSource // Origin of the observation, distinct from its subject.
	threadStarted *codex.ThreadStartedNotification
}

// The card map is allocated with the wire, created once per session, instead of
// by the first record that stores a card.
type nativeSubagents struct {
	timeline agent.NativeSubagentTimeline
	spawns   map[string]agent.NativeSubagent // canonical identity → card
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

// codexRootAgentPath is the agent path Codex reports for a session's own root
// thread. A child can emit a sub-agent activity that targets the root when it
// interacts with its parent, but the root is not a native subagent and must
// never become a card.
const codexRootAgentPath = "/root"

// parse folds one routed item: a spawn proves an agent, coordination tools and
// child thread lifecycle only update threads the wire has already seen.
func (n *nativeSubagents) parse(item *decodedItem) ([]agent.Message, error) {
	switch {
	case item.statusChanged != nil:
		return n.parseThreadStatus(item.statusChanged), nil
	case item.turnCompleted != nil:
		return n.parseTurnCompleted(item.turnCompleted), nil
	}
	switch item.typ {
	case codex.ItemTypeSubAgentActivity:
		return n.parseActivity(item.activity), nil
	case codex.ItemTypeCollabAgentToolCall:
		if item.collaboration != nil {
			return n.observeCollabToolCall(item.collaboration), nil
		}
		return n.parseCollabToolCall(item.raw)
	default:
		return nil, nil
	}
}

// parseActivity maps a sub-agent lifecycle activity item, which is how Codex
// reports a spawned agent's lifecycle. Its identity is the agent thread ID, the
// same identity the collaboration receiver path uses, so both item types settle
// one card per agent.
func (n *nativeSubagents) parseActivity(item *codex.SubAgentActivityItem) []agent.Message {
	if item.AgentThreadID == "" || item.AgentPath == codexRootAgentPath {
		return nil
	}
	s, known := n.known(item.AgentThreadID)
	if !known {
		// Only a started activity proves an agent the wire has not already seen
		// spawn. An interaction, interruption, or completion is evidence about an
		// existing card and cannot invent one.
		if item.Kind != codex.SubAgentActivityKindStarted {
			return nil
		}
		s = agent.NativeSubagent{ID: codexThreadIdentity(item.AgentThreadID)}
	}
	if s.Label == "" && item.AgentPath != "" {
		// The path is the agent's own hierarchy path; its last segment is the
		// name the harness gave this agent.
		s.Label = path.Base(strings.TrimSuffix(item.AgentPath, "/"))
	}
	if item.Kind == codex.SubAgentActivityKindStarted {
		// The started activity's ID is the spawn call that produced this agent. A
		// collaborative agent runs independently of the parent turn.
		if item.ID != "" {
			s.ToolUseID = item.ID
		}
		s.Background = true
	}
	s.Status = subAgentActivityStatus(item.Kind)
	n.remember(item.AgentThreadID, &s)
	return n.timeline.Observe(&s)
}

// parseThreadStatus tracks a spawned child thread's own lifecycle. Codex
// reports its root thread through the same notification, but the root never has
// a card, so only known children are updated. Active means a turn is in flight;
// idle means no turn is in flight but the agent is still resumable, which is the
// canonical paused (non-terminal) state rather than a terminal outcome.
func (n *nativeSubagents) parseThreadStatus(ev *codex.ThreadStatusChangedNotification) []agent.Message {
	s, known := n.known(ev.ThreadID)
	if !known || s.Status.Terminal() {
		return nil
	}
	switch ev.Status.Type {
	case codex.ThreadStatusTypeActive:
		s.Status = agent.NativeSubagentStatusRunning
	case codex.ThreadStatusTypeIdle:
		s.Status = agent.NativeSubagentStatusPaused
	case codex.ThreadStatusTypeSystemError:
		s.Status = agent.NativeSubagentStatusFailed
	default:
		return nil
	}
	n.remember(ev.ThreadID, &s)
	return n.timeline.Observe(&s)
}

// parseTurnCompleted settles a child thread's failed or interrupted turn. A
// completed turn is not terminal: the child's thread status then reports idle,
// which stays resumable. Only a known child card is updated.
func (n *nativeSubagents) parseTurnCompleted(ev *codex.TurnCompletedNotification) []agent.Message {
	s, known := n.known(ev.ThreadID)
	if !known || s.Status.Terminal() {
		return nil
	}
	switch ev.Turn.Status {
	case codex.TurnStatusFailed:
		s.Status = agent.NativeSubagentStatusFailed
	case codex.TurnStatusInterrupted:
		s.Status = agent.NativeSubagentStatusInterrupted
	default:
		return nil
	}
	n.remember(ev.ThreadID, &s)
	return n.timeline.Observe(&s)
}

// parseCollabToolCall maps collaboration tool calls: a spawn with receiver
// threads proves agents, while coordination tools only update threads the wire
// has already seen.
func (n *nativeSubagents) parseCollabToolCall(raw json.RawMessage) ([]agent.Message, error) {
	var item codex.CollabAgentToolCallItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	return n.observeCollabToolCall(&item), nil
}

func (n *nativeSubagents) observeCollabToolCall(item *codex.CollabAgentToolCallItem) []agent.Message {
	var out []agent.Message
	// Only the app-server spawn tool proves a spawn. The remaining
	// collaborative tools (wait, sendInput, resumeAgent, closeAgent, ...) and
	// the receiverless cases handled below update known threads only.
	if item.Tool == codex.CollabAgentToolSpawnAgent {
		for _, id := range item.ReceiverThreadIDs {
			if id == "" {
				continue
			}
			s := agent.NativeSubagent{ID: codexThreadIdentity(id), ToolUseID: item.ID, GroupID: item.ID, Prompt: item.Prompt, Status: agent.NativeSubagentStatusUnknown, Background: true}
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
	return out
}

// backgroundCommands folds unified-exec processes that outlive their turn
// into canonical background commands. Codex runs every command as a
// unified-exec process; when the turn ends while the process still runs, the
// process keeps running and Codex reports its exit later with an item/completed
// that carries the original turn ID. A command that settles inside its turn
// never becomes a card. Child threads follow the same lifecycle: their
// processes report their exits, including signal exits when the child shuts
// down, and a host exit retires the rest.
//
// Codex starts no turn when such a process ends, so a card never wakes the
// agent and never keeps the task running.
//
// Only the full projection carries the item payload; the metadata projection
// tracks no command.
type backgroundCommands struct {
	timeline agent.BackgroundCommandTimeline
	running  []runningCommand // Command items started and not completed, in start order.
}

// parse folds one routed record. The caller emits a command's completion after
// the late tool result of the same record.
func (b *backgroundCommands) parse(method codex.Method, record *decodedItem) ([]agent.Message, error) {
	if record.turnCompleted != nil {
		return b.detach(record.source.ThreadID, record.source.TurnID), nil
	}
	if record.typ != codex.ItemTypeCommandExecution || record.raw == nil {
		return nil, nil
	}
	key := streamItem{threadID: record.source.ThreadID, itemID: record.source.ItemID}
	switch method {
	case codex.MethodItemStarted:
		var item codex.CommandExecutionItem
		if err := json.Unmarshal(record.raw, &item); err != nil {
			return nil, err
		}
		// A relay replay can repeat the start of an item the wire already holds.
		c := runningCommand{item: key, turnID: record.source.TurnID, label: item.Command}
		if i := slices.IndexFunc(b.running, func(c runningCommand) bool { return c.item == key }); i >= 0 {
			b.running[i] = c
		} else {
			b.running = append(b.running, c)
		}
		return nil, nil
	case codex.MethodItemCompleted:
		i := slices.IndexFunc(b.running, func(c runningCommand) bool { return c.item == key })
		if i < 0 {
			return nil, nil
		}
		detached := b.running[i].detached
		b.running = slices.Delete(b.running, i, i+1)
		if !detached {
			return nil, nil
		}
		var item codex.CommandExecutionItem
		if err := json.Unmarshal(record.raw, &item); err != nil {
			return nil, err
		}
		s := agent.BackgroundCommand{ID: codexCommandIdentity(key), Status: codexBackgroundCommandStatus(item.Status), ExitCode: item.ExitCode}
		return b.timeline.Observe(&s), nil
	default:
		return nil, nil
	}
}

// detach marks the running commands of a completed turn as background
// commands.
func (b *backgroundCommands) detach(threadID, turnID string) []agent.Message {
	var out []agent.Message
	for i := range b.running {
		c := &b.running[i]
		if c.detached || c.item.threadID != threadID || c.turnID != turnID {
			continue
		}
		c.detached = true
		s := agent.BackgroundCommand{ID: codexCommandIdentity(c.item), Label: c.label, Status: agent.BackgroundCommandStatusRunning, ToolUseID: c.item.itemID}
		out = append(out, b.timeline.Observe(&s)...)
	}
	return out
}

// reset forgets the commands of a Codex process that a new one replaced; their
// exits can no longer reach the wire.
func (b *backgroundCommands) reset() {
	b.running = nil
}

// runningCommand is one command item in flight.
type runningCommand struct {
	item     streamItem
	turnID   string
	label    string
	detached bool // Its turn completed: it is a background command card.
}

// codexCommandIdentity is the canonical card identity for a Codex command
// item. Item IDs are unique only within their thread.
func codexCommandIdentity(item streamItem) string {
	return "codex:exec:" + item.threadID + ":" + item.itemID
}

// codexBackgroundCommandStatus maps a completed command item's status.
func codexBackgroundCommandStatus(status codex.CommandExecutionStatus) agent.BackgroundCommandStatus {
	switch status {
	case codex.CommandExecutionStatusCompleted:
		return agent.BackgroundCommandStatusCompleted
	case codex.CommandExecutionStatusFailed, codex.CommandExecutionStatusDeclined:
		return agent.BackgroundCommandStatusFailed
	default:
		return agent.BackgroundCommandStatusUnknown
	}
}

// codexThreadIdentity is the canonical card identity for a Codex agent thread.
// Both the activity items and the collaboration receiver/state maps address the
// same agent by thread ID, so every writer and reader of spawns must use this key
// or a completion reported by one path would never settle a card created by the
// other.
func codexThreadIdentity(threadID string) string {
	return "codex:thread:" + threadID
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
