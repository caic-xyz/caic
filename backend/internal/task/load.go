// Loads task definitions from disk and resolves their configurations.

package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// v1TypeEnvelope extracts the legacy v1 control type used by tail metadata scans.
type v1TypeEnvelope struct {
	Type string `json:"type"`
}

// metaKnown is the set of JSON field names recognised by agent.MetaMessage.
var metaKnown = jsonutil.KnownFields(agent.MetaMessage{})

// resultKnown is the set of JSON field names recognised by agent.MetaResultMessage.
var resultKnown = jsonutil.KnownFields(agent.MetaResultMessage{})

type logAuthority struct {
	Version agent.LogVersion
	Harness harness.Name
}

// NativeParserResolver resolves a fresh native message parser for a validated
// task-log harness. It must not retain or share parser state between scans.
type NativeParserResolver func(harness.Name) (func([]byte) ([]agent.Message, error), error)

// ValidatedLogSnapshot proves the physical file and authority observed by a
// completed task-log scan. Its fields, including Messages, are immutable by
// convention after publication. It is in-memory only and is never persisted
// as an authority source.
type ValidatedLogSnapshot struct {
	Path         string
	Device       uint64
	Inode        uint64
	Size         int64
	ModTimeNs    int64
	Authority    logAuthority
	EOFValidated bool
	// RawHeader is an immutable string containing the exact first header bytes.
	RawHeader string
	// Messages contains the semantic records parsed during this scan. The slice
	// is immutable by convention and is populated only by a semantic scan.
	Messages []agent.ParsedMessage
	records  []semanticRecord
}

// semanticRecord maps one non-empty physical record to its messages in the
// snapshot without retaining a second slice of those messages.
type semanticRecord struct {
	end     int
	control bool
}

type tailLogRecord struct {
	line []byte
}

type physicalLogReader struct {
	file   *os.File
	reader io.ReadCloser
	info   os.FileInfo
}

type physicalFileIdentity struct {
	Device uint64
	Inode  uint64
	Valid  bool
}

func newValidatedLogSnapshot(path string, file *os.File, info os.FileInfo, authority logAuthority, rawHeader []byte, eofValidated bool) (*ValidatedLogSnapshot, error) {
	if !eofValidated {
		return nil, fmt.Errorf("task log has not been validated through EOF: %s", path)
	}
	identity := physicalFileIdentityFromFile(file, info)
	if !identity.Valid {
		return nil, fmt.Errorf("task log has no stable physical identity: %s", path)
	}
	return &ValidatedLogSnapshot{
		Path:         filepath.Clean(path),
		Device:       identity.Device,
		Inode:        identity.Inode,
		Size:         info.Size(),
		ModTimeNs:    info.ModTime().UnixNano(),
		Authority:    authority,
		EOFValidated: true,
		RawHeader:    string(rawHeader),
	}, nil
}

// CacheProof is the derived identity and raw header authority used to bind a
// rebuildable replay sidecar to one physical task log observation.
type CacheProof struct {
	Device    uint64
	Inode     uint64
	Size      int64
	ModTimeNs int64
	Version   agent.LogVersion
	Harness   harness.Name
	RawHeader string
}

// CacheProofForLog validates the raw first header on the same physical file
// observation whose identity is returned. It is deliberately task-owned so
// caches cannot select a parser or invent authority from their own metadata.
func CacheProofForLog(path string) (_ CacheProof, retErr error) {
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return CacheProof{}, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	scanner := newPhysicalLogScanner(r.reader, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return CacheProof{}, err
	}
	info, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return CacheProof{}, err
	}
	identity := physicalFileIdentityFromFile(r.file, info)
	if !identity.Valid {
		return CacheProof{}, fmt.Errorf("task log has no stable physical identity: %s", path)
	}
	return CacheProof{Device: identity.Device, Inode: identity.Inode, Size: info.Size(), ModTimeNs: info.ModTime().UnixNano(), Version: scanner.authority.Version, Harness: scanner.authority.Harness, RawHeader: string(scanner.headerRaw)}, nil
}

func validatedSnapshotMatchesFile(snapshot *ValidatedLogSnapshot, path string) bool {
	if snapshot == nil || !snapshot.EOFValidated || snapshot.Path != filepath.Clean(path) {
		return false
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	identity := physicalFileIdentityFromFile(file, info)
	if !identity.Valid || snapshot.Device != identity.Device || snapshot.Inode != identity.Inode ||
		snapshot.Size != info.Size() || snapshot.ModTimeNs != info.ModTime().UnixNano() {
		return false
	}
	_, err = verifyPhysicalLog(path, file, info)
	return err == nil
}

// validationProof returns a copy safe to retain on a LoadedTask.
//
// Semantic messages and their record index are temporary scan state used to
// reconstruct task fields. Retaining them duplicates the task history already
// stored in Msgs and can pin a large log's parsed messages for the task's
// lifetime.
func (snapshot *ValidatedLogSnapshot) validationProof() *ValidatedLogSnapshot {
	proof := *snapshot
	proof.Messages = nil
	proof.records = nil
	return &proof
}

func rebindSnapshotToFile(path string, source *ValidatedLogSnapshot) (_ *ValidatedLogSnapshot, retErr error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	identity := physicalFileIdentityFromFile(file, info)
	if !identity.Valid {
		return nil, fmt.Errorf("task log has no stable physical identity: %s", path)
	}
	return &ValidatedLogSnapshot{
		Path:      filepath.Clean(path),
		Device:    identity.Device,
		Inode:     identity.Inode,
		Size:      info.Size(),
		ModTimeNs: info.ModTime().UnixNano(),
		Authority: source.Authority,
		RawHeader: source.RawHeader,
		Messages:  source.Messages,
		records:   source.records,
	}, nil
}

func openPhysicalLogReader(path string) (*physicalLogReader, error) {
	r, err := openLogReader(path)
	if err != nil {
		return nil, err
	}
	var f *os.File
	switch v := r.(type) {
	case *os.File:
		f = v
	case *zstdReadCloser:
		f = v.file
	default:
		return nil, errors.Join(fmt.Errorf("unsupported physical log reader %T", r), r.Close())
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return &physicalLogReader{file: f, reader: r, info: info}, nil
}

func (r *physicalLogReader) Close() error {
	return r.reader.Close()
}

func verifyPhysicalLog(path string, f *os.File, expected os.FileInfo) (os.FileInfo, error) {
	current, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, current) || current.Size() != expected.Size() || current.ModTime().UnixNano() != expected.ModTime().UnixNano() {
		return nil, fmt.Errorf("task log changed while reading: %s", path)
	}
	pathInfo, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	if !os.SameFile(current, pathInfo) || pathInfo.Size() != current.Size() || pathInfo.ModTime().UnixNano() != current.ModTime().UnixNano() {
		return nil, fmt.Errorf("task log replaced while reading: %s", path)
	}
	return current, nil
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
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
	return &physicalLogScanner{scanner: scanner, src: src}
}

func (s *physicalLogScanner) ReadHeader(fw *jsonutil.FieldWarner) (agent.MetaMessage, error) {
	if s.headerSet {
		return agent.MetaMessage{}, errors.New("task log header already read")
	}
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		meta, authority, err := decodeAuthorityMeta(line, fw)
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

var errNotMetaRecord = errors.New("not a caic_meta record")
var errDuplicateRawKey = errors.New("duplicate raw key")

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
	fields map[string]json.RawMessage
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
		if _, exists := obj.fields[key]; exists && isAuthorityField(key) {
			return rawJSONObject{}, true, fmt.Errorf("%w %q", errDuplicateRawKey, key)
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

func decodeAuthorityMeta(line []byte, fw *jsonutil.FieldWarner) (agent.MetaMessage, logAuthority, error) {
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
	var meta agent.MetaMessage
	if err := json.Unmarshal(line, &meta); err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	meta.MessageType = "caic_meta"
	if err := meta.Validate(); err != nil {
		return agent.MetaMessage{}, logAuthority{}, err
	}
	if authority.Version == agent.LogVersionV2 {
		delete(obj.fields, "t")
	}
	unknown := jsonutil.CollectUnknown(obj.fields, metaKnown)
	if authority.Version == agent.LogVersionV2 && len(unknown) > 0 {
		return agent.MetaMessage{}, logAuthority{}, fmt.Errorf("unknown v2 caic_meta fields: %v", slices.Sorted(maps.Keys(unknown)))
	}
	if fw != nil {
		fw.Warn("caic_meta", unknown)
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
			meta, authority, err := decodeAuthorityMeta(line, nil)
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

func readLogAuthority(path string) (_ logAuthority, retErr error) {
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return logAuthority{}, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	return scanLogAuthority(r.reader, path)
}

func scanLogAuthority(r io.Reader, src string) (logAuthority, error) {
	scanner := newPhysicalLogScanner(r, src)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return logAuthority{}, err
	}
	for scanner.Scan() {
	}
	return scanner.authority, scanner.Err()
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
	fw *jsonutil.FieldWarner

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

// noteDiffStatLine updates lt from a caic_diff_stat record: it marks DiffCreated
// when the diff is non-empty (sticky, so a later empty diff never clears it) and
// advances LastStateUpdateAt from the relay timestamp.
func noteDiffStatLine(lt *LoadedTask, line []byte) {
	var ds agent.DiffStatMessage
	if json.Unmarshal(line, &ds) != nil {
		return
	}
	if len(ds.DiffStat) > 0 {
		lt.DiffCreated = true
	}
	if ds.Ts > 0 {
		if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
			lt.LastStateUpdateAt = t
		}
	}
}

// TODO: Trim legacyCaicInit after 2026-08 once legacy caic_init logs are old enough to ignore.
// legacyCaicInit is the OpenCode pre-caic_session session metadata record.
type legacyCaicInit struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Version   string `json:"version"`
}

// LoadedTask holds the data reconstructed from a single task log file.
type LoadedTask struct {
	TaskID            string // Task ID parsed from log filename; empty if unparseable.
	Prompt            string
	Title             string
	Repos             []RepoMount // GitRoot will be empty for purged tasks loaded from logs.
	LogVersion        agent.LogVersion
	Harness           harness.Name
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Latest relay ts from caic_diff_stat records, falling back to log file mtime.
	State             State
	ForgeIssue        int // Originating issue number for bot comment callbacks.
	ForkedFromTaskID  string
	ForgeOwner        string
	ForgeRepo         string
	ForgePR           int // PR number created during the task; 0 if none.
	Tailscale         bool
	USB               bool
	Display           bool
	Sudo              bool
	GitHubToken       bool
	RuntimeName       runtime.Name
	BaseImage         string
	ContainerPlatform string
	MaxCPUs           int
	CacheMounts       []runtime.CacheMount
	Mounts            []runtime.Mount
	Model             string
	Effort            string
	SessionID         string // Backend-native session/thread ID required to resume stateful harnesses.
	AgentVersion      string
	LogSize           int64 // Byte size of the log file on disk; populated by LoadLogs.
	DiffCreated       bool  // True if any non-empty diff was recorded in the log; sticky across the run.
	Msgs              []agent.Message
	Result            *Result

	path     string               // Absolute path for lazy message loading via LoadMessages.
	resolver NativeParserResolver // Fresh parser factory supplied by the task owner.

	snapshotMu sync.Mutex
	snapshot   *ValidatedLogSnapshot // Last completed physical validation.
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

// ValidatedSnapshot returns the last completed physical validation proof.
// Callers must treat the proof and its Messages slice as immutable by
// convention.
func (lt *LoadedTask) ValidatedSnapshot() *ValidatedLogSnapshot {
	lt.snapshotMu.Lock()
	defer lt.snapshotMu.Unlock()
	return lt.snapshot
}

// loadSemanticLogSnapshot validates and semantically parses one task log in a
// single physical pass. It resolves its native parser only after validating the
// first header from that same pass.
func loadSemanticLogSnapshot(path string, resolver NativeParserResolver) (_ *ValidatedLogSnapshot, retErr error) {
	if resolver == nil {
		return nil, errors.New("native parser resolver is nil")
	}
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, err
	}
	return loadSemanticLogSnapshotFromReader(path, r, resolver)
}

func loadSemanticLogSnapshotFromReader(path string, r *physicalLogReader, resolver NativeParserResolver) (_ *ValidatedLogSnapshot, retErr error) {
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()

	scanner := newPhysicalLogScanner(r.reader, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return nil, err
	}
	nativeParser, err := resolver(scanner.authority.Harness)
	if err != nil {
		return nil, fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
	}
	parser, err := agent.NewLogRecordParser(scanner.authority.Version, nativeParser)
	if err != nil {
		return nil, fmt.Errorf("construct log parser: %w", err)
	}

	messages := make([]agent.ParsedMessage, 0)
	records := make([]semanticRecord, 0)
	appendRecord := func(parsed []agent.ParsedMessage, control bool) {
		if len(parsed) == 0 {
			return
		}
		messages = append(messages, parsed...)
		records = append(records, semanticRecord{end: len(messages), control: control})
	}
	bootstrap, err := parser.ParseRecord(scanner.headerRaw)
	if err != nil {
		return nil, fmt.Errorf("parse task log bootstrap %s: %w", path, err)
	}
	appendRecord(bootstrap.Messages, bootstrap.Control)
	for scanner.Scan() {
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			if record.Control || scanner.authority.Version == agent.LogVersionV2 {
				return nil, fmt.Errorf("parse task log %s: %w", path, err)
			}
			slog.Warn("failed to parse message", "err", err, "path", path)
			continue
		}
		appendRecord(record.Messages, record.Control)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	stableInfo, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return nil, err
	}
	snapshot, err := newValidatedLogSnapshot(path, r.file, stableInfo, scanner.authority, scanner.headerRaw, scanner.EOFValidated())
	if err != nil {
		return nil, err
	}
	snapshot.Messages = messages
	snapshot.records = records
	return snapshot, nil
}

func loadSemanticSnapshot(lt *LoadedTask, resolver NativeParserResolver) (*LoadedTask, *ValidatedLogSnapshot, error) {
	snapshot, err := loadSemanticLogSnapshot(lt.path, resolver)
	if err != nil {
		return nil, nil, err
	}
	loaded := semanticLoadedTask(snapshot)
	return loaded, snapshot.validationProof(), nil
}

func loadSemanticSessionMetadata(path string, resolver NativeParserResolver) (_ *LoadedTask, _ *ValidatedLogSnapshot, retErr error) {
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()

	scanner := newPhysicalLogScanner(r.reader, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return nil, nil, err
	}
	nativeParser, err := resolver(scanner.authority.Harness)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
	}
	parser, err := agent.NewLogRecordParser(scanner.authority.Version, nativeParser)
	if err != nil {
		return nil, nil, fmt.Errorf("construct log parser: %w", err)
	}
	loaded := &LoadedTask{LogVersion: scanner.authority.Version}
	found := false
	for scanner.Scan() {
		if found {
			continue
		}
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			if record.Control || scanner.authority.Version == agent.LogVersionV2 {
				return nil, nil, fmt.Errorf("parse task log %s: %w", path, err)
			}
			slog.Warn("failed to parse message", "err", err, "path", path)
			continue
		}
		for _, message := range record.Messages {
			if init, ok := message.Message.(*agent.InitMessage); ok {
				applySessionMetadataMessages(loaded, []agent.Message{init})
			}
		}
		found = loaded.SessionID != "" && loaded.AgentVersion != ""
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	stableInfo, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := newValidatedLogSnapshot(path, r.file, stableInfo, scanner.authority, scanner.headerRaw, scanner.EOFValidated())
	if err != nil {
		return nil, nil, err
	}
	return loaded, snapshot, nil
}

// ExportDiscussion loads one physical task log with its header-authorized native
// parser and renders the resulting task data as markdown.
func ExportDiscussion(path string, resolver NativeParserResolver) (string, error) {
	snapshot, err := loadSemanticLogSnapshot(path, resolver)
	if err != nil {
		return "", err
	}
	var meta *agent.MetaMessage
	var result *agent.MetaResultMessage
	var pr *agent.MetaPRMessage
	messages := make([]agent.Message, 0, len(snapshot.Messages))
	for _, parsed := range snapshot.Messages {
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

func semanticLoadedTask(snapshot *ValidatedLogSnapshot) *LoadedTask {
	loaded := &LoadedTask{LogVersion: snapshot.Authority.Version}
	start := 0
	for _, record := range snapshot.records {
		semanticLoadedMessages(loaded, record.control, snapshot.Messages[start:record.end])
		start = record.end
	}
	return loaded
}

func semanticLoadedMessages(loaded *LoadedTask, control bool, messages []agent.ParsedMessage) {
	if !control {
		for _, parsed := range messages {
			if init, ok := parsed.Message.(*agent.InitMessage); ok {
				applySessionMetadataMessages(loaded, []agent.Message{init})
			}
			loaded.Msgs = append(loaded.Msgs, parsed.Message)
		}
		return
	}
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
		case *agent.MetaMessage:
			continue
		default:
			loaded.Msgs = append(loaded.Msgs, msg)
		}
	}
}

func loadSemanticTailSnapshot(path string, resolver NativeParserResolver, tailBytes int64) (_ *ValidatedLogSnapshot, retErr error) {
	if resolver == nil {
		return nil, errors.New("native parser resolver is nil")
	}
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()

	scanner := newPhysicalLogScanner(r.reader, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return nil, err
	}
	nativeParser, err := resolver(scanner.authority.Harness)
	if err != nil {
		return nil, fmt.Errorf("resolve native parser for harness %q: %w", scanner.authority.Harness, err)
	}
	parser, err := agent.NewLogRecordParser(scanner.authority.Version, nativeParser)
	if err != nil {
		return nil, fmt.Errorf("construct log parser: %w", err)
	}
	if _, err := parser.ParseRecord(scanner.headerRaw); err != nil {
		return nil, fmt.Errorf("parse task log bootstrap %s: %w", path, err)
	}

	messages := make([]agent.ParsedMessage, 0)
	semantic := make([]semanticRecord, 0)
	appendRecord := func(line []byte) error {
		record, err := parser.ParseRecord(line)
		if err != nil {
			if record.Control || scanner.authority.Version == agent.LogVersionV2 {
				return err
			}
			slog.Warn("failed to parse message", "err", err, "path", path)
			return nil
		}
		if len(record.Messages) == 0 {
			return nil
		}
		messages = append(messages, record.Messages...)
		semantic = append(semantic, semanticRecord{end: len(messages), control: record.Control})
		return nil
	}

	var records []tailLogRecord
	if isLogCompressed(path) {
		var total int64
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			records = append(records, tailLogRecord{line: line})
			total += int64(len(line) + 1)
			for total > tailBytes && len(records) > 1 {
				total -= int64(len(records[0].line) + 1)
				records = records[1:]
			}
		}
	} else {
		for scanner.Scan() {
		}
		offset := max(int64(0), r.info.Size()-tailBytes)
		if _, err := r.file.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek %s to %d: %w", path, offset, err)
		}
		tailScanner := bufio.NewScanner(r.file)
		tailScanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
		skipFirst := offset > 0
		for tailScanner.Scan() {
			line := tailScanner.Bytes()
			if skipFirst {
				skipFirst = false
				continue
			}
			if len(line) == 0 {
				continue
			}
			if err := appendRecord(line); err != nil {
				return nil, fmt.Errorf("parse task log %s: %w", path, err)
			}
		}
		if err := tailScanner.Err(); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, record := range records {
		if err := appendRecord(record.line); err != nil {
			return nil, fmt.Errorf("parse task log %s: %w", path, err)
		}
	}
	stableInfo, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return nil, err
	}
	snapshot, err := newValidatedLogSnapshot(path, r.file, stableInfo, scanner.authority, scanner.headerRaw, scanner.EOFValidated())
	if err != nil {
		return nil, err
	}
	snapshot.Messages = messages
	snapshot.records = semantic
	return snapshot, nil
}

func applySemanticSnapshot(lt, loaded *LoadedTask, snapshot *ValidatedLogSnapshot, messages bool) {
	lt.setValidatedSnapshot(snapshot)
	lt.mergeSessionMetadata(loaded)
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
	}
}

// LoadMessagesWithResolver lazily loads full conversation messages with a
// fresh parser derived from the log's validated header.
func (lt *LoadedTask) LoadMessagesWithResolver(resolver NativeParserResolver) error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	loaded, snapshot, err := loadSemanticSnapshot(lt, resolver)
	if err != nil {
		return err
	}
	applySemanticSnapshot(lt, loaded, snapshot, true)
	return nil
}

// LoadSessionMetadataWithResolver loads session metadata with a fresh parser
// derived from the log's validated header.
func (lt *LoadedTask) LoadSessionMetadataWithResolver(resolver NativeParserResolver) error {
	if lt.path == "" || (lt.SessionID != "" && lt.AgentVersion != "") {
		return nil
	}
	loaded, snapshot, err := loadSemanticSessionMetadata(lt.path, resolver)
	if err != nil {
		return err
	}
	lt.setValidatedSnapshot(snapshot)
	lt.mergeSessionMetadata(loaded)
	return nil
}

// LoadMessagesTailWithResolver loads task messages with a fresh parser derived
// from the log's validated header.
func (lt *LoadedTask) LoadMessagesTailWithResolver(resolver NativeParserResolver) error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if !isLogCompressed(lt.path) && lt.LogSize <= maxTailLoadBytes {
		return lt.LoadMessagesWithResolver(resolver)
	}
	slog.Info("load: reading tail only", "path", lt.path, "size", lt.LogSize, "tail", maxTailLoadBytes)
	snapshot, err := loadSemanticTailSnapshot(lt.path, resolver, maxTailLoadBytes)
	if err != nil {
		return err
	}
	loaded := semanticLoadedTask(snapshot)
	applySemanticSnapshot(lt, loaded, snapshot.validationProof(), true)
	if loaded.Title != "" {
		lt.Title = loaded.Title
	}
	if loaded.Result != nil {
		lt.State = loaded.State
		lt.Result = loaded.Result
	}
	if !loaded.LastStateUpdateAt.IsZero() {
		lt.LastStateUpdateAt = loaded.LastStateUpdateAt
	}
	return nil
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

// LoadLogs scans logDir for task log files and loads task metadata.
// Only the header and result trailer are parsed; call LoadMessages for
// full conversation history after supplying a fresh native parser resolver.
func LoadLogs(logDir string) ([]*LoadedTask, error) {
	paths, err := logPaths(logDir, nil)
	if err != nil {
		return nil, err
	}
	return loadLogsFromPaths(paths, false)
}

// LoadLogsForTaskIDs loads metadata for logs whose parsed filename task ID
// matches one of taskIDs. It avoids parsing unrelated purged task logs during
// startup adoption of live runtime instances.
func LoadLogsForTaskIDs(logDir string, taskIDs []string) ([]*LoadedTask, error) {
	ids := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	paths, err := logPaths(logDir, ids)
	if err != nil {
		return nil, err
	}
	found := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		found[taskIDFromLogBase(trimLogExt(filepath.Base(path)))] = struct{}{}
	}
	missing := make([]string, 0)
	for id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)

	tasks, loadErr := loadLogsFromPaths(paths, true)
	if len(missing) > 0 {
		loadErr = errors.Join(loadErr, fmt.Errorf("missing task logs for IDs: %s", strings.Join(missing, ", ")))
	}
	return tasks, loadErr
}

func logPaths(logDir string, taskIDs map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Filter to task log files. If both plain and compressed logs exist for
	// the same base name, prefer the compressed file.
	pathsByBase := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !IsLogName(e.Name()) {
			continue
		}
		base := trimLogExt(e.Name())
		if taskIDs != nil {
			if _, ok := taskIDs[taskIDFromLogBase(base)]; !ok {
				continue
			}
		}
		path := filepath.Join(logDir, e.Name())
		if prev := pathsByBase[base]; prev == "" || isLogCompressed(path) {
			pathsByBase[base] = path
		}
	}
	paths := make([]string, 0, len(pathsByBase))
	for _, p := range pathsByBase {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, nil
}

func loadLogsFromPaths(paths []string, strict bool) ([]*LoadedTask, error) {
	// Parse headers in parallel — each file is independent.
	type result struct {
		lt  *LoadedTask
		err error
	}
	results := make([]result, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Go(func() {
			lt, err := loadLogHeader(p)
			results[i] = result{lt, err}
		})
	}
	wg.Wait()

	var tasks []*LoadedTask
	var errs []error
	for i, r := range results {
		if r.err != nil {
			if strict {
				errs = append(errs, fmt.Errorf("load task log %s: %w", paths[i], r.err))
			} else if !errors.Is(r.err, errNotLogFile) {
				slog.Warn("skipping log file", "file", filepath.Base(paths[i]), "err", r.err)
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

// SetNativeParserResolver installs the task-owned fresh native parser factory.
// The factory is called only after a scan has validated its physical header.
func (lt *LoadedTask) SetNativeParserResolver(resolver NativeParserResolver) {
	lt.resolver = resolver
}

// LoadMessages lazily loads the full conversation messages from the log file.
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

// maxTailLoadBytes is the maximum number of bytes to read from the tail of a
// log file during on-demand loading. Files larger than this are read from the
// tail only, skipping older messages to avoid OOM.
const maxTailLoadBytes = 64 << 20 // 64 MiB

// LoadMessagesTail loads messages from the log file, reading only the tail when
// the log may be large. This is used for on-demand loading to avoid OOM on
// multi-GB log files. Plain logs can seek to the tail; compressed logs are
// scanned once while retaining only the tail window in memory.
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
// log. It resolves a fresh parser only after validating the header and retains
// producer timestamps until replay conversion.
func (lt *LoadedTask) StreamMessages() iter.Seq2[agent.ParsedMessage, error] {
	return func(yield func(agent.ParsedMessage, error) bool) {
		if lt.path == "" {
			return
		}
		if lt.resolver == nil {
			yield(agent.ParsedMessage{}, errors.New("no parser resolver set"))
			return
		}
		r, err := openPhysicalLogReader(lt.path)
		if err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		defer func() { _ = r.Close() }()
		scanner := newPhysicalLogScanner(r.reader, lt.path)
		if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		native, err := lt.resolver(scanner.authority.Harness)
		if err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		parser, err := agent.NewLogRecordParser(scanner.authority.Version, native)
		if err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		if _, err := parser.ParseRecord(scanner.headerRaw); err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		for scanner.Scan() {
			record, err := parser.ParseRecord(scanner.Bytes())
			if err != nil {
				if record.Control || scanner.authority.Version == agent.LogVersionV2 {
					yield(agent.ParsedMessage{}, err)
					return
				}
				slog.Warn("failed to parse message", "err", err, "path", lt.path)
				continue
			}
			for _, message := range record.Messages {
				if record.Control {
					if _, ok := message.Message.(*agent.LogMessage); !ok {
						continue
					}
				}
				if !yield(message, nil) {
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(agent.ParsedMessage{}, err)
			return
		}
		if _, err := verifyPhysicalLog(lt.path, r.file, r.info); err != nil || !scanner.EOFValidated() {
			if err == nil {
				err = errors.New("task log did not reach EOF")
			}
			yield(agent.ParsedMessage{}, err)
		}
	}
}

func (lt *LoadedTask) headerMatches(cached *LoadedTask) bool {
	// Title and Model are mutable projections updated by later session/result
	// records, so they are intentionally not compared with the first header.
	return lt.TaskID == cached.TaskID &&
		lt.Prompt == cached.Prompt &&
		slices.Equal(lt.Repos, cached.Repos) &&
		lt.LogVersion == cached.LogVersion &&
		lt.Harness == cached.Harness &&
		lt.RuntimeName == cached.RuntimeName &&
		lt.StartedAt.Equal(cached.StartedAt) &&
		lt.ForgeIssue == cached.ForgeIssue &&
		lt.Tailscale == cached.Tailscale &&
		lt.USB == cached.USB &&
		lt.Display == cached.Display &&
		lt.Sudo == cached.Sudo &&
		lt.GitHubToken == cached.GitHubToken &&
		lt.BaseImage == cached.BaseImage &&
		lt.ContainerPlatform == cached.ContainerPlatform &&
		lt.MaxCPUs == cached.MaxCPUs &&
		slices.Equal(lt.CacheMounts, cached.CacheMounts) &&
		slices.Equal(lt.Mounts, cached.Mounts) &&
		lt.Effort == cached.Effort
}

func (lt *LoadedTask) setValidatedSnapshot(snapshot *ValidatedLogSnapshot) {
	lt.snapshotMu.Lock()
	lt.snapshot = snapshot
	lt.snapshotMu.Unlock()
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

func applySessionMetadataLine(lt *LoadedTask, typ string, line []byte) bool {
	switch typ {
	case "caic_session":
		var m agent.MetaSessionMessage
		if json.Unmarshal(line, &m) != nil {
			return false
		}
		if m.SessionID != "" {
			lt.SessionID = m.SessionID
		}
		if m.Model != "" {
			lt.Model = m.Model
		}
		if m.AgentVersion != "" {
			lt.AgentVersion = m.AgentVersion
		}
		return m.SessionID != "" || m.AgentVersion != ""
	case "caic_init":
		var m legacyCaicInit
		if json.Unmarshal(line, &m) != nil {
			return false
		}
		if m.SessionID != "" {
			lt.SessionID = m.SessionID
		}
		if m.Model != "" {
			lt.Model = m.Model
		}
		if m.Version != "" {
			lt.AgentVersion = m.Version
		}
		return m.SessionID != "" || m.Version != ""
	default:
		return false
	}
}

func applySessionMetadataMessages(lt *LoadedTask, msgs []agent.Message) bool {
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
		return init.SessionID != "" || init.Version != ""
	}
	return false
}

// maybeLogTailRecord cheaply filters ordinary conversation lines before JSON decoding.
func maybeLogTailRecord(line []byte) bool {
	return bytes.Contains(line, []byte(`"caic_`)) ||
		bytes.Contains(line, []byte(`"system"`)) ||
		bytes.Contains(line, []byte(`"result"`))
}

func (s *logTailScan) apply(lt *LoadedTask, line []byte) {
	if !maybeLogTailRecord(line) {
		return
	}
	var typeEnv v1TypeEnvelope
	if json.Unmarshal(line, &typeEnv) != nil {
		return
	}
	switch typeEnv.Type {
	case "caic_session", "caic_init":
		if lt.LogVersion == agent.LogVersionV1 {
			applySessionMetadataLine(lt, typeEnv.Type, line)
		}
		return
	case "caic_pr":
		var mp agent.MetaPRMessage
		if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
			lt.ForgeOwner = mp.ForgeOwner
			lt.ForgeRepo = mp.ForgeRepo
			lt.ForgePR = mp.ForgePR
		}
	case "caic_diff_stat":
		noteDiffStatLine(lt, line)
	case "caic_result":
		var mr agent.MetaResultMessage
		if err := json.Unmarshal(line, &mr); err == nil {
			var raw map[string]json.RawMessage
			if s.fw != nil && json.Unmarshal(line, &raw) == nil {
				s.fw.Warn("caic_result", jsonutil.CollectUnknown(raw, resultKnown))
			}
			applyMetaResult(lt, &mr)
		}
	case "system":
		var sm tailInit
		if json.Unmarshal(line, &sm) == nil && sm.Subtype == "init" {
			if sm.Model != "" {
				lt.Model = sm.Model
			}
			if sm.Version != "" {
				lt.AgentVersion = sm.Version
			}
		}
	case "result":
		var rm tailResult
		if json.Unmarshal(line, &rm) == nil {
			s.lastResultCostUSD = rm.TotalCostUSD
			s.lastResultDuration = time.Duration(rm.DurationMs) * time.Millisecond
			s.lastResultNumTurns = rm.NumTurns
			s.lastResultUsage = rm.Usage
		}
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

func loadLogSessionMetadata(path string, parseFn func([]byte) ([]agent.Message, error)) (_ *LoadedTask, retErr error) {
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()

	scanner := newPhysicalLogScanner(r.reader, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return nil, err
	}
	lt := &LoadedTask{LogVersion: scanner.authority.Version}
	found := false
	for scanner.Scan() {
		if found {
			continue
		}
		if scanner.authority.Version != agent.LogVersionV1 {
			continue
		}
		line := scanner.Bytes()
		typ := scanner.Type()
		if applySessionMetadataLine(lt, typ, line) {
			found = true
			continue
		}
		if parseFn == nil {
			continue
		}
		msgs, err := parseFn(line)
		if err == nil && applySessionMetadataMessages(lt, msgs) {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if _, err := verifyPhysicalLog(path, r.file, r.info); err != nil {
		return nil, err
	}
	return lt, nil
}

// loadLogHeader reads the metadata header and result trailer from a task log.
// It does NOT parse individual messages — call LoadMessages for that. The path
// is stored for lazy loading.
func loadLogHeader(path string) (_ *LoadedTask, retErr error) {
	if isLogCompressed(path) {
		return loadCompressedLogHeaderWithOptions(path, true, true)
	}
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	return loadPlainLogHeader(path, r, r.reader, r.file)
}

func loadPlainLogHeader(path string, r *physicalLogReader, source io.Reader, tailSource io.ReaderAt) (*LoadedTask, error) {
	fw := &jsonutil.FieldWarner{}
	scanner := newPhysicalLogScanner(source, path)
	meta, err := scanner.ReadHeader(fw)
	if err != nil {
		return nil, err
	}

	base := trimLogExt(filepath.Base(path))
	taskIDStr := taskIDFromLogBase(base)
	lt := loadedTaskFromMeta(path, taskIDStr, &meta, r.info.ModTime().UTC(), r.info.Size())

	// The authority pass already visits every record. Capture backend-neutral
	// session snapshots while validating segment headers so live adoption does
	// not need a second full scan for the usual persisted metadata form.
	for scanner.Scan() {
		if scanner.authority.Version == agent.LogVersionV1 {
			applySessionMetadataLine(lt, scanner.Type(), scanner.Bytes())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	const tailSize = 65536
	offset := max(int64(0), r.info.Size()-tailSize)
	buf := make([]byte, r.info.Size()-offset)
	n, readErr := tailSource.ReadAt(buf, offset)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	tail := logTailScan{fw: fw}
	for line := range bytes.SplitSeq(buf[:n], []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 && scanner.authority.Version == agent.LogVersionV1 {
			tail.apply(lt, line)
		}
	}
	tail.finish(lt)
	stableInfo, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return nil, err
	}
	snapshot, err := newValidatedLogSnapshot(path, r.file, stableInfo, scanner.authority, scanner.headerRaw, scanner.EOFValidated())
	if err != nil {
		return nil, err
	}
	lt.setValidatedSnapshot(snapshot)
	return lt, nil
}

func loadCompressedLogHeaderWithOptions(path string, useSummary, storeSummary bool) (_ *LoadedTask, retErr error) {
	r, err := openPhysicalLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	return loadCompressedLogHeaderFromReader(path, r, useSummary, storeSummary)
}

// loadCompressedLogHeaderFromReader is the reader-owned production core used
// by loadCompressedLogHeaderWithOptions. Keeping it separate lets pass-count
// tests observe the same zstd scan without changing filesystem authority.
func loadCompressedLogHeaderFromReader(path string, r *physicalLogReader, useSummary, storeSummary bool) (*LoadedTask, error) {
	fw := &jsonutil.FieldWarner{}
	scanner := newPhysicalLogScanner(r.reader, path)
	meta, err := scanner.ReadHeader(fw)
	if err != nil {
		return nil, err
	}
	if useSummary {
		if lt, ok := loadLogSummary(path, r.file, r.info, scanner.authority, &meta); ok {
			return lt, nil
		}
	}
	base := trimLogExt(filepath.Base(path))
	taskIDStr := taskIDFromLogBase(base)
	lt := loadedTaskFromMeta(path, taskIDStr, &meta, r.info.ModTime().UTC(), r.info.Size())

	tail := logTailScan{fw: fw}
	for scanner.Scan() {
		if scanner.authority.Version == agent.LogVersionV1 {
			tail.apply(lt, scanner.Bytes())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	tail.finish(lt)
	stableInfo, err := verifyPhysicalLog(path, r.file, r.info)
	if err != nil {
		return nil, err
	}
	snapshot, err := newValidatedLogSnapshot(path, r.file, stableInfo, scanner.authority, scanner.headerRaw, scanner.EOFValidated())
	if err != nil {
		return nil, err
	}
	lt.setValidatedSnapshot(snapshot)
	if storeSummary {
		if err := storeLogSummaryForFile(lt, r.file, stableInfo); err != nil {
			if _, verifyErr := verifyPhysicalLog(path, r.file, stableInfo); verifyErr != nil {
				return nil, errors.Join(err, verifyErr)
			}
			slog.Warn("task log summary: write failed", "path", path, "err", err)
		}
	}
	return lt, nil
}

// tsToTime converts a Unix epoch float64 (seconds with sub-second precision)
// to a time.Time in UTC.
func tsToTime(ts float64) time.Time {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// parseState converts a state string back to a State value.
func parseState(s string) State {
	switch s {
	case "pending":
		return StatePending
	case "branching":
		return StateBranching
	case "provisioning":
		return StateProvisioning
	case "starting":
		return StateStarting
	case "running":
		return StateRunning
	case "waiting":
		return StateWaiting
	case "asking":
		return StateAsking
	case "has_plan":
		return StateHasPlan
	case "pulling":
		return StatePulling
	case "pushing":
		return StatePushing
	case "stopping":
		return StateStopping
	case "stopped":
		return StateStopped
	case "purging":
		return StatePurging
	case "crashed":
		return StateCrashed
	case "failed":
		return StateFailed
	case "purged", "terminated": // "terminated" is for backward compat with pre-rename logs; remove once old logs age out
		return StatePurged
	default:
		return StateFailed
	}
}
