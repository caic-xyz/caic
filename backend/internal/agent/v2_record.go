// Strict canonical v2 task-log record extraction and decoding.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

var v2MetaMessageFields = jsonutil.KnownFields(MetaMessage{})

const (
	v2AgentRecordPrefix   = `{"t":"agent","ts":`
	v2AgentMessagePrefix  = `,"msg":`
	v2MaxEncodedRecordLen = 32 << 20
	v2MaxUnixSeconds      = int64(^uint64(0)>>1) - 62_135_596_800
)

// parseV2Record validates and decodes one canonical v2 physical record.
// Agent records use the zero-copy fast path; control records use the general
// decoder and update parser state before the resulting messages are returned.
func parseV2Record(p *LogRecordParser, line []byte) (ParsedRecord, error) {
	if err := validateV2RecordBytes(line); err != nil {
		return ParsedRecord{}, err
	}
	if bytes.HasPrefix(line, []byte(v2AgentRecordPrefix)) {
		msgs, err := parseV2AgentRecord(p, line)
		return ParsedRecord{Messages: msgs}, err
	}

	// Controls are small and retain ordinary decoding. Canonical agent records
	// take the branch above, so no valid agent envelope reaches this decoder.
	token, fields, err := decodeV2ControlFields(line)
	if err != nil {
		return ParsedRecord{}, fmt.Errorf("corrupt v2 log record: decode control discriminator: %w", err)
	}
	if token == "" {
		if containsV2ControlField(fields, "type") {
			return ParsedRecord{}, errors.New("corrupt v2 log record: unknown top-level field \"type\": top-level type discriminator is not valid in v2")
		}
		return ParsedRecord{}, errors.New("corrupt v2 log record: missing top-level t")
	}
	if token == "agent" {
		return ParsedRecord{}, errors.New("corrupt v2 agent record: noncanonical envelope")
	}
	kind, ok := p.controlKind(token)
	if !ok {
		return ParsedRecord{}, fmt.Errorf("corrupt v2 log record: unknown top-level t %q", token)
	}
	record := ParsedRecord{Control: true}
	if err := validateV2ControlFields(kind, token, fields); err != nil {
		return record, err
	}
	msgs, err := parseV2Control(p, kind, token, line)
	if err != nil {
		return record, err
	}
	msgs, err = p.applyMessageState(msgs)
	record.Messages = wrapParsedMessages(msgs, time.Time{})
	return record, err
}

func decodeV2ControlFields(line []byte) (token string, fields []string, err error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	opening, err := decoder.Token()
	if err != nil {
		return "", nil, err
	}
	if opening != json.Delim('{') {
		return "", nil, errors.New("record must be a JSON object")
	}

	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", nil, err
		}
		field, ok := keyToken.(string)
		if !ok {
			return "", nil, errors.New("record field name is not a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return "", nil, fmt.Errorf("duplicate top-level field %q", field)
		}
		seen[field] = struct{}{}
		fields = append(fields, field)

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", nil, err
		}
		if field == "t" {
			if err := json.Unmarshal(value, &token); err != nil {
				return "", nil, fmt.Errorf("top-level t must be a string: %w", err)
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return "", nil, err
	}
	if closing != json.Delim('}') {
		return "", nil, errors.New("record has an invalid closing delimiter")
	}
	if trailing, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return "", nil, fmt.Errorf("invalid trailing data: %w", err)
		}
		return "", nil, fmt.Errorf("unexpected trailing token %v", trailing)
	}
	return token, fields, nil
}

func containsV2ControlField(fields []string, want string) bool {
	return slices.Contains(fields, want)
}

func validateV2ControlFields(kind logControlKind, token string, fields []string) error {
	for _, field := range fields {
		if v2ControlFieldAllowed(kind, field) {
			continue
		}
		if field == "type" {
			return errors.New("corrupt v2 log record: unknown top-level field \"type\": top-level type discriminator is not valid in v2")
		}
		return fmt.Errorf("corrupt v2 %s record: unknown top-level field %q", token, field)
	}
	return nil
}

func v2ControlFieldAllowed(kind logControlKind, field string) bool {
	if field == "t" {
		return true
	}
	switch kind {
	case logControlMeta:
		// V2 substitutes the canonical t discriminator for MetaMessage's V1
		// type field; every remaining allowed key comes from MetaMessage itself.
		_, known := v2MetaMessageFields[field]
		return field != "type" && known
	case logControlDiffStat:
		return field == "diff_stat" || field == "ts"
	case logControlExit:
		switch field {
		case "cmd", "error", "exit_code", "signal", "stderr_truncated", "ts":
			return true
		}
	case logControlStrippedEnv:
		return field == "variables"
	case logControlSession:
		switch field {
		case "agent_version", "model", "session_id":
			return true
		}
	case logControlModelInfo:
		return field == "context_window"
	case logControlPR:
		switch field {
		case "forge_owner", "forge_pr", "forge_repo":
			return true
		}
	case logControlResult:
		switch field {
		case "agent_result", "cache_creation_input_tokens", "cache_read_input_tokens", "cost_usd", "diff_stat", "duration", "error", "input_tokens", "num_turns", "output_tokens", "reasoning_output_tokens", "state", "title":
			return true
		}
	case logControlPendingUserAction:
		return field == "action"
	case logControlProvisioningLog:
		return field == "line"
	case logControlContextCleared:
		return false
	case logControlLegacyInit:
		return false
	}
	return false
}

func parseV2Control(p *LogRecordParser, kind logControlKind, token string, line []byte) ([]Message, error) {
	if kind != logControlMeta {
		return p.parseControl(kind, token, line)
	}

	var m MetaMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", token, err)
	}
	m.MessageType = token
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("decode %s: %w", token, err)
	}
	if LogVersion(m.Version) != p.version {
		return nil, fmt.Errorf(
			"decode %s: header version %d does not match parser version %d",
			token,
			m.Version,
			p.version,
		)
	}
	return []Message{&m}, nil
}

func validateV2RecordBytes(line []byte) error {
	if len(line) >= v2MaxEncodedRecordLen-1 {
		return fmt.Errorf(
			"corrupt v2 log record: encoded size with LF must be smaller than %d bytes",
			v2MaxEncodedRecordLen,
		)
	}
	if !utf8.Valid(line) {
		return errors.New("corrupt v2 log record: invalid UTF-8")
	}
	if bytes.IndexByte(line, '\n') >= 0 {
		return errors.New("corrupt v2 log record: unexpected LF")
	}
	return nil
}

func parseV2AgentRecord(p *LogRecordParser, line []byte) ([]ParsedMessage, error) {
	rest := line[len(v2AgentRecordPrefix):]
	delimiter := bytes.IndexByte(rest, ',')
	if delimiter < 0 || !bytes.HasPrefix(rest[delimiter:], []byte(v2AgentMessagePrefix)) {
		return nil, errors.New("corrupt v2 agent record: invalid timestamp delimiter")
	}
	producerTime, err := parseV2ProducerTime(rest[:delimiter])
	if err != nil {
		return nil, fmt.Errorf("corrupt v2 agent record: invalid ts: %w", err)
	}

	msgStart := len(v2AgentRecordPrefix) + delimiter + len(v2AgentMessagePrefix)
	if msgStart >= len(line) || line[len(line)-1] != '}' {
		return nil, errors.New("corrupt v2 agent record: missing final msg or closing brace")
	}
	msg := line[msgStart : len(line)-1]
	if len(msg) == 0 {
		return nil, errors.New("corrupt v2 agent record: missing msg")
	}
	if isJSONWhitespace(msg[0]) || isJSONWhitespace(msg[len(msg)-1]) {
		return nil, errors.New("corrupt v2 agent record: msg has surrounding JSON whitespace")
	}
	if bytes.Equal(msg, []byte("null")) {
		return nil, errors.New("corrupt v2 agent record: null msg")
	}
	// msg aliases the scanner-owned record and is valid only for this
	// synchronous callback. The native parser must not retain it.
	msgs, err := p.parseAndApplyNative(msg)
	if err != nil {
		return nil, err
	}
	return wrapParsedMessages(msgs, producerTime), nil
}

func parseV2ProducerTime(raw []byte) (time.Time, error) {
	if len(raw) < len("0.000") || raw[len(raw)-4] != '.' {
		return time.Time{}, errors.New("timestamp must have exactly three fractional digits")
	}
	whole := raw[:len(raw)-4]
	fraction := raw[len(raw)-3:]
	if len(whole) == 0 || (whole[0] == '0' && len(whole) != 1) {
		return time.Time{}, errors.New("timestamp has a noncanonical integer part")
	}
	for _, digit := range whole {
		if digit < '0' || digit > '9' {
			return time.Time{}, errors.New("timestamp integer part must contain only digits")
		}
	}
	for _, digit := range fraction {
		if digit < '0' || digit > '9' {
			return time.Time{}, errors.New("timestamp fraction must contain only digits")
		}
	}

	seconds, err := strconv.ParseInt(string(whole), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp integer is out of range: %w", err)
	}
	if seconds > v2MaxUnixSeconds {
		return time.Time{}, errors.New("timestamp is outside the supported Unix time range")
	}
	milliseconds := int64(0)
	for _, digit := range fraction {
		milliseconds = milliseconds*10 + int64(digit-'0')
	}
	if seconds == 0 && milliseconds == 0 {
		return time.Time{}, errors.New("timestamp must be positive")
	}

	nanoseconds := milliseconds * int64(time.Millisecond)
	producerTime := time.Unix(seconds, nanoseconds).UTC()
	if producerTime.Unix() != seconds || producerTime.Nanosecond() != int(nanoseconds) {
		return time.Time{}, errors.New("timestamp is outside the supported Unix time range")
	}
	return producerTime, nil
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
