// Validates canonical v3 physical frames and owned control schemas without provider interpretation.

package taskslog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// Kind is the compact top-level "t" discriminator in v3 task-log records.
type Kind string

func (t Kind) controlKind() (logControlKind, bool) {
	switch t {
	case KindMeta:
		return logControlMeta, true
	case KindDiffStat, KindGitSummary:
		return logControlDiffStat, true
	case KindExit:
		return logControlExit, true
	case KindStrippedEnv:
		return logControlStrippedEnv, true
	case KindSession:
		return logControlSession, true
	case KindModelInfo:
		return logControlModelInfo, true
	case KindPR:
		return logControlPR, true
	case KindResult:
		return logControlResult, true
	case KindTurnCommitSnapshot:
		return logControlTurnCommitSnapshot, true
	case KindPendingUserAction:
		return logControlPendingUserAction, true
	case KindProvisioningLog:
		return logControlProvisioningLog, true
	case KindContextCleared:
		return logControlContextCleared, true
	case KindInputDeliveryFailed:
		return logControlInputDeliveryFailed, true
	case KindInputDelivery:
		return logControlInputDelivery, true
	case KindText:
		return logControlText, true
	case KindUserInput:
		return logControlUserInput, true
	case KindMCPRequest:
		return logControlMCPRequest, true
	case KindRelayGeneration:
		return logControlRelayGeneration, true
	default:
		return 0, false
	}
}

type controlFields struct {
	kind    Kind
	names   []string
	version json.RawMessage
}

func decodeV3ControlFields(line []byte) (controlFields, error) {
	var fields controlFields
	decoder := json.NewDecoder(bytes.NewReader(line))
	opening, err := decoder.Token()
	if err != nil {
		return controlFields{}, err
	}
	if opening != json.Delim('{') {
		return controlFields{}, errors.New("record must be a JSON object")
	}

	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return controlFields{}, err
		}
		field, ok := keyToken.(string)
		if !ok {
			return controlFields{}, errors.New("record field name is not a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return controlFields{}, fmt.Errorf("duplicate top-level field %q", field)
		}
		seen[field] = struct{}{}
		fields.names = append(fields.names, field)

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return controlFields{}, err
		}
		if field == "version" {
			fields.version = value
		}
		if field == "t" {
			if err := json.Unmarshal(value, &fields.kind); err != nil {
				return controlFields{}, fmt.Errorf("top-level t must be a string: %w", err)
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return controlFields{}, err
	}
	if closing != json.Delim('}') {
		return controlFields{}, errors.New("record has an invalid closing delimiter")
	}
	if trailing, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return controlFields{}, fmt.Errorf("invalid trailing data: %w", err)
		}
		return controlFields{}, fmt.Errorf("unexpected trailing token %v", trailing)
	}
	return fields, nil
}

// Physical v3 record kinds.
const (
	KindAgent    Kind = "agent"
	KindInput    Kind = "input"
	KindMeta     Kind = "caic_meta"
	KindDiffStat Kind = "diff_stat"
	KindExit     Kind = "exit"
	// KindGitSummary identifies host-generated repository summaries.
	KindGitSummary          Kind = "git_summary"
	KindStrippedEnv         Kind = "stripped_env"
	KindSession             Kind = "session"
	KindModelInfo           Kind = "model_info"
	KindPR                  Kind = "pr"
	KindResult              Kind = "result"
	KindTurnCommitSnapshot  Kind = "turn_commit_snapshot"
	KindPendingUserAction   Kind = "pending_user_action"
	KindProvisioningLog     Kind = "log"
	KindContextCleared      Kind = "context_cleared"
	KindInputDeliveryFailed Kind = "input_delivery_failed"
	KindInputDelivery       Kind = "input_delivery"
	KindText                Kind = "text"
	KindUserInput           Kind = "user_input"
	KindMCPRequest          Kind = "mcp_request"
	KindRelayGeneration     Kind = "relay_generation"
)

const (
	v3AgentRecordPrefix  = `{"t":"` + string(KindAgent) + `","ts":`
	v3InputRecordPrefix  = `{"t":"` + string(KindInput) + `","ts":`
	v3AgentMessagePrefix = `,"msg":`
	v3MaxUnixSeconds     = int64(^uint64(0)>>1) - 62_135_596_800
)

// MaxRecordSize is the exclusive byte limit for a physical task-log record,
// including its LF. Live transport, validation, and history readers share it.
// The Python relay's _MAX_ENCODED_RECORD_LEN must match this limit.
const MaxRecordSize = 128 << 20

// Frame is a validated v3 physical record. Encoded and Native borrow the input
// until its owner reuses the buffer; Control holds an owned v3 schema value.
type Frame struct {
	Kind         Kind
	Encoded      []byte
	Native       []byte
	ProducerTime time.Time
	Control      any
}

// DecodeFrame validates one physical record without an LF or provider decoding.
func DecodeFrame(line []byte) (Frame, error) {
	if err := validateV3RecordBytes(line); err != nil {
		return Frame{}, err
	}
	if bytes.HasPrefix(line, []byte(v3AgentRecordPrefix)) {
		return parseV3NativeRecord(line, v3AgentRecordPrefix, KindAgent)
	}
	if bytes.HasPrefix(line, []byte(v3InputRecordPrefix)) {
		return parseV3NativeRecord(line, v3InputRecordPrefix, KindInput)
	}
	fields, err := decodeV3ControlFields(line)
	if err != nil {
		return Frame{}, fmt.Errorf("corrupt v3 log record: decode control discriminator: %w", err)
	}
	token := fields.kind
	if token == "" {
		if containsV3ControlField(fields.names, "type") {
			return Frame{}, errors.New("corrupt v3 log record: unknown top-level field \"type\": top-level type discriminator is not valid in v3")
		}
		return Frame{}, errors.New("corrupt v3 log record: missing top-level t")
	}
	if token == KindAgent {
		return Frame{}, errors.New("corrupt v3 agent record: noncanonical envelope")
	}
	kind, ok := token.controlKind()
	if !ok {
		return Frame{}, fmt.Errorf("corrupt v3 log record: unknown top-level t %q", token)
	}
	frame := Frame{Kind: token, Encoded: line}
	if err := validateV3ControlFields(kind, token, fields.names); err != nil {
		return frame, err
	}
	if kind == logControlMeta {
		// Exact-name version is physical authority. A case-insensitive schema
		// alias must not replace it, even if decoding the owned DTO accepts one.
		if fields.version == nil {
			return frame, errors.New("missing version")
		}
		var version int
		if err := json.Unmarshal(fields.version, &version); err != nil {
			return frame, fmt.Errorf("version must be an integer: %w", err)
		}
		if version != 3 {
			return frame, fmt.Errorf("unsupported log version %d", version)
		}
	}
	control, err := decodeControl(kind, line)
	if err != nil {
		return frame, fmt.Errorf("decode %s: %w", token, err)
	}
	frame.Control = control
	if input, ok := control.(*v3.UserInputMessage); ok && input.Ts > 0 {
		frame.ProducerTime = time.UnixMilli(int64(math.Round(input.Ts * 1000)))
	}
	if system, ok := control.(*v3.SystemMessage); ok && system.Ts > 0 {
		frame.ProducerTime = time.UnixMilli(int64(math.Round(system.Ts * 1000)))
	}
	if diff, ok := control.(*v3.DiffStatMessage); ok && diff.Ts > 0 {
		frame.ProducerTime = time.UnixMilli(int64(math.Round(diff.Ts * 1000)))
	}
	if snapshot, ok := control.(*v3.TurnCommitSnapshotMessage); ok && snapshot.Ts > 0 {
		frame.ProducerTime = time.UnixMilli(int64(math.Round(snapshot.Ts * 1000)))
	}
	return frame, nil
}

func containsV3ControlField(fields []string, want string) bool {
	return slices.Contains(fields, want)
}

func validateV3ControlFields(kind logControlKind, token Kind, fields []string) error {
	for _, field := range fields {
		if field == "type" {
			return errors.New("corrupt v3 log record: unknown top-level field \"type\": top-level type discriminator is not valid in v3")
		}
		// The strict owned-schema decoder validates caic_meta fields after
		// this pass has enforced the v3 discriminator and duplicate-key rules.
		if kind == logControlMeta || v3ControlFieldAllowed(kind, field) {
			continue
		}
		return fmt.Errorf("corrupt v3 %s record: unknown top-level field %q", token, field)
	}
	return nil
}

// v3ControlFields lists the top-level fields the strict decoder accepts for
// each v3 control kind, mirroring the owned schemas. The envelope fields are
// implicit. caic_meta is absent because its strict decoder owns its fields.
var v3ControlFields = map[logControlKind][]string{
	logControlDiffStat:            {"diff_stat", "repos", "ts"},
	logControlExit:                {"cmd", "error", "exit_code", "signal", "stderr_truncated", "ts"},
	logControlStrippedEnv:         {"variables"},
	logControlSession:             {"agent_version", "auto_compact_token_limit", "model", "reported_effort", "session_id"},
	logControlModelInfo:           {"context_window"},
	logControlPR:                  {"forge_owner", "forge_pr", "forge_repo"},
	logControlResult:              {"agent_result", "cache_creation_input_tokens", "cache_read_input_tokens", "cache_writes", "cost_complete", "cost_known", "cost_usd", "diff_stat", "disk_used_bytes", "duration", "error", "input_tokens", "num_turns", "output_tokens", "reasoning_output_tokens", "startup_failure", "state", "title"},
	logControlTurnCommitSnapshot:  {"baseline", "change_stat", "repository_commits", "ts"},
	logControlPendingUserAction:   {"action"},
	logControlProvisioningLog:     {"line"},
	logControlContextCleared:      {"context_tokens_after", "context_tokens_before", "detail", "model", "session_id", "subtype", "ts", "uuid"},
	logControlInputDeliveryFailed: {"context_tokens_after", "context_tokens_before", "detail", "model", "session_id", "subtype", "ts", "uuid"},
	logControlInputDelivery:       {"context_tokens_after", "context_tokens_before", "detail", "model", "session_id", "subtype", "ts", "uuid"},
	logControlText:                {"phase", "text"},
	logControlUserInput:           {"images", "text", "ts"},
	logControlMCPRequest:          {"arguments", "id", "method", "name"},
	logControlRelayGeneration:     {"generation"},
}

func v3ControlFieldAllowed(kind logControlKind, field string) bool {
	if field == "t" || field == "source" {
		return true
	}
	return slices.Contains(v3ControlFields[kind], field)
}

func validateV3RecordBytes(line []byte) error {
	if len(line) >= MaxRecordSize-1 {
		return fmt.Errorf(
			"corrupt v3 log record: encoded size with LF must be smaller than %d bytes",
			MaxRecordSize,
		)
	}
	if !utf8.Valid(line) {
		return errors.New("corrupt v3 log record: invalid UTF-8")
	}
	if bytes.IndexByte(line, '\n') >= 0 {
		return errors.New("corrupt v3 log record: unexpected LF")
	}
	return nil
}

func parseV3NativeRecord(line []byte, prefix string, kind Kind) (Frame, error) {
	rest := line[len(prefix):]
	delimiter := bytes.IndexByte(rest, ',')
	if delimiter < 0 || !bytes.HasPrefix(rest[delimiter:], []byte(v3AgentMessagePrefix)) {
		return Frame{}, errors.New("corrupt v3 agent record: invalid timestamp delimiter")
	}
	producerTime, err := parseV3ProducerTime(rest[:delimiter])
	if err != nil {
		return Frame{}, fmt.Errorf("corrupt v3 agent record: invalid ts: %w", err)
	}

	msgStart := len(prefix) + delimiter + len(v3AgentMessagePrefix)
	if msgStart >= len(line) || line[len(line)-1] != '}' {
		return Frame{}, errors.New("corrupt v3 agent record: missing final msg or closing brace")
	}
	msg := line[msgStart : len(line)-1]
	if len(msg) == 0 {
		return Frame{}, errors.New("corrupt v3 agent record: missing msg")
	}
	if isJSONWhitespace(msg[0]) || isJSONWhitespace(msg[len(msg)-1]) {
		return Frame{}, errors.New("corrupt v3 agent record: msg has surrounding JSON whitespace")
	}
	if bytes.Equal(msg, []byte("null")) {
		return Frame{}, errors.New("corrupt v3 agent record: null msg")
	}
	if !json.Valid(msg) {
		return Frame{}, errors.New("corrupt v3 agent record: invalid or noncanonical msg envelope")
	}
	return Frame{Kind: kind, Encoded: line, Native: msg, ProducerTime: producerTime}, nil
}

func parseV3ProducerTime(raw []byte) (time.Time, error) {
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
	if seconds > v3MaxUnixSeconds {
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

type logControlKind int

const (
	logControlMeta logControlKind = iota
	logControlDiffStat
	logControlExit
	logControlStrippedEnv
	logControlSession
	logControlModelInfo
	logControlPR
	logControlResult
	logControlTurnCommitSnapshot
	logControlPendingUserAction
	logControlProvisioningLog
	logControlContextCleared
	logControlInputDeliveryFailed
	logControlInputDelivery
	logControlText
	logControlUserInput
	logControlMCPRequest
	logControlRelayGeneration
)

// newControl returns an empty v3 control of the kind's owned schema.
func newControl(kind logControlKind) any {
	var out any
	switch kind {
	case logControlMeta:
		out = new(v3.MetaMessage)
	case logControlDiffStat:
		out = new(v3.DiffStatMessage)
	case logControlExit:
		out = new(v3.ExitMessage)
	case logControlStrippedEnv:
		out = new(v3.StrippedEnvMessage)
	case logControlSession:
		out = new(v3.MetaSessionMessage)
	case logControlModelInfo:
		out = new(v3.ModelInfoMessage)
	case logControlPR:
		out = new(v3.MetaPRMessage)
	case logControlResult:
		out = new(v3.MetaResultMessage)
	case logControlTurnCommitSnapshot:
		out = new(v3.TurnCommitSnapshotMessage)
	case logControlPendingUserAction:
		out = new(v3.PendingUserActionMessage)
	case logControlProvisioningLog:
		out = new(v3.LogMessage)
	case logControlContextCleared:
		out = new(v3.SystemMessage)
	case logControlInputDeliveryFailed:
		out = new(v3.SystemMessage)
	case logControlInputDelivery:
		out = new(v3.SystemMessage)
	case logControlText:
		out = new(v3.TextMessage)
	case logControlUserInput:
		out = new(v3.UserInputMessage)
	case logControlMCPRequest:
		out = new(v3.MCPRequestMessage)
	case logControlRelayGeneration:
		out = new(v3.RelayGenerationMessage)
	}
	return out
}

func decodeControl(kind logControlKind, line []byte) (any, error) {
	out := newControl(kind)
	var err error
	if kind == logControlMeta || kind == logControlText {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(out)
	} else {
		err = json.Unmarshal(line, out)
	}
	if err != nil {
		return nil, err
	}
	switch m := out.(type) {
	case *v3.MetaMessage:
		if m.MessageType != string(KindMeta) {
			return nil, fmt.Errorf("unexpected type %q", m.MessageType)
		}
		if m.Version != 3 {
			return nil, fmt.Errorf("unsupported log version %d", m.Version)
		}
		if m.Prompt == "" {
			return nil, errors.New("missing prompt")
		}
		if m.Harness == "" {
			return nil, errors.New("missing harness")
		}
	case *v3.MetaResultMessage:
		if m.CacheCreationInputTokens < 0 {
			return nil, errors.New("negative cache creation tokens")
		}
		if len(m.CacheWrites) > 0 {
			var total int64
			for _, w := range m.CacheWrites {
				if w.Tokens < 0 || total > math.MaxInt64-w.Tokens {
					return nil, errors.New("invalid cache write tokens")
				}
				total += w.Tokens
			}
			if total != int64(m.CacheCreationInputTokens) {
				return nil, fmt.Errorf("cache writes total %d, want %d", total, m.CacheCreationInputTokens)
			}
		}
	case *v3.ModelInfoMessage:
		if m.ContextWindow > 0 && int64(int(m.ContextWindow)) != m.ContextWindow {
			return nil, errors.New("context_window out of range")
		}
	case *v3.TurnCommitSnapshotMessage:
		if m.Ts < 0 || m.Ts > float64(math.MaxInt64)/1000 {
			return nil, errors.New("invalid commit snapshot producer timestamp")
		}
	case *v3.UserInputMessage:
		if m.Ts < 0 || m.Ts > float64(math.MaxInt64)/1000 {
			return nil, errors.New("invalid user input observation timestamp")
		}
	case *v3.RelayGenerationMessage:
		if m.Generation == "" {
			return nil, errors.New("generation is empty")
		}
	case *v3.MCPRequestMessage:
		if m.ID == "" || (m.Method != "tools/list" && (m.Method != "tools/call" || m.Name == "")) {
			return nil, errors.New("invalid MCP request")
		}
	case *v3.SystemMessage:
		switch kind { //nolint:exhaustive // Only these kinds decode to a SystemMessage.
		case logControlContextCleared:
			if m.Subtype != "" && m.Subtype != "context_cleared" {
				return nil, fmt.Errorf("unexpected subtype %q", m.Subtype)
			}
		case logControlInputDeliveryFailed:
			if m.Subtype != "input_delivery_failed" {
				return nil, fmt.Errorf("unexpected subtype %q", m.Subtype)
			}
		case logControlInputDelivery:
			if m.Subtype != "input_delivery_started" && m.Subtype != "input_delivery_completed" {
				return nil, fmt.Errorf("unexpected subtype %q", m.Subtype)
			}
		}
	}
	return out, nil
}
