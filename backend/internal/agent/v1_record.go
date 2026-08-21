// Legacy v1 task-log record decoding.

package agent

import (
	"encoding/json"
	"time"
)

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
