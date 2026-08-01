// Legacy v1 task-log record decoding.

package agent

import (
	"encoding/json"
	"time"
)

type v1RecordDiscriminator struct {
	Type string `json:"type"`
}

func parseV1Record(p *LogRecordParser, line []byte) ([]ParsedMessage, error) {
	var envelope v1RecordDiscriminator
	if err := json.Unmarshal(line, &envelope); err != nil {
		msgs, parseErr := p.parseAndApplyNative(line)
		return wrapParsedMessages(msgs, time.Time{}), parseErr
	}
	kind, ok := p.controlKind(envelope.Type)
	if ok {
		msgs, err := p.parseControl(kind, envelope.Type, line)
		if err != nil {
			return nil, err
		}
		msgs, err = p.applyMessageState(msgs)
		return wrapParsedMessages(msgs, time.Time{}), err
	}
	msgs, err := p.parseAndApplyNative(line)
	return wrapParsedMessages(msgs, time.Time{}), err
}
