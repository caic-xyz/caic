// Selects streamed Claude content deltas that a later final assistant record repeats, for settled logs.

package claudecode

import (
	"errors"

	"github.com/buger/jsonparser"
	"github.com/maruel/genai/providers/claudecode"
)

// streamBlock identifies one content block of one API message in one
// delegation namespace.
type streamBlock struct {
	sessionID       string
	parentToolUseID string
	apiMessageID    string
	index           int
}

// streamTrim drops content deltas of stopped blocks. Claude emits the complete
// block in an assistant record once the block stops: text, tool input, and the
// thinking signature. Thinking text is the exception: the final thinking block
// carries only its signature, so thinking deltas are the sole copy and stay. A
// block that never stopped, such as one interrupted by a stop request, has no
// final record and keeps its deltas.
type streamTrim struct {
	stopped map[streamBlock]struct{}
}

// Observe records stopped blocks.
func (s *streamTrim) Observe(line []byte) error {
	ev, err := decodeStreamBlock(line)
	if err != nil || !ev.identified || ev.eventType != "content_block_stop" {
		return err
	}
	s.stopped[ev.block] = struct{}{}
	return nil
}

// Superseded reports whether line is a delta repeated by its stopped block's
// final assistant record.
func (s *streamTrim) Superseded(line []byte) (bool, error) {
	ev, err := decodeStreamBlock(line)
	if err != nil || !ev.identified || ev.eventType != "content_block_delta" {
		return false, err
	}
	switch ev.deltaType {
	case "text_delta", "input_json_delta", "signature_delta":
		_, stopped := s.stopped[ev.block]
		return stopped, nil
	default:
		return false, nil
	}
}

// streamBlockEvent is the block identity and event discriminators of one
// stream event. identified is false for other records, for message-level
// events, and for block events without an API message identity or index:
// blocks of successive messages share indexes and cannot be matched to their
// final record without both.
type streamBlockEvent struct {
	block      streamBlock
	eventType  string
	deltaType  string
	identified bool
}

// decodeStreamBlock reads only the identity and discriminators: payloads are
// left undecoded so that a field this rule does not read cannot fail
// settlement.
func decodeStreamBlock(line []byte) (streamBlockEvent, error) {
	var ev streamBlockEvent
	typ, err := borrowedString(line, "type")
	if err != nil || string(typ) != string(claudecode.OutputStreamEvent) {
		return ev, err
	}
	if ev.block.apiMessageID, err = copiedString(line, "api_message_id"); err != nil {
		return ev, err
	}
	if ev.block.sessionID, err = copiedString(line, "session_id"); err != nil {
		return ev, err
	}
	if ev.block.parentToolUseID, err = copiedString(line, "parent_tool_use_id"); err != nil {
		return ev, err
	}
	if ev.eventType, err = copiedString(line, "event", "type"); err != nil {
		return ev, err
	}
	if ev.deltaType, err = copiedString(line, "event", "delta", "type"); err != nil {
		return ev, err
	}
	if ev.block.apiMessageID == "" || (ev.eventType != "content_block_delta" && ev.eventType != "content_block_stop") {
		return ev, nil
	}
	index, err := jsonparser.GetInt(line, "event", "index")
	if errors.Is(err, jsonparser.KeyPathNotFoundError) {
		return ev, nil
	}
	if err != nil {
		return ev, err
	}
	ev.block.index = int(index)
	ev.identified = true
	return ev, nil
}

// copiedString reads an optional string field into memory the line does not own.
func copiedString(line []byte, keys ...string) (string, error) {
	v, err := borrowedString(line, keys...)
	return string(v), err
}
