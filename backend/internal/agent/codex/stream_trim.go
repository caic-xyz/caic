// Selects streamed Codex message and reasoning-summary deltas that a later item/completed repeats, for settled logs.

package codex

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/codex"
)

// streamItem identifies one item of one native thread. Item IDs are unique
// within a thread across turns.
type streamItem struct {
	threadID string
	itemID   string
}

// streamTrim drops agentMessage and reasoning-summary deltas of completed
// items. item/completed carries the agentMessage's full text and the
// reasoning item's full summary. An item that never completed, such as one
// interrupted by a stop request, keeps its deltas.
//
// item/commandExecution/outputDelta stays: the completed commandExecution
// result carries no output, so the deltas are the only projected copy, and
// aggregatedOutput was observed shorter than the deltas.
type streamTrim struct {
	completed map[streamItem]struct{}
}

// Observe records completed agentMessage and reasoning items.
func (s *streamTrim) Observe(line []byte) error {
	method, err := agent.JSONString(line, "method")
	if err != nil || codex.Method(method) != codex.MethodItemCompleted {
		return err
	}
	typ, err := agent.JSONString(line, "params", "item", "type")
	if err != nil || (codex.ItemType(typ) != codex.ItemTypeAgentMessage && codex.ItemType(typ) != codex.ItemTypeReasoning) {
		return err
	}
	var item streamItem
	if item.threadID, err = agent.JSONString(line, "params", "threadId"); err != nil {
		return err
	}
	if item.itemID, err = agent.JSONString(line, "params", "item", "id"); err != nil || item.itemID == "" {
		return err
	}
	s.completed[item] = struct{}{}
	return nil
}

// Superseded reports whether line is a delta its completed item repeats.
func (s *streamTrim) Superseded(line []byte) (bool, error) {
	method, err := agent.JSONString(line, "method")
	if err != nil {
		return false, err
	}
	if m := codex.Method(method); m != codex.MethodItemDelta && m != codex.MethodReasoningSummaryTextDelta {
		return false, nil
	}
	var item streamItem
	if item.threadID, err = agent.JSONString(line, "params", "threadId"); err != nil {
		return false, err
	}
	if item.itemID, err = agent.JSONString(line, "params", "itemId"); err != nil || item.itemID == "" {
		return false, err
	}
	_, completed := s.completed[item]
	return completed, nil
}
