// Encodes semantic controls and forwards exact native records to physical journal writers.

package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// MarshalLogMessage encodes one semantic backend control as a v3 record.
func MarshalLogMessage(m Message) ([]byte, error) {
	if _, ok := m.(*RawMessage); ok {
		return nil, errors.New("raw messages must be appended as native records")
	}
	token, err := v3ControlToken(m)
	if err != nil {
		return nil, err
	}
	var record any
	switch m := m.(type) {
	case *TextMessage:
		r := v3.TextMessage(*m)
		r.MessageType = string(token)
		record = r
	case *UserInputMessage:
		r := v3.UserInputMessage(*m)
		r.MessageType = string(token)
		record = r
	case *SystemMessage:
		r := v3.SystemMessage(*m)
		r.MessageType = string(token)
		record = r
	case *LogMessage:
		r := v3.LogMessage(*m)
		r.MessageType = string(token)
		record = r
	case *StrippedEnvMessage:
		r := v3.StrippedEnvMessage(*m)
		r.MessageType = string(token)
		record = r
	case *DiffStatMessage:
		r := v3.DiffStatMessage(*m)
		r.MessageType = string(token)
		record = r
	case *ExitMessage:
		r := v3.ExitMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaSessionMessage:
		r := v3.MetaSessionMessage(*m)
		r.MessageType = string(token)
		record = r
	case *RelayGenerationMessage:
		r := v3.RelayGenerationMessage(*m)
		r.MessageType = string(token)
		record = r
	case *ModelInfoMessage:
		r := v3.ModelInfoMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaResultMessage:
		r := v3.MetaResultMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaPRMessage:
		r := v3.MetaPRMessage(*m)
		r.MessageType = string(token)
		record = r
	case *TurnCommitSnapshotMessage:
		r := v3.TurnCommitSnapshotMessage(*m)
		r.MessageType = string(token)
		record = r
	case *PendingUserActionMessage:
		r := v3.PendingUserActionMessage(*m)
		r.MessageType = string(token)
		record = r
	default:
		return nil, fmt.Errorf("message type %q is not a task-log control", m.Type())
	}
	// Released Go controls use lexicographically sorted keys.
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// RecordedLog encodes runtime controls over an owned physical byte writer.
type RecordedLog struct{ writer taskslog.Writer }

// NewRecordedLog transfers the writer's close ownership to the agent recorder.
func NewRecordedLog(writer taskslog.Writer) *RecordedLog { return &RecordedLog{writer: writer} }

// AppendRecord forwards exact bytes; append provenance belongs to the task owner.
func (l *RecordedLog) AppendRecord(record PhysicalRecord) error {
	return l.writer.Append(record.Encoded)
}

// AppendMessage encodes one caic-owned control before writing it.
func (l *RecordedLog) AppendMessage(m Message) error {
	data, err := MarshalLogMessage(m)
	if err != nil {
		return err
	}
	return l.writer.Append(append(data, '\n'))
}

// Close closes the owned byte writer.
func (l *RecordedLog) Close() error { return l.writer.Close() }
