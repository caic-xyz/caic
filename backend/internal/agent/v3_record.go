// Converts validated v3 frames to ordered agent events and relay provenance.

package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// FrameProvenance classifies a validated relay frame without provider decoding.
// Local appends and import batches choose local provenance at their owner.
func FrameProvenance(frame *taskslog.Frame) RecordProvenance {
	var p RecordProvenance
	switch frame.Kind { //nolint:exhaustive // Only relay-owned kinds carry remote cursor provenance.
	case taskslog.KindAgent, taskslog.KindDiffStat, taskslog.KindExit, taskslog.KindStrippedEnv, taskslog.KindMCPRequest, taskslog.KindRelayGeneration:
		p.Source = RecordSourceRelay
	}
	if m, ok := frame.Control.(*v3.RelayGenerationMessage); ok {
		p.Generation = m.Generation
	}
	return p
}

// LogRecordParser interprets validated frames with ordered native protocol state.
// ParseRecord also performs physical validation for live relay readers.
// A parser is not safe for concurrent use.
type LogRecordParser struct {
	native        MessageParser
	contextWindow int
}

// NewLogRecordParser constructs a v3 task-log parser.
// native owns ordered input/output protocol state for the complete stream.
func NewLogRecordParser(native MessageParser) (*LogRecordParser, error) {
	if native == nil {
		return nil, errors.New("native message parser is nil")
	}
	return &LogRecordParser{native: native}, nil
}

// ParseRecord decodes one v3 physical task-log record.
// Classification belongs to this parser so task-log consumers
// never duplicate or guess control vocabulary.
func (p *LogRecordParser) ParseRecord(line []byte) (ParsedRecord, error) {
	frame, err := taskslog.DecodeFrame(line)
	if err != nil {
		return ParsedRecord{Control: frame.Kind != "" && frame.Kind != taskslog.KindAgent && frame.Kind != taskslog.KindInput, Provenance: FrameProvenance(&frame)}, err
	}
	return p.DecodeFrame(&frame)
}

// DecodeFrame interprets a validated frame with this scan's ordered native state.
func (p *LogRecordParser) DecodeFrame(frame *taskslog.Frame) (ParsedRecord, error) {
	record := ParsedRecord{Provenance: FrameProvenance(frame), Control: frame.Control != nil}
	if !record.Control {
		msgs, err := p.parseAndApplyNative(frame.Native, frame.Kind == taskslog.KindInput)
		record.Messages = wrapParsedMessages(msgs, frame.ProducerTime)
		return record, err
	}
	var msgs []Message
	switch m := frame.Control.(type) {
	case *v3.MetaMessage:
		msgs = []Message{new(MetaMessage(*m))}
	case *v3.DiffStatMessage:
		msg := DiffStatMessage(*m)
		msg.MessageType = messageTypeDiffStat
		msgs = []Message{&msg}
	case *v3.ExitMessage:
		msg := ExitMessage(*m)
		msg.MessageType = messageTypeExit
		msgs = []Message{&msg}
	case *v3.StrippedEnvMessage:
		msg := StrippedEnvMessage(*m)
		msg.MessageType = messageTypeStrippedEnv
		msgs = []Message{&msg}
	case *v3.MetaSessionMessage:
		msg := MetaSessionMessage(*m)
		msg.MessageType = messageTypeSession
		msgs = []Message{&msg}
	case *v3.MetaPRMessage:
		msg := MetaPRMessage(*m)
		msg.MessageType = messageTypePR
		msgs = []Message{&msg}
	case *v3.TurnCommitSnapshotMessage:
		msg := TurnCommitSnapshotMessage(*m)
		msg.MessageType = messageTypeTurnCommitSnapshot
		msgs = []Message{&msg}
	case *v3.PendingUserActionMessage:
		msg := PendingUserActionMessage(*m)
		msg.MessageType = PendingUserActionMessageType
		msgs = []Message{&msg}
	case *v3.LogMessage:
		msg := LogMessage(*m)
		msg.MessageType = messageTypeProvisioningLogRecord
		msgs = []Message{&msg}
	case *v3.TextMessage:
		msgs = []Message{new(TextMessage(*m))}
	case *v3.UserInputMessage:
		msgs = []Message{new(UserInputMessage(*m))}
	case *v3.MCPRequestMessage:
		msgs = []Message{new(MCPRequestMessage(*m))}

	case *v3.MetaResultMessage:
		msg := MetaResultMessage(*m)
		msg.MessageType = messageTypeResult
		msgs = []Message{&msg}
	case *v3.ModelInfoMessage:
		if m.ContextWindow > 0 {
			p.contextWindow = int(m.ContextWindow)
		}
	case *v3.RelayGenerationMessage:
	case *v3.SystemMessage:
		msg := SystemMessage(*m)
		if frame.Kind == taskslog.KindContextCleared {
			msg = *ContextCleared()
			msg.MessageProvenance = m.MessageProvenance
		} else {
			msg.MessageType = messageTypeSystem
		}
		msgs = []Message{&msg}
	}
	var err error
	msgs, err = p.applyMessageState(msgs)
	record.Messages = wrapParsedMessages(msgs, frame.ProducerTime)
	return record, err
}

func (p *LogRecordParser) parseAndApplyNative(line []byte, input bool) ([]Message, error) {
	var msgs []Message
	var err error
	if input {
		msgs, err = p.native.ParseInput(line)
	} else {
		msgs, err = p.native.ParseOutput(line)
	}
	if err != nil {
		return nil, err
	}
	return p.applyMessageState(msgs)
}

func wrapParsedMessages(msgs []Message, producerTime time.Time) []TimedMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]TimedMessage, len(msgs))
	for i, msg := range msgs {
		out[i] = TimedMessage{Message: msg, ProducerTime: producerTime}
	}
	return out
}

func (p *LogRecordParser) applyMessageState(msgs []Message) ([]Message, error) {
	for i, msg := range msgs {
		if messageIsNil(msg) {
			return nil, fmt.Errorf("native message %d is nil", i)
		}
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	out := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		if m, ok := msg.(*UsageMessage); ok && !m.Source.IsChild() && m.ContextWindow == 0 && p.contextWindow > 0 {
			m.ContextWindow = p.contextWindow
		}
		out = append(out, msg)
	}
	return out, nil
}

// ParsedRecord is a parser-owned semantic task-log record. Control reports
// whether the physical discriminator identifies a caic-owned control.
type ParsedRecord struct {
	Messages   []TimedMessage
	Provenance RecordProvenance
	Control    bool
}
