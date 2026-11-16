// Selects streamed Codex text, reasoning, and command-output deltas that item/completed repeats, for settled logs.

package codex

import (
	"bytes"
	"crypto/sha256"
	"hash"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/codex"
)

// streamItem identifies one item of one native thread. Item IDs are unique
// within a thread across turns.
type streamItem struct {
	threadID string
	itemID   string
}

// streamTrim drops deltas of completed items that item/completed repeats: an
// agentMessage's full text, a reasoning item's full summary, and a
// commandExecution's aggregatedOutput. An item that never completed, such as
// one interrupted by a stop request, keeps its deltas.
//
// A command's output deltas go only when they concatenate to exactly its
// aggregatedOutput, which the wire then projects in their place (see
// wireFormat.commandOutput). Where aggregatedOutput differs, usually by a
// prefix the deltas lack, the deltas stay.
type streamTrim struct {
	completed map[streamItem]struct{}
	output    map[streamItem]hash.Hash // Streamed output of running commands.
}

// Observe records completed items whose deltas the completion repeats.
func (s *streamTrim) Observe(line []byte) error {
	method, err := agent.JSONString(line, "method")
	if err != nil {
		return err
	}
	switch codex.Method(method) {
	case codex.MethodCommandOutputDelta:
		item, err := deltaItem(line)
		if err != nil || item.itemID == "" {
			return err
		}
		delta, err := agent.JSONString(line, "params", "delta")
		if err != nil {
			return err
		}
		h := s.output[item]
		if h == nil {
			h = sha256.New()
			s.output[item] = h
		}
		_, _ = h.Write([]byte(delta))
		return nil
	case codex.MethodItemCompleted:
	default:
		return nil
	}
	typ, err := agent.JSONString(line, "params", "item", "type")
	if err != nil {
		return err
	}
	var item streamItem
	if item.threadID, err = agent.JSONString(line, "params", "threadId"); err != nil {
		return err
	}
	if item.itemID, err = agent.JSONString(line, "params", "item", "id"); err != nil || item.itemID == "" {
		return err
	}
	switch codex.ItemType(typ) {
	case codex.ItemTypeAgentMessage, codex.ItemTypeReasoning:
	case codex.ItemTypeCommandExecution:
		streamed := s.output[item]
		delete(s.output, item)
		if streamed == nil {
			return nil
		}
		output, err := agent.JSONString(line, "params", "item", "aggregatedOutput")
		if err != nil {
			return err
		}
		if sum := sha256.Sum256([]byte(output)); !bytes.Equal(streamed.Sum(nil), sum[:]) {
			return nil
		}
	default:
		return nil
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
	switch codex.Method(method) {
	case codex.MethodItemDelta, codex.MethodReasoningSummaryTextDelta, codex.MethodCommandOutputDelta:
	default:
		return false, nil
	}
	item, err := deltaItem(line)
	if err != nil || item.itemID == "" {
		return false, err
	}
	_, completed := s.completed[item]
	return completed, nil
}

// deltaItem reads the item a delta notification streams.
func deltaItem(line []byte) (streamItem, error) {
	var item streamItem
	var err error
	if item.threadID, err = agent.JSONString(line, "params", "threadId"); err != nil {
		return item, err
	}
	item.itemID, err = agent.JSONString(line, "params", "itemId")
	return item, err
}
