// Selects streamed Pi content deltas that a later message_end repeats, for settled logs.

package pi

import (
	"errors"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/pi"
)

// streamBlock identifies one content block of one native message. Pi updates
// carry no message identity, so message counts the message_start and
// message_end boundaries before the block in log order.
type streamBlock struct {
	message int
	index   int64
}

// streamTrim drops content deltas of blocks that a message_end repeats. Pi
// streams each block as start, deltas and end updates, then emits the
// consolidated assistant message in message_end, which is the projection's only
// source of final text and thinking. A block is settled when its end update
// arrived and its message then reached a non-error assistant message_end. A
// block without an end, a message without message_end (crash, restart), and an
// error stop, whose message_end projects only the error, keep their deltas.
//
// The first text or thinking delta after any other native record also stays:
// it is the earliest activity of its run, which proves input acceptance and
// starts the fallback turn clock of a result without a native duration.
type streamTrim struct {
	settled map[streamBlock]struct{}
	ended   []int64 // Ended block indexes of the current observed message.
	// Each pass counts message boundaries independently over the same records.
	observed int
	decided  int
	// active is set once a text or thinking delta follows the last record
	// other than message_update.
	active bool
}

// Observe records settled blocks.
func (s *streamTrim) Observe(line []byte) error {
	ev, err := decodeStreamEvent(line)
	if err != nil {
		return err
	}
	switch ev.eventType {
	case pi.EventMessageStart:
		s.ended = s.ended[:0]
	case pi.EventMessageEnd:
		role, err := agent.JSONString(line, "message", "role")
		if err != nil {
			return err
		}
		stop, err := agent.JSONString(line, "message", "stopReason")
		if err != nil {
			return err
		}
		if pi.Role(role) == pi.RoleAssistant && pi.StopReason(stop) != pi.StopReasonError {
			for _, i := range s.ended {
				s.settled[streamBlock{message: s.observed, index: i}] = struct{}{}
			}
		}
		s.ended = s.ended[:0]
	case pi.EventMessageUpdate:
		switch ev.deltaType {
		case pi.DeltaTextEnd, pi.DeltaThinkEnd, pi.DeltaToolEnd:
			if ev.identified {
				s.ended = append(s.ended, ev.index)
			}
		default:
		}
	default:
	}
	s.observed += boundary(ev.eventType)
	return nil
}

// Superseded reports whether line is a delta of a settled block.
func (s *streamTrim) Superseded(line []byte) (bool, error) {
	ev, err := decodeStreamEvent(line)
	if err != nil {
		return false, err
	}
	block := streamBlock{message: s.decided, index: ev.index}
	s.decided += boundary(ev.eventType)
	if ev.eventType != pi.EventMessageUpdate {
		s.active = false
		return false, nil
	}
	switch ev.deltaType {
	case pi.DeltaTextDelta, pi.DeltaThinkDelta:
		if !s.active {
			s.active = true
			return false, nil
		}
	case pi.DeltaToolDelta:
	default:
		return false, nil
	}
	if !ev.identified {
		return false, nil
	}
	_, settled := s.settled[block]
	return settled, nil
}

// streamEvent is the discriminators and block index of one native record.
// identified is false for records other than message_update and for updates
// without a content index.
type streamEvent struct {
	eventType  pi.EventType
	deltaType  pi.DeltaType
	index      int64
	identified bool
}

// decodeStreamEvent reads only the discriminators and the content index:
// payloads are left undecoded so that a field this rule does not read cannot
// fail settlement.
func decodeStreamEvent(line []byte) (streamEvent, error) {
	var ev streamEvent
	typ, err := agent.JSONString(line, "type")
	if err != nil {
		return ev, err
	}
	ev.eventType = pi.EventType(typ)
	if ev.eventType != pi.EventMessageUpdate {
		return ev, nil
	}
	delta, err := agent.JSONString(line, "assistantMessageEvent", "type")
	if err != nil {
		return ev, err
	}
	ev.deltaType = pi.DeltaType(delta)
	ev.index, err = jsonparser.GetInt(line, "assistantMessageEvent", "contentIndex")
	if errors.Is(err, jsonparser.KeyPathNotFoundError) {
		return ev, nil
	}
	if err != nil {
		return ev, err
	}
	ev.identified = true
	return ev, nil
}

// boundary counts a message boundary. Counting message_end too keeps a stray
// update after it out of the settled message.
func boundary(t pi.EventType) int {
	if t == pi.EventMessageStart || t == pi.EventMessageEnd {
		return 1
	}
	return 0
}
