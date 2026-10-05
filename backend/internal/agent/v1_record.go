// Legacy v1 task-log record decoding through the frozen physical schema.

package agent

import (
	"encoding/json"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v1"
)

// DecodeV1MetaMessage decodes a legacy v1 metadata header into canonical fields.
func DecodeV1MetaMessage(line []byte) (MetaMessage, error) {
	record, err := decodeLogData[v1.MetaMessage](line, false)
	if err != nil {
		return MetaMessage{}, err
	}
	return logV1MetaMessageFromData(&record), err
}

// DecodeV1MetaSessionMessage decodes legacy v1 session metadata into canonical fields.
func DecodeV1MetaSessionMessage(line []byte) (MetaSessionMessage, error) {
	record, err := decodeLogData[v1.MetaSessionMessage](line, false)
	if err != nil {
		return MetaSessionMessage{}, err
	}
	return MetaSessionMessage(record), err
}

type v1RecordDiscriminator struct {
	Type string `json:"type"`
}

func parseV1Record(p *LogRecordParser, line []byte) (ParsedRecord, error) {
	var envelope v1RecordDiscriminator
	if err := json.Unmarshal(line, &envelope); err != nil {
		msgs, parseErr := p.parseAndApplyNative(line)
		return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{})}, parseErr
	}
	kind, ok := v1LogControlKinds[envelope.Type]
	if ok {
		msgs, err := p.parseControl(kind, envelope.Type, line)
		if err != nil {
			return ParsedRecord{Control: true}, err
		}
		msgs, err = p.applyMessageState(msgs)
		return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{}), Control: true}, err
	}
	msgs, err := p.parseAndApplyNative(line)
	return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{})}, err
}
