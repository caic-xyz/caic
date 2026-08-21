// Task-log loading reconstructs persisted task metadata and messages from disk.

package taskslog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// maxTailLoadBytes is the maximum retained physical-record suffix for
// interactive loading of a large task log.
const maxTailLoadBytes = 64 << 20

// maxParallelLogHeaderLoads bounds the transient memory and file handles
// needed while scanning a large persisted task-log directory.
const maxParallelLogHeaderLoads = 8

// v1TypeEnvelope extracts the legacy v1 native-message type used by inventory parsing.
type v1TypeEnvelope struct {
	Type string `json:"type"`
}

type logAuthority struct {
	Version agent.LogVersion
	Harness harness.Name
}

// NativeParserResolver resolves a fresh native message parser for a validated
// task-log harness. It must not retain or share parser state between scans.
type NativeParserResolver func(harness.Name) (func([]byte) ([]agent.Message, error), error)

// semanticRecord maps one non-empty physical record to its parsed messages.
type semanticRecord struct {
	end     int
	control bool
}

type semanticLog struct {
	authority logAuthority
	messages  []agent.ParsedMessage
	records   []semanticRecord
}

type retainedSemanticRecord struct {
	bytes  int64
	record agent.ParsedRecord
	parsed bool
}

type physicalLogScanner struct {
	scanner   *bufio.Scanner
	src       string
	authority logAuthority
	line      []byte
	typ       string
	err       error
	headerSet bool
	headerRaw []byte
	eof       bool
}

func newPhysicalLogScanner(r io.Reader, src string) *physicalLogScanner {
	return newPhysicalLogScannerWithBuffer(r, src, 1<<20)
}

func newPhysicalLogScannerWithBuffer(r io.Reader, src string, initialBuffer int) *physicalLogScanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialBuffer), 32<<20)
	return &physicalLogScanner{scanner: scanner, src: src}
}

func (s *physicalLogScanner) ReadHeader() (agent.MetaMessage, error) {
	if s.headerSet {
		return agent.MetaMessage{}, errors.New("task log header already read")
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		meta, authority, err := decodeAuthorityMeta(line)
		if err != nil {
			return agent.MetaMessage{}, fmt.Errorf("%s: invalid first log header: %w", s.src, err)
		}
		s.authority = authority
		s.headerRaw = bytes.Clone(line)
		s.headerSet = true
		return meta, nil
	}
	if err := s.scanner.Err(); err != nil {
		return agent.MetaMessage{}, err
	}
	return agent.MetaMessage{}, fmt.Errorf("%s: %w", s.src, errNotLogFile)
}

func (s *physicalLogScanner) Scan() bool {
	if !s.headerSet {
		s.err = errors.New("task log header not read")
		return false
	}
	if s.err != nil {
		return false
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if s.authority.Version == agent.LogVersionV2 && bytes.HasPrefix(line, []byte(`{"t":"agent","ts":`)) {
			s.line = line
			s.typ = "agent"
			return true
		}
		typ, candidate, probeErr := decodeDiscriminatorProbe(line, s.authority.Version)
		if probeErr != nil {
			s.err = fmt.Errorf("%s: invalid log record: %w", s.src, probeErr)
			return false
		}
		if !candidate {
			s.line = line
			s.typ = typ
			return true
		}
		typ, meta, err := decodeSegmentMeta(line, s.authority.Version)
		if errors.Is(err, errNotMetaRecord) {
			s.line = line
			s.typ = typ
			return true
		}
		if err != nil {
			s.err = fmt.Errorf("%s: invalid log segment header: %w", s.src, err)
			return false
		}
		if agent.LogVersion(meta.Version) != s.authority.Version || meta.Harness != s.authority.Harness {
			s.err = fmt.Errorf(
				"%s: log segment authority changed from version %d harness %q to version %d harness %q",
				s.src, s.authority.Version, s.authority.Harness, meta.Version, meta.Harness)
			return false
		}
		s.line = line
		s.typ = typ
		return true
	}
	if s.scanner.Err() == nil {
		s.eof = true
	}
	return false
}

func (s *physicalLogScanner) Bytes() []byte {
	return s.line
}

func (s *physicalLogScanner) Type() string {
	return s.typ
}

func (s *physicalLogScanner) Err() error {
	return errors.Join(s.err, s.scanner.Err())
}

// EOFValidated reports whether Scan reached the physical reader's end without
// a scanner or decompressor error.
func (s *physicalLogScanner) EOFValidated() bool {
	return s.eof && s.Err() == nil
}

// scanPhysicalLog opens one task log, reads its header, and closes it after
// scan returns. Full scans must consume scanner through EOF before returning.
func scanPhysicalLog(path string, validateEOF bool, scan func(os.FileInfo, *physicalLogScanner, agent.MetaMessage) error) (retErr error) {
	r, err := openLogReader(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return errors.Join(err, r.Close())
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()

	scanner := newPhysicalLogScanner(r, path)
	meta, err := scanner.ReadHeader()
	if err != nil {
		return err
	}
	if err := scan(info, scanner, meta); err != nil {
		return err
	}
	if validateEOF && !scanner.EOFValidated() {
		return errors.New("task log did not reach EOF")
	}
	return nil
}

var (
	errNotMetaRecord   = errors.New("not a caic_meta record")
	errDuplicateRawKey = errors.New("duplicate raw key")
)

// decodeDiscriminatorProbe cheaply identifies caic_meta candidates before the
// allocation-heavy raw-object decoder validates their authority fields.
func decodeDiscriminatorProbe(line []byte, version agent.LogVersion) (typ string, candidate bool, err error) {
	i := skipJSONWhitespace(line, 0)
	if i >= len(line) || line[i] != '{' {
		return "", false, nil
	}
	i++
	var typeValue, tValue string
	var typeMeta, tMeta bool
	result := func(err error) (string, bool, error) {
		typ := typeValue
		if version == agent.LogVersionV2 {
			typ = tValue
		}
		return typ, typeMeta || tMeta, err
	}
	for {
		i = skipJSONWhitespace(line, i)
		if i >= len(line) {
			return result(nil)
		}
		if line[i] == '}' {
			break
		}
		keyStart := i
		var ok bool
		i, ok = skipJSONString(line, i)
		if !ok {
			return result(nil)
		}
		key := discriminatorKey(line[keyStart:i])
		i = skipJSONWhitespace(line, i)
		if i >= len(line) || line[i] != ':' {
			return result(nil)
		}
		i = skipJSONWhitespace(line, i+1)
		valueStart := i
		valueEnd, ok, err := skipJSONValue(line, i)
		if err != nil {
			return result(err)
		}
		if !ok {
			return result(nil)
		}
		if key == "type" || key == "t" {
			var value string
			if err := json.Unmarshal(line[valueStart:valueEnd], &value); err == nil {
				if key == "type" {
					typeValue = value
					typeMeta = typeMeta || value == "caic_meta"
				} else {
					tValue = value
					tMeta = tMeta || value == "caic_meta"
				}
				if value != "caic_meta" && !bytes.Contains(line[valueEnd:], []byte(`"caic_meta"`)) {
					return result(nil)
				}
			}
		}
		i = skipJSONWhitespace(line, valueEnd)
		if i >= len(line) {
			return result(nil)
		}
		switch line[i] {
		case ',':
			i++
		case '}':
			i++
			i = skipJSONWhitespace(line, i)
			if i != len(line) {
				return result(nil)
			}
			goto done
		default:
			return result(nil)
		}
	}

done:
	typ = typeValue
	if version == agent.LogVersionV2 {
		typ = tValue
	}
	return typ, typeMeta || tMeta, nil
}

func discriminatorKey(raw []byte) string {
	if bytes.Equal(raw, []byte(`"type"`)) {
		return "type"
	}
	if bytes.Equal(raw, []byte(`"t"`)) {
		return "t"
	}
	var key string
	if json.Unmarshal(raw, &key) == nil {
		return key
	}
	return ""
}

func skipJSONWhitespace(line []byte, i int) int {
	for i < len(line) {
		switch line[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func skipJSONString(line []byte, i int) (int, bool) {
	if i >= len(line) || line[i] != '"' {
		return i, false
	}
	for i++; i < len(line); i++ {
		switch line[i] {
		case '"':
			return i + 1, true
		case '\\':
			i++
			if i >= len(line) {
				return i, false
			}
			if line[i] == 'u' {
				if i+4 >= len(line) || !isJSONHex(line[i+1]) || !isJSONHex(line[i+2]) || !isJSONHex(line[i+3]) || !isJSONHex(line[i+4]) {
					return i, false
				}
				i += 4
			}
		case '\x00', '\x01', '\x02', '\x03', '\x04', '\x05', '\x06', '\x07', '\x08', '\x09', '\x0a', '\x0b', '\x0c', '\x0d', '\x0e', '\x0f', '\x10', '\x11', '\x12', '\x13', '\x14', '\x15', '\x16', '\x17', '\x18', '\x19', '\x1a', '\x1b', '\x1c', '\x1d', '\x1e', '\x1f':
			return i, false
		}
	}
	return i, false
}

func isJSONHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// Keep nesting bounded without imposing a practical limit on valid 32 MiB
// task-log records. The scanner is iterative, so this bounds heap growth rather
// than relying on the goroutine stack.
const maxJSONNesting = 1 << 20

type jsonContainer struct {
	kind  byte
	state uint8
}

const (
	jsonObjectKey uint8 = iota
	jsonObjectAfterComma
	jsonObjectValue
	jsonObjectComma
	jsonArrayValue
	jsonArrayAfterComma
	jsonArrayComma
)

var errJSONNesting = errors.New("JSON nesting exceeds limit")

func skipJSONValue(line []byte, i int) (end int, valid bool, err error) {
	i = skipJSONWhitespace(line, i)
	if i >= len(line) {
		return i, false, nil
	}

	var inlineStack [64]jsonContainer
	stack := inlineStack[:0]
	needValue := true
	for {
		if needValue {
			if len(stack) > 0 {
				frame := &stack[len(stack)-1]
				switch frame.state {
				case jsonObjectValue:
					frame.state = jsonObjectComma
				case jsonArrayValue:
					frame.state = jsonArrayComma
				}
			}
			i = skipJSONWhitespace(line, i)
			if i >= len(line) {
				return i, false, nil
			}
			switch line[i] {
			case '"':
				var ok bool
				i, ok = skipJSONString(line, i)
				if !ok {
					return i, false, nil
				}
			case '{', '[':
				if len(stack) >= maxJSONNesting {
					return i, false, errJSONNesting
				}
				frame := jsonContainer{kind: line[i]}
				if line[i] == '{' {
					frame.state = jsonObjectKey
				} else {
					frame.state = jsonArrayValue
				}
				stack = append(stack, frame)
				i++
				needValue = false
				continue
			case 't':
				var ok bool
				i, ok = skipJSONLiteral(line, i, "true")
				if !ok {
					return i, false, nil
				}
			case 'f':
				var ok bool
				i, ok = skipJSONLiteral(line, i, "false")
				if !ok {
					return i, false, nil
				}
			case 'n':
				var ok bool
				i, ok = skipJSONLiteral(line, i, "null")
				if !ok {
					return i, false, nil
				}
			default:
				if line[i] != '-' && (line[i] < '0' || line[i] > '9') {
					return i, false, nil
				}
				var ok bool
				i, ok = skipJSONNumber(line, i)
				if !ok {
					return i, false, nil
				}
			}
			needValue = false
		}

		if len(stack) == 0 {
			return i, true, nil
		}
		frame := &stack[len(stack)-1]
		i = skipJSONWhitespace(line, i)
		switch frame.state {
		case jsonObjectKey, jsonObjectAfterComma:
			if i >= len(line) || frame.kind != '{' {
				return i, false, nil
			}
			if line[i] == '}' {
				if frame.state == jsonObjectAfterComma {
					return i, false, nil
				}
				i++
				stack = stack[:len(stack)-1]
				continue
			}
			var ok bool
			if i, ok = skipJSONString(line, i); !ok {
				return i, false, nil
			}
			i = skipJSONWhitespace(line, i)
			if i >= len(line) || line[i] != ':' {
				return i, false, nil
			}
			frame.state = jsonObjectValue
			i++
			needValue = true
		case jsonObjectComma:
			if i >= len(line) || frame.kind != '{' {
				return i, false, nil
			}
			switch line[i] {
			case ',':
				frame.state = jsonObjectAfterComma
				i++
			case '}':
				i++
				stack = stack[:len(stack)-1]
			default:
				return i, false, nil
			}
		case jsonArrayValue, jsonArrayAfterComma:
			if i >= len(line) || frame.kind != '[' {
				return i, false, nil
			}
			if line[i] == ']' {
				if frame.state == jsonArrayAfterComma {
					return i, false, nil
				}
				i++
				stack = stack[:len(stack)-1]
				continue
			}
			frame.state = jsonArrayComma
			needValue = true
		case jsonArrayComma:
			if i >= len(line) || frame.kind != '[' {
				return i, false, nil
			}
			switch line[i] {
			case ',':
				frame.state = jsonArrayAfterComma
				i++
			case ']':
				i++
				stack = stack[:len(stack)-1]
			default:
				return i, false, nil
			}
		}
	}
}

func skipJSONLiteral(line []byte, i int, literal string) (int, bool) {
	if len(line)-i < len(literal) {
		return i, false
	}
	for j := range literal {
		if line[i+j] != literal[j] {
			return i, false
		}
	}
	return i + len(literal), true
}

func skipJSONNumber(line []byte, i int) (int, bool) {
	if line[i] == '-' {
		i++
		if i >= len(line) {
			return i, false
		}
	}
	switch {
	case line[i] == '0':
		i++
	case line[i] >= '1' && line[i] <= '9':
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
	default:
		return i, false
	}
	if i < len(line) && line[i] == '.' {
		i++
		start := i
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
		if i == start {
			return i, false
		}
	}
	if i < len(line) && (line[i] == 'e' || line[i] == 'E') {
		i++
		if i < len(line) && (line[i] == '+' || line[i] == '-') {
			i++
		}
		start := i
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
		if i == start {
			return i, false
		}
	}
	return i, true
}

type rawJSONObject struct {
	fields     map[string]json.RawMessage
	duplicates map[string]struct{}
}

func decodeJSONObject(line []byte) (rawJSONObject, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	opening, err := decoder.Token()
	if err != nil {
		return rawJSONObject{}, false, err
	}
	if opening != json.Delim('{') {
		return rawJSONObject{}, false, nil
	}
	obj := rawJSONObject{fields: make(map[string]json.RawMessage)}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return rawJSONObject{}, true, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return rawJSONObject{}, true, errors.New("object field name is not a string")
		}
		if _, exists := obj.fields[key]; exists {
			if isAuthorityField(key) {
				return rawJSONObject{}, true, fmt.Errorf("%w %q", errDuplicateRawKey, key)
			}
			if obj.duplicates == nil {
				obj.duplicates = make(map[string]struct{})
			}
			obj.duplicates[key] = struct{}{}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return rawJSONObject{}, true, err
		}
		obj.fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil {
		return rawJSONObject{}, true, err
	}
	if closing != json.Delim('}') {
		return rawJSONObject{}, true, errors.New("object has an invalid closing delimiter")
	}
	if trailing, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return rawJSONObject{}, true, fmt.Errorf("invalid trailing data: %w", err)
		}
		return rawJSONObject{}, true, fmt.Errorf("unexpected trailing token %v", trailing)
	}
	return obj, true, nil
}

func isAuthorityField(key string) bool {
	switch key {
	case "type", "t", "version", "harness":
		return true
	default:
		return false
	}
}

func stringField(obj rawJSONObject, key string) (string, error) {
	raw, ok := obj.fields[key]
	if !ok {
		return "", errors.New("missing " + key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", key, err)
	}
	return value, nil
}

func decodeAuthorityMeta(line []byte) (agent.MetaMessage, logAuthority, error) {
	if !utf8.Valid(line) {
		return agent.MetaMessage{}, logAuthority{}, errors.New("invalid UTF-8")
	}
	obj, isObject, err := decodeJSONObject(line)
	if err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	if !isObject {
		return agent.MetaMessage{}, logAuthority{}, errors.New("header must be a JSON object")
	}
	rawVersion, ok := obj.fields["version"]
	if !ok {
		return agent.MetaMessage{}, logAuthority{}, errors.New("missing version")
	}
	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return agent.MetaMessage{}, logAuthority{}, fmt.Errorf("version must be an integer: %w", err)
	}
	authority := logAuthority{Version: agent.LogVersion(version)}
	if err := authority.Version.Validate(); err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	if authority.Version == agent.LogVersionV2 && len(obj.duplicates) != 0 {
		duplicates := make([]string, 0, len(obj.duplicates))
		for key := range obj.duplicates {
			duplicates = append(duplicates, key)
		}
		slices.Sort(duplicates)
		return agent.MetaMessage{}, logAuthority{}, fmt.Errorf("%w %q", errDuplicateRawKey, duplicates[0])
	}
	key := "type"
	if authority.Version == agent.LogVersionV2 {
		key = "t"
	}
	otherKey := "t"
	if key == "t" {
		otherKey = "type"
	}
	if _, exists := obj.fields[otherKey]; exists {
		return agent.MetaMessage{}, logAuthority{}, fmt.Errorf("both %q and %q discriminators are present", key, otherKey)
	}
	discriminator, err := stringField(obj, key)
	if err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	if discriminator != "caic_meta" {
		return agent.MetaMessage{}, logAuthority{}, fmt.Errorf("wrong %s discriminator %q", key, discriminator)
	}
	h, err := stringField(obj, "harness")
	if err != nil || h == "" {
		if err == nil {
			err = errors.New("harness is empty")
		}
		return agent.MetaMessage{}, logAuthority{}, err
	}
	authority.Harness = harness.Name(h)
	if authority.Version == agent.LogVersionV2 {
		delete(obj.fields, "t")
		obj.fields["type"] = []byte(`"caic_meta"`)
		line, err = json.Marshal(obj.fields)
		if err != nil {
			return agent.MetaMessage{}, logAuthority{}, err
		}
	}
	var meta agent.MetaMessage
	if authority.Version == agent.LogVersionV2 {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&meta); err != nil {
			return agent.MetaMessage{}, logAuthority{}, err
		}
	} else if err := json.Unmarshal(line, &meta); err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	if err := meta.Validate(); err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	return meta, authority, nil
}

func decodeSegmentMeta(line []byte, version agent.LogVersion) (string, agent.MetaMessage, error) {
	typ, candidate, probeErr := decodeDiscriminatorProbe(line, version)
	if probeErr != nil {
		return typ, agent.MetaMessage{}, probeErr
	}
	if !candidate {
		return typ, agent.MetaMessage{}, errNotMetaRecord
	}
	obj, isObject, err := decodeJSONObject(line)
	if err != nil {
		return typ, agent.MetaMessage{}, err
	}
	if !isObject {
		return typ, agent.MetaMessage{}, errNotMetaRecord
	}
	key := "type"
	if version == agent.LogVersionV2 {
		key = "t"
	}
	otherKey := "t"
	if key == "t" {
		otherKey = "type"
	}
	value, hasKey := obj.fields[key]
	otherValue, hasOther := obj.fields[otherKey]
	if hasKey {
		var discriminator string
		if err := json.Unmarshal(value, &discriminator); err == nil && discriminator == "caic_meta" {
			meta, authority, err := decodeAuthorityMeta(line)
			if err != nil {
				return typ, agent.MetaMessage{}, err
			}
			if authority.Version != version {
				return typ, agent.MetaMessage{}, fmt.Errorf("authority version %d does not match log version %d", authority.Version, version)
			}
			return typ, meta, nil
		}
	}
	if hasOther {
		var discriminator string
		if err := json.Unmarshal(otherValue, &discriminator); err == nil && discriminator == "caic_meta" {
			return typ, agent.MetaMessage{}, fmt.Errorf("wrong %s discriminator for log version %d", otherKey, version)
		}
	}
	return typ, agent.MetaMessage{}, errNotMetaRecord
}

// tailInit is the subset of a system/init message parsed from the tail scan.
type tailInit struct {
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Version string `json:"claude_code_version"`
}

// tailResult is the subset of a result message parsed from the tail scan.
type tailResult struct {
	TotalCostUSD float64     `json:"total_cost_usd"`
	DurationMs   int64       `json:"duration_ms"`
	NumTurns     int         `json:"num_turns"`
	Usage        agent.Usage `json:"usage"`
}

type logTailScan struct {
	lastResultCostUSD  float64
	lastResultDuration time.Duration
	lastResultNumTurns int
	lastResultUsage    agent.Usage
}

func applyMetaResult(lt *LoadedTask, mr *agent.MetaResultMessage) {
	lt.State = parseState(mr.State)
	if mr.Title != "" {
		lt.Title = mr.Title
	}
	lt.Result = &Result{
		State:    lt.State,
		CostUSD:  mr.CostUSD,
		Duration: time.Duration(mr.Duration * float64(time.Second)),
		NumTurns: mr.NumTurns,
		Usage: agent.Usage{
			InputTokens:              mr.InputTokens,
			OutputTokens:             mr.OutputTokens,
			CacheCreationInputTokens: mr.CacheCreationInputTokens,
			CacheReadInputTokens:     mr.CacheReadInputTokens,
			ReasoningOutputTokens:    mr.ReasoningOutputTokens,
		},
		DiffStat:    mr.DiffStat,
		AgentResult: mr.AgentResult,
	}
	if len(mr.DiffStat) > 0 {
		lt.DiffCreated = true
	}
	if mr.Error != "" {
		lt.Result.Err = errors.New(mr.Error)
	}
}

// LoadedTask holds the data reconstructed from a single task log file.
//
// Is serialized as task metadata to disk. Is not used for HTTP wire protocol.
type LoadedTask struct {
	TaskID            string               `json:"task_id"` // Task ID parsed from log filename; empty if unparseable.
	Prompt            string               `json:"prompt"`
	Title             string               `json:"title"`
	Repos             []RepoMount          `json:"repos"` // GitRoot will be empty for purged tasks loaded from logs.
	LogVersion        agent.LogVersion     `json:"log_version"`
	Harness           harness.Name         `json:"harness"`
	StartedAt         time.Time            `json:"started_at"`
	LastStateUpdateAt time.Time            `json:"last_state_update_at"` // Latest relay ts from caic_diff_stat records, falling back to log file mtime.
	State             State                `json:"state"`
	ForgeIssue        int                  `json:"forge_issue"` // Originating issue number for bot comment callbacks.
	ForkedFromTaskID  string               `json:"forked_from_task_id"`
	ForgeOwner        string               `json:"forge_owner"`
	ForgeRepo         string               `json:"forge_repo"`
	ForgePR           int                  `json:"forge_pr"` // PR number created during the task; 0 if none.
	Tailscale         bool                 `json:"tailscale"`
	USB               bool                 `json:"usb"`
	Display           bool                 `json:"display"`
	Sudo              bool                 `json:"sudo"`
	GitHubToken       bool                 `json:"github_token"`
	RuntimeName       runtime.Name         `json:"runtime_name"`
	BaseImage         string               `json:"base_image"`
	ContainerPlatform string               `json:"container_platform"`
	MaxCPUs           int                  `json:"max_cpus"`
	CacheMounts       []runtime.CacheMount `json:"cache_mounts"`
	Mounts            []runtime.Mount      `json:"mounts"`
	Model             string               `json:"model"`
	Effort            string               `json:"effort"`
	SessionID         string               `json:"session_id"` // Backend-native session/thread ID required to resume stateful harnesses.
	AgentVersion      string               `json:"agent_version"`
	LogSize           int64                `json:"log_size"`     // Byte size of the log file on disk; populated by Store.Load.
	DiffCreated       bool                 `json:"diff_created"` // True if any non-empty diff was recorded in the log; sticky across the run.
	Result            *Result              `json:"result"`
	Msgs              []agent.Message      `json:"-"`

	path           string               // Absolute path for lazy message loading via LoadMessages.
	resolver       NativeParserResolver // Fresh parser factory supplied by the task owner.
	messagesLoaded bool                 // A completed semantic scan may validly produce no messages.
}

// Primary returns a pointer to the primary RepoMount (Repos[0]), or nil for no-repo tasks.
func (lt *LoadedTask) Primary() *RepoMount {
	if len(lt.Repos) == 0 {
		return nil
	}
	return &lt.Repos[0]
}

// LogPath returns the absolute task log path used to load the task.
func (lt *LoadedTask) LogPath() string {
	return lt.path
}

// LoadMessagesWithResolver lazily loads full conversation messages with a
// fresh parser derived from the log header.
func (lt *LoadedTask) LoadMessagesWithResolver(resolver NativeParserResolver) error {
	if lt.messagesLoaded || lt.Msgs != nil || lt.path == "" {
		return nil
	}
	loaded, err := loadSemanticTask(lt.path, resolver)
	if err != nil {
		return err
	}
	applySemanticTask(lt, loaded, true)
	return nil
}

// LoadSessionMetadataWithResolver loads session metadata with a fresh parser
// derived from the log header.
func (lt *LoadedTask) LoadSessionMetadataWithResolver(resolver NativeParserResolver) error {
	if lt.path == "" || (lt.SessionID != "" && lt.AgentVersion != "") {
		return nil
	}
	loaded, err := loadSemanticSessionMetadata(lt.path, resolver)
	if err != nil {
		return err
	}
	lt.mergeSessionMetadata(loaded)
	return nil
}

// LoadMessagesTailWithResolver parses the complete source with one ordered
// parser while retaining only a bounded recent suffix for interactive loading.
func (lt *LoadedTask) LoadMessagesTailWithResolver(resolver NativeParserResolver) error {
	if lt.messagesLoaded || lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if !isLogCompressed(lt.path) && lt.LogSize <= maxTailLoadBytes {
		return lt.LoadMessagesWithResolver(resolver)
	}
	loaded, err := loadSemanticTail(lt.path, resolver, maxTailLoadBytes)
	if err != nil {
		return err
	}
	applySemanticTask(lt, loaded, true)
	return nil
}

// SetNativeParserResolver installs the task-owned fresh native parser factory.
// The factory is called only after a scan has validated its physical header.
func (lt *LoadedTask) SetNativeParserResolver(resolver NativeParserResolver) {
	lt.resolver = resolver
}

// LoadMessages lazily loads the full conversation messages from the log file.
// This is an EOF scan of the complete physical log and can be expensive for
// large historical sessions; use LoadMessagesTail for interactive loading.
func (lt *LoadedTask) LoadMessages() error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if lt.resolver == nil {
		return fmt.Errorf("no parser resolver set for harness %q", lt.Harness)
	}
	return lt.LoadMessagesWithResolver(lt.resolver)
}

// LoadSessionMetadata scans the log for backend-neutral session metadata.
func (lt *LoadedTask) LoadSessionMetadata() error {
	if lt.path == "" || (lt.SessionID != "" && lt.AgentVersion != "") {
		return nil
	}
	if lt.resolver == nil {
		return fmt.Errorf("no parser resolver set for harness %q", lt.Harness)
	}
	return lt.LoadSessionMetadataWithResolver(lt.resolver)
}

// LoadMessagesTail loads a bounded suffix of messages from a large log while
// still parsing every record in order to preserve parser state.
func (lt *LoadedTask) LoadMessagesTail() error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if lt.resolver == nil {
		return fmt.Errorf("no parser resolver set for harness %q", lt.Harness)
	}
	return lt.LoadMessagesTailWithResolver(lt.resolver)
}

// StreamMessages streams parsed task conversation records directly from the
// log. Cancellation is checked between records and before delivery.
func (lt *LoadedTask) StreamMessages(ctx context.Context) iter.Seq2[agent.ParsedMessage, error] {
	return func(yield func(agent.ParsedMessage, error) bool) {
		if lt.path == "" {
			yield(agent.ParsedMessage{}, errors.New("task has no log path"))
			return
		}
		if lt.resolver == nil {
			yield(agent.ParsedMessage{}, errors.New("no parser resolver set"))
			return
		}
		err := scanPhysicalLog(lt.path, true, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
			native, err := lt.resolver(scanner.authority.Harness)
			if err != nil {
				return err
			}
			parser, err := agent.NewLogRecordParser(scanner.authority.Version, native)
			if err != nil {
				return err
			}
			if _, err := parser.ParseRecord(scanner.headerRaw); err != nil {
				return err
			}
			for scanner.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				record, err := parser.ParseRecord(scanner.Bytes())
				if err != nil {
					return err
				}
				for _, message := range record.Messages {
					if record.Control && !isHistoryStreamControlMessage(message.Message) {
						continue
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if !yield(message, nil) {
						return errTaskLogStreamStopped
					}
				}
			}
			return scanner.Err()
		})
		if err != nil && !errors.Is(err, errTaskLogStreamStopped) {
			yield(agent.ParsedMessage{}, err)
		}
	}
}

func (lt *LoadedTask) mergeSessionMetadata(src *LoadedTask) {
	if src == nil {
		return
	}
	if lt.SessionID == "" {
		lt.SessionID = src.SessionID
	}
	if lt.Model == "" {
		lt.Model = src.Model
	}
	if lt.AgentVersion == "" {
		lt.AgentVersion = src.AgentVersion
	}
}

// loadSemanticLog parses one complete task log with a fresh parser selected
// by its metadata header.
func loadSemanticLog(path string, resolver NativeParserResolver) (out *semanticLog, retErr error) {
	if resolver == nil {
		return nil, errors.New("native parser resolver is nil")
	}
	retErr = scanPhysicalLog(path, true, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
		native, err := resolver(scanner.authority.Harness)
		if err != nil {
			return fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
		}
		parser, err := agent.NewLogRecordParser(scanner.authority.Version, native)
		if err != nil {
			return fmt.Errorf("construct log parser: %w", err)
		}
		out = &semanticLog{authority: scanner.authority}
		appendRecord := func(record agent.ParsedRecord) {
			if len(record.Messages) == 0 {
				return
			}
			out.messages = append(out.messages, record.Messages...)
			out.records = append(out.records, semanticRecord{end: len(out.messages), control: record.Control})
		}
		record, err := parser.ParseRecord(scanner.headerRaw)
		if err != nil {
			return fmt.Errorf("parse task log bootstrap %s: %w", path, err)
		}
		appendRecord(record)
		for scanner.Scan() {
			record, err := parser.ParseRecord(scanner.Bytes())
			if err != nil {
				if record.Control || scanner.authority.Version == agent.LogVersionV2 {
					return fmt.Errorf("parse task log %s: %w", path, err)
				}
				continue
			}
			appendRecord(record)
		}
		return scanner.Err()
	})
	if retErr != nil {
		return nil, retErr
	}
	return out, nil
}

func loadSemanticTask(path string, resolver NativeParserResolver) (*LoadedTask, error) {
	log, err := loadSemanticLog(path, resolver)
	if err != nil {
		return nil, err
	}
	return semanticLoadedTask(log), nil
}

func loadSemanticSessionMetadata(path string, resolver NativeParserResolver) (loaded *LoadedTask, retErr error) {
	if resolver == nil {
		return nil, errors.New("native parser resolver is nil")
	}
	retErr = scanPhysicalLog(path, true, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
		native, err := resolver(scanner.authority.Harness)
		if err != nil {
			return fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
		}
		parser, err := agent.NewLogRecordParser(scanner.authority.Version, native)
		if err != nil {
			return fmt.Errorf("construct log parser: %w", err)
		}
		loaded = &LoadedTask{LogVersion: scanner.authority.Version}
		apply := func(record agent.ParsedRecord, err error) error {
			if err != nil {
				if record.Control || scanner.authority.Version == agent.LogVersionV2 {
					return fmt.Errorf("parse task log %s: %w", path, err)
				}
				return nil
			}
			applyParsedSessionMetadata(loaded, record.Messages)
			return nil
		}
		record, err := parser.ParseRecord(scanner.headerRaw)
		if err := apply(record, err); err != nil {
			return err
		}
		for scanner.Scan() {
			record, err := parser.ParseRecord(scanner.Bytes())
			if err := apply(record, err); err != nil {
				return err
			}
		}
		return scanner.Err()
	})
	if retErr != nil {
		return nil, retErr
	}
	return loaded, nil
}

// loadSemanticTail parses the complete physical log with one ordered parser
// while retaining only a bounded suffix of semantic records.
func loadSemanticTail(path string, resolver NativeParserResolver, tailBytes int64) (loaded *LoadedTask, retErr error) {
	if resolver == nil {
		return nil, errors.New("native parser resolver is nil")
	}
	retErr = scanPhysicalLog(path, true, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
		native, err := resolver(scanner.authority.Harness)
		if err != nil {
			return fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
		}
		parser, err := agent.NewLogRecordParser(scanner.authority.Version, native)
		if err != nil {
			return fmt.Errorf("construct log parser: %w", err)
		}
		if _, err := parser.ParseRecord(scanner.headerRaw); err != nil {
			return fmt.Errorf("parse task log bootstrap %s: %w", path, err)
		}

		var retained []retainedSemanticRecord
		var retainedBytes int64
		for scanner.Scan() {
			record, err := parser.ParseRecord(scanner.Bytes())
			if err != nil {
				if record.Control || scanner.authority.Version == agent.LogVersionV2 {
					return fmt.Errorf("parse task log %s: %w", path, err)
				}
			}
			retained = append(retained, retainedSemanticRecord{
				bytes:  int64(len(scanner.Bytes()) + 1),
				record: record,
				parsed: err == nil,
			})
			retainedBytes += int64(len(scanner.Bytes()) + 1)
			for retainedBytes > tailBytes && len(retained) > 1 {
				retainedBytes -= retained[0].bytes
				retained = retained[1:]
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}

		log := &semanticLog{authority: scanner.authority}
		for _, item := range retained {
			if !item.parsed || len(item.record.Messages) == 0 {
				continue
			}
			log.messages = append(log.messages, item.record.Messages...)
			log.records = append(log.records, semanticRecord{end: len(log.messages), control: item.record.Control})
		}
		loaded = semanticLoadedTask(log)
		return nil
	})
	if retErr != nil {
		return nil, retErr
	}
	return loaded, nil
}

// ExportDiscussion loads one physical task log with its header-authorized native
// parser and renders the resulting task data as markdown.
func ExportDiscussion(path string, resolver NativeParserResolver) (string, error) {
	log, err := loadSemanticLog(path, resolver)
	if err != nil {
		return "", err
	}
	var meta *agent.MetaMessage
	var result *agent.MetaResultMessage
	var pr *agent.MetaPRMessage
	messages := make([]agent.Message, 0, len(log.messages))
	for _, parsed := range log.messages {
		switch message := parsed.Message.(type) {
		case *agent.MetaMessage:
			meta = message
		case *agent.MetaResultMessage:
			result = message
		case *agent.MetaPRMessage:
			pr = message
		default:
			messages = append(messages, message)
		}
	}
	if meta == nil {
		return "", fmt.Errorf("%s: no caic_meta header", path)
	}
	return agent.RenderDiscussion(meta, result, pr, messages), nil
}

func semanticLoadedTask(log *semanticLog) *LoadedTask {
	loaded := &LoadedTask{LogVersion: log.authority.Version}
	start := 0
	for _, record := range log.records {
		semanticLoadedMessages(loaded, record.control, log.messages[start:record.end])
		start = record.end
	}
	return loaded
}

func semanticLoadedMessages(loaded *LoadedTask, control bool, messages []agent.ParsedMessage) {
	applyInventoryMetadata(loaded, nil, messages)
	for _, parsed := range messages {
		if control && isInventoryMetadataMessage(parsed.Message) {
			continue
		}
		loaded.Msgs = append(loaded.Msgs, parsed.Message)
	}
}

// inventoryResultMetadata is the V1-native result subset needed to backfill an
// omitted caic_result trailer. It is only emitted by the inventory parser.
type inventoryResultMetadata struct {
	tailResult
}

// Type implements agent.Message.
func (*inventoryResultMetadata) Type() string { return "inventory_result" }

// parseInventoryNativeMetadata strictly validates native JSON while preserving
// the V1 inventory scan's generic init/result tail backfill without assigning
// any harness-specific conversation semantics.
func parseInventoryNativeMetadata(raw []byte) ([]agent.Message, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid native JSON value")
	}
	var env v1TypeEnvelope
	if json.Unmarshal(raw, &env) == nil {
		switch env.Type {
		case "system":
			var init tailInit
			if json.Unmarshal(raw, &init) == nil && init.Subtype == "init" {
				return []agent.Message{&agent.InitMessage{Model: init.Model, Version: init.Version}}, nil
			}
		case "result":
			var result tailResult
			if json.Unmarshal(raw, &result) == nil {
				return []agent.Message{&inventoryResultMetadata{tailResult: result}}, nil
			}
		}
	}
	return nil, nil
}

// applyInventoryMetadata updates only task projections available during an
// inventory scan. It deliberately never appends conversation or control
// messages, keeping Msgs nil until a later semantic load.
func applyInventoryMetadata(loaded *LoadedTask, tail *logTailScan, messages []agent.ParsedMessage) {
	for _, parsed := range messages {
		switch msg := parsed.Message.(type) {
		case *agent.InitMessage:
			applySessionMetadataMessages(loaded, []agent.Message{msg})
		case *agent.MetaPRMessage:
			if msg.ForgePR > 0 {
				loaded.ForgeOwner = msg.ForgeOwner
				loaded.ForgeRepo = msg.ForgeRepo
				loaded.ForgePR = msg.ForgePR
			}
		case *agent.MetaResultMessage:
			applyMetaResult(loaded, msg)
		case *agent.DiffStatMessage:
			if len(msg.DiffStat) > 0 {
				loaded.DiffCreated = true
			}
			if msg.Ts > 0 {
				if t := tsToTime(msg.Ts); t.After(loaded.LastStateUpdateAt) {
					loaded.LastStateUpdateAt = t
				}
			}
		case *inventoryResultMetadata:
			if tail != nil {
				tail.lastResultCostUSD = msg.TotalCostUSD
				tail.lastResultDuration = time.Duration(msg.DurationMs) * time.Millisecond
				tail.lastResultNumTurns = msg.NumTurns
				tail.lastResultUsage = msg.Usage
			}
		}
	}
}

func isInventoryMetadataMessage(msg agent.Message) bool {
	switch msg.(type) {
	case *agent.InitMessage, *agent.MetaMessage, *agent.MetaPRMessage, *agent.MetaResultMessage, *agent.DiffStatMessage, *inventoryResultMetadata:
		return true
	default:
		return false
	}
}

func applySemanticTask(lt, loaded *LoadedTask, messages bool) {
	lt.mergeSessionMetadata(loaded)
	if loaded.Title != "" {
		lt.Title = loaded.Title
	}
	if loaded.Result != nil {
		lt.State = loaded.State
		lt.Result = loaded.Result
	}
	if loaded.LastStateUpdateAt.After(lt.LastStateUpdateAt) {
		lt.LastStateUpdateAt = loaded.LastStateUpdateAt
	}
	if loaded.ForgePR > 0 {
		lt.ForgeOwner = loaded.ForgeOwner
		lt.ForgeRepo = loaded.ForgeRepo
		lt.ForgePR = loaded.ForgePR
	}
	if loaded.DiffCreated {
		lt.DiffCreated = true
	}
	if messages {
		lt.Msgs = loaded.Msgs
		lt.messagesLoaded = true
	}
}

func loadedTaskFromMeta(path, taskID string, meta *agent.MetaMessage, modified time.Time, size int64) *LoadedTask {
	repos := make([]RepoMount, len(meta.Repos))
	for i, mr := range meta.Repos {
		repos[i] = RepoMountFromMeta(mr, "")
	}
	return &LoadedTask{
		path:              path,
		TaskID:            taskID,
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repos:             repos,
		LogVersion:        agent.LogVersion(meta.Version),
		Harness:           meta.Harness,
		Model:             meta.Model,
		Effort:            meta.Effort,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: modified,
		State:             StateRunning,
		ForgeIssue:        meta.ForgeIssue,
		ForkedFromTaskID:  meta.ForkedFromTaskID,
		Tailscale:         meta.Tailscale,
		USB:               meta.USB,
		Display:           meta.Display,
		Sudo:              meta.Sudo,
		GitHubToken:       meta.GitHubToken,
		RuntimeName:       runtime.Name(meta.RuntimeName),
		BaseImage:         meta.BaseImage,
		ContainerPlatform: meta.ContainerPlatform,
		MaxCPUs:           meta.MaxCPUs,
		CacheMounts:       runtimeCacheMountsFromMeta(meta.CacheMounts),
		Mounts:            runtimeMountsFromMeta(meta.Mounts),
		LogSize:           size,
	}
}

// capSettledPaths keeps the newest limitPerRepo log paths per repo (by mtime).
// Each repo is read from the cheap first-line header (or its cache), bounded to
// maxParallelLogHeaderLoads concurrent reads so a large cache does not blow up
// transient memory; the caller then fully decodes only the kept paths.
func capSettledPaths(log *slog.Logger, paths []string, limitPerRepo int) []string {
	type cand struct {
		path  string
		repo  string
		mtime time.Time
	}
	cands := make([]cand, len(paths))
	workers := min(len(paths), max(goruntime.GOMAXPROCS(0), 1), maxParallelLogHeaderLoads)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				info, err := os.Stat(paths[i])
				var mtime time.Time
				if info != nil {
					mtime = info.ModTime().UTC()
				} else if err != nil && !os.IsNotExist(err) {
					// A stat failure leaves mtime zero, which sorts the path
					// oldest and can drop it under the cap; surface the cause.
					log.Warn("stat settled task log for capping", "path", paths[i], "err", err)
				}
				cands[i] = cand{path: paths[i], repo: repoFromHeader(log, paths[i]), mtime: mtime}
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// Stable: equal mtimes (batch writes, copied files) must keep the
	// deterministic ReadDir order instead of an arbitrary sort outcome.
	slices.SortStableFunc(cands, func(a, b cand) int { return b.mtime.Compare(a.mtime) })
	perRepo := make(map[string]int)
	kept := make([]string, 0, len(cands))
	for _, c := range cands {
		if perRepo[c.repo] < limitPerRepo {
			perRepo[c.repo]++
			kept = append(kept, c.path)
		}
	}
	return kept
}

// repoFromHeader returns the primary repo name for a task log, from the header
// cache when present and otherwise the cheap first-line header. It is used to
// cap settled logs per repo without a full decode.
func repoFromHeader(log *slog.Logger, path string) string {
	if lt, ok := readHeaderCache(path); ok {
		return primaryRepoName(lt)
	}
	lt, err := loadLogMetaHeader(path)
	if err != nil {
		// Unattributable logs share the "" bucket with no-repo tasks; surface
		// the cause instead of silently consuming that bucket's cap slots.
		log.Warn("read task-log header for repo attribution", "path", path, "err", err)
		return ""
	}
	return primaryRepoName(lt)
}

func primaryRepoName(lt *LoadedTask) string {
	if p := lt.Primary(); p != nil {
		return p.Name
	}
	return ""
}

// logPaths lists task log paths in logDir restricted to one storage form
// (plain or compressed) and an optional mtime age cutoff. When taskIDs is
// non-empty only matching task IDs are returned. For the compressed form, a base
// that also has a plain source is skipped so the plain source stays
// authoritative after an interrupted compression. When cutoff is non-zero,
// logs with an mtime older than cutoff are skipped before any decode.
func logPaths(log *slog.Logger, logDir string, taskIDs map[string]struct{}, compressed bool, cutoff time.Time) ([]string, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Plain sources take precedence over interrupted plain-and-compressed
	// pairs, so compute the plain bases first. logBases tracks every log base
	// (either form) so orphaned header caches can be reaped below.
	plainBases := make(map[string]struct{})
	logBases := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || !IsLogName(e.Name()) {
			continue
		}
		base := trimLogExt(e.Name())
		logBases[base] = struct{}{}
		if !isLogCompressed(e.Name()) {
			plainBases[base] = struct{}{}
		}
	}

	reapHeaderCaches(logDir, entries, logBases)

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !IsLogName(e.Name()) || isLogCompressed(e.Name()) != compressed {
			continue
		}
		base := trimLogExt(e.Name())
		if len(taskIDs) > 0 {
			if _, ok := taskIDs[taskIDFromLogBase(base)]; !ok {
				continue
			}
		}
		if compressed {
			if _, ok := plainBases[base]; ok {
				continue
			}
		}
		if !cutoff.IsZero() {
			info, err := e.Info()
			if err != nil {
				// Fail open: a stat failure keeps the log (an extra decode) but
				// stays visible, so a directory the scan cannot stat into does
				// not silently disable cutoff trimming. A log that vanished
				// between ReadDir and stat (concurrent purge or settle) is
				// routine, so it fails open without a warning.
				if !os.IsNotExist(err) {
					log.Warn("stat task log during scan", "path", filepath.Join(logDir, e.Name()), "err", err)
				}
			} else if info.ModTime().UTC().Before(cutoff) {
				continue
			}
		}
		paths = append(paths, filepath.Join(logDir, e.Name()))
	}
	slices.Sort(paths)
	return paths, nil
}

func loadLogsFromPaths(log *slog.Logger, paths []string, strict, cacheHeader bool) ([]*LoadedTask, error) {
	// Parse headers concurrently, but retain only a bounded number of scanner
	// buffers and decompressor/file handles when a cache has many old logs.
	type result struct {
		lt  *LoadedTask
		err error
	}
	results := make([]result, len(paths))
	workers := min(len(paths), max(goruntime.GOMAXPROCS(0), 1), maxParallelLogHeaderLoads)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				lt, err := loadLogHeader(log, paths[i], cacheHeader)
				results[i] = result{lt, err}
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	var tasks []*LoadedTask
	var errs []error
	for i, r := range results {
		if r.err != nil {
			if strict {
				errs = append(errs, fmt.Errorf("load task log %s: %w", paths[i], r.err))
			}
			continue
		}
		tasks = append(tasks, r.lt)
	}

	slices.SortFunc(tasks, func(a, b *LoadedTask) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return tasks, errors.Join(errs...)
}

func taskIDFromLogBase(base string) string {
	if before, _, ok := strings.Cut(base, "-"); ok {
		return before
	}
	return base
}

var errTaskLogStreamStopped = errors.New("task log stream consumer stopped")

func applyParsedSessionMetadata(lt *LoadedTask, msgs []agent.ParsedMessage) {
	for _, parsed := range msgs {
		init, ok := parsed.Message.(*agent.InitMessage)
		if !ok {
			continue
		}
		if init.SessionID != "" {
			lt.SessionID = init.SessionID
		}
		if init.Model != "" {
			lt.Model = init.Model
		}
		if init.Version != "" {
			lt.AgentVersion = init.Version
		}
		return
	}
}

func applySessionMetadataMessages(lt *LoadedTask, msgs []agent.Message) {
	for _, msg := range msgs {
		init, ok := msg.(*agent.InitMessage)
		if !ok {
			continue
		}
		if init.SessionID != "" {
			lt.SessionID = init.SessionID
		}
		if init.Model != "" {
			lt.Model = init.Model
		}
		if init.Version != "" {
			lt.AgentVersion = init.Version
		}
		return
	}
}

func (s *logTailScan) finish(lt *LoadedTask) {
	if lt.Result != nil && lt.Result.CostUSD == 0 && s.lastResultCostUSD > 0 {
		lt.Result.CostUSD = s.lastResultCostUSD
		lt.Result.Duration = s.lastResultDuration
		lt.Result.NumTurns = s.lastResultNumTurns
		lt.Result.Usage = s.lastResultUsage
	}
}

// loadLogMetaHeader parses only the leading metadata header of a task log
// (plain or compressed) into a LoadedTask. It intentionally does not consume
// the log past the header or validate EOF; it exists so the per-repo cap can
// attribute a log to its repo without decoding the body.
func loadLogMetaHeader(path string) (loaded *LoadedTask, retErr error) {
	retErr = scanPhysicalLog(path, false, func(info os.FileInfo, _ *physicalLogScanner, meta agent.MetaMessage) error {
		base := trimLogExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		return nil
	})
	if retErr != nil {
		return nil, retErr
	}
	return loaded, nil
}

// loadLogHeader reads the metadata header and result trailer from a task log.
// It does NOT parse individual messages — call LoadMessages for that. The path
// is stored for lazy loading. A header cache matching the log size and mtime
// skips the full decode; any mismatch falls back to the scan and refreshes it.
func loadLogHeader(log *slog.Logger, path string, cacheHeader bool) (loaded *LoadedTask, retErr error) {
	if cached, ok := readHeaderCache(path); ok {
		return cached, nil
	}
	retErr = scanPhysicalLog(path, false, func(info os.FileInfo, scanner *physicalLogScanner, meta agent.MetaMessage) error {
		base := trimLogExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		return scanInventoryRecords(path, scanner, loaded)
	})
	if retErr != nil {
		return nil, retErr
	}
	// Cache the header so the next load skips the scan. Only terminal (immutable)
	// logs are worth caching: they are the compressed, CPU-bound set, while live
	// logs change on every append and would just invalidate the entry.
	if cacheHeader && loaded.State.IsTerminal() {
		if err := writeHeaderCache(path, loaded); err != nil {
			// Non-fatal: the load succeeded and the next load re-scans. Log it so a
			// persistent failure (e.g. a read-only cache dir) is visible; it slows
			// startup but never affects correctness.
			log.Warn("write task log header cache", "path", path, "err", err)
		}
	}
	return loaded, nil
}

// isHistoryStreamControlMessage reports controls delivered alongside native
// messages while streaming task history.
func isHistoryStreamControlMessage(msg agent.Message) bool {
	switch msg := msg.(type) {
	case *agent.DiffStatMessage, *agent.ExitMessage, *agent.LogMessage:
		return true
	case *agent.SystemMessage:
		return msg.Subtype == "context_cleared"
	default:
		return false
	}
}

// scanInventoryRecords projects only inventory metadata through a parser local
// to this physical scan. ParsedRecord.Control is the sole control classifier
// for both V1 and V2 logs.
func scanInventoryRecords(path string, scanner *physicalLogScanner, lt *LoadedTask) error {
	tail := logTailScan{}
	parser, err := agent.NewLogRecordParser(scanner.authority.Version, parseInventoryNativeMetadata)
	if err != nil {
		return err
	}
	bootstrap, err := parser.ParseRecord(scanner.headerRaw)
	if err != nil {
		return fmt.Errorf("parse task log bootstrap %s: %w", path, err)
	}
	if bootstrap.Control {
		applyInventoryMetadata(lt, &tail, bootstrap.Messages)
	}
	for scanner.Scan() {
		if scanner.authority.Version == agent.LogVersionV1 && !needsV1InventoryParse(scanner.Type()) {
			continue
		}
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			if record.Control || scanner.authority.Version == agent.LogVersionV2 {
				return fmt.Errorf("parse task log %s: %w", path, err)
			}
			continue
		}
		if record.Control {
			applyInventoryMetadata(lt, &tail, record.Messages)
			continue
		}
		applyInventoryMetadata(lt, &tail, record.Messages)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	tail.finish(lt)
	return nil
}

// needsV1InventoryParse reports whether a legacy record can affect startup
// task metadata. Ordinary native conversation records are intentionally lazy.
func needsV1InventoryParse(typ string) bool {
	return typ == "system" || typ == "result" || typ == agent.PendingUserActionMessageType || strings.HasPrefix(typ, "caic_")
}

// tsToTime converts a Unix epoch float64 (seconds with sub-second precision)
// to a time.Time in UTC.
func tsToTime(ts float64) time.Time {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// parseState converts a persisted state string back to a State value.
// An unrecognized value (corrupt or from a newer version) is treated as
// failed rather than rejected, so one bad historical log cannot block startup.
func parseState(s string) State {
	if s == "terminated" { // backward compat with pre-rename logs; remove once old logs age out
		return StatePurged
	}
	state := State(s)
	if state.Validate() != nil {
		return StateFailed
	}
	return state
}

// LoadHistorySource validates and loads only a log header for history
// streaming. It intentionally does not scan inventory records or validate
// EOF: the caller's subsequent semantic stream performs that one full scan
// and validation.
func LoadHistorySource(path string) (loaded *LoadedTask, retErr error) {
	if path == "" {
		return nil, ErrNoLog
	}
	retErr = scanPhysicalLog(path, false, func(info os.FileInfo, _ *physicalLogScanner, meta agent.MetaMessage) error {
		base := trimLogExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		return nil
	})
	if retErr != nil {
		return nil, retErr
	}
	return loaded, nil
}
