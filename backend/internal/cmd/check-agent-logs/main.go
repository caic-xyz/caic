// Command check-agent-logs deletes obsolete task logs, validates recent v3 harness records against direction-specific genai wire DTOs and caic's parsers, flags unclassified Claude background tools, and lists the newest harness version each harness ran.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agydata "github.com/caic-xyz/caic/backend/internal/agent/antigravity/data"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	agentharness "github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/klauspost/compress/zstd"
	antigravitydto "github.com/maruel/genai/providers/antigravity"
	claudedto "github.com/maruel/genai/providers/claudecode"
	codexdto "github.com/maruel/genai/providers/codex"
	pidto "github.com/maruel/genai/providers/pi"
	"golang.org/x/mod/semver"
)

type config struct {
	dir   string
	since time.Duration
	all   bool
	paths []string
}

type fileCheck struct {
	findings []finding
	version  int
	checked  bool
	harness  string
	// agentVersions lists each harness version the log's processes reported.
	agentVersions []string
}

// newestVersion tracks the newest version a harness reported across logs.
type newestVersion struct {
	version string
	logs    int    // Logs that ran version.
	latest  string // Last such log path in sorted order.
}

type finding struct {
	path    string
	line    int
	harness string
	dto     string
	err     error
	hint    string
}

func checkMessage(path string, line int, harness string, message []byte, input bool, openCodeMethods map[openCodeRequestKey]acp.Method) *finding {
	if len(message) != 0 && message[0] == '"' {
		var reason string
		if err := json.Unmarshal(message, &reason); err == nil {
			return &finding{
				path:    path,
				line:    line,
				harness: harness,
				dto:     "relay diagnostic",
				err:     errors.New(reason),
				hint:    "inspect: backend/internal/agent/relay/relay_v2.py",
			}
		}
	}
	var err error
	var dto, hint string
	switch harness {
	case "antigravity":
		if input {
			// caic's record, unlike the genai DTO, has no lenient custom unmarshaler.
			dto = "InputMessage"
			hint = "update: backend/internal/agent/antigravity/data/prompt.go"
			err = strictDecode(message, &agydata.InputMessage{})
		} else {
			dto = "StreamEvent"
			err = strictDecode(message, &antigravitydto.StreamEvent{})
		}
	case "claude":
		if input {
			dto, err = checkClaudeInput(message)
		} else {
			dto, err = checkClaude(message)
		}
	case "codex":
		if input {
			dto, err = checkCodexInput(message)
		} else {
			dto, err = checkCodex(message)
		}
	case "pi":
		if input {
			dto, err = checkPiInput(message)
		} else {
			dto, err = checkPi(message)
		}
	case "dsh", "opencode":
		dto, err = checkOpenCode(message, input, openCodeMethods)
	default:
		return &finding{
			path:    path,
			line:    line,
			harness: harness,
			dto:     "harness dispatch",
			err:     fmt.Errorf("unrecognized harness %q; add its DTO and checker dispatch", harness),
		}
	}
	if err == nil {
		return nil
	}
	return &finding{path: path, line: line, harness: harness, dto: dto, err: err, hint: hint}
}

func (f finding) String() string {
	hint := f.hint
	if hint == "" && (f.harness == "opencode" || f.harness == "dsh") {
		hint = "update: ~/src/genai/acp/dto.go"
	}
	if hint == "" {
		hint = "update: ~/src/genai/providers/" + providerDir(f.harness) + "/dto.go"
	}
	return fmt.Sprintf("%s:%d: harness=%s dto=%s: %v\n  %s", f.path, f.line, f.harness, f.dto, f.err, hint)
}

func providerDir(harness string) string {
	if harness == "claude" {
		return "claudecode"
	}
	return harness
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run processes all discoverable logs even when discovery, file I/O, or reporting fails.
func run(args []string, out io.Writer) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	paths, runErr := logPaths(cfg)
	var findings []finding
	var checked int
	newest := map[string]*newestVersion{}
	resolver := backends.Default("", nil)
	var cutoff time.Time
	if len(cfg.paths) == 0 && !cfg.all {
		cutoff = time.Now().Add(-cfg.since)
	}
	for _, path := range paths {
		result, err := checkFile(path, cutoff, resolver)
		if err != nil {
			runErr = errors.Join(runErr, err)
			continue
		}
		switch result.version {
		case 1, 2:
			if err := os.Remove(path); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("delete obsolete v%d task log %s: %w", result.version, path, err))
				continue
			}
			if _, err := fmt.Fprintf(out, "deleted obsolete v%d task log %s\n", result.version, path); err != nil {
				runErr = errors.Join(runErr, err)
			}
		case agent.LogVersionV3:
			if result.checked {
				checked++
			}
			for _, v := range result.agentVersions {
				n := newest[result.harness]
				if n == nil || semver.Compare("v"+v, "v"+n.version) > 0 {
					n = &newestVersion{version: v}
					newest[result.harness] = n
				}
				if v == n.version {
					n.logs++
					n.latest = path
				}
			}
		}
		findings = append(findings, result.findings...)
	}
	findings = dedupe(findings)
	for _, f := range findings {
		if _, err := fmt.Fprintln(out, f.String()); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	// Compare with the release pinned in each genai DTO file header.
	for _, h := range slices.Sorted(maps.Keys(newest)) {
		n := newest[h]
		if _, err := fmt.Fprintf(out, "harness=%s newest version %s in %d logs; latest: %s\n", h, n.version, n.logs, n.latest); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	if _, err := fmt.Fprintf(out, "checked %d v3 task logs; found %d issues\n", checked, len(findings)); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if len(findings) != 0 {
		runErr = errors.Join(runErr, errors.New("agent log validation failed"))
	}
	return runErr
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("check-agent-logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("dir", "task-log directory; matching v1 and v2 logs are deleted regardless of age", func(dir string) error {
		if dir == "" {
			return errors.New("-dir must not be empty")
		}
		cfg.dir = dir
		return nil
	})
	fs.DurationVar(&cfg.since, "since", 72*time.Hour, "validate v3 bodies modified within this duration; explicit paths ignore this cutoff")
	fs.BoolVar(&cfg.all, "all", false, "validate all v3 bodies regardless of modification time")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.since < 0 {
		return config{}, errors.New("-since must not be negative")
	}
	cfg.paths = fs.Args()
	if len(cfg.paths) == 0 && cfg.dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("resolve default task-log directory: %w", err)
		}
		cfg.dir = filepath.Join(home, ".cache", "caic", "tasks")
	}
	return cfg, nil
}

// logPaths returns normalized, distinct paths alongside errors from other selections.
func logPaths(cfg config) ([]string, error) {
	var roots []string
	if len(cfg.paths) == 0 {
		roots = []string{cfg.dir}
	} else {
		roots = cfg.paths
	}
	var paths []string
	var pathErr error
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			pathErr = errors.Join(pathErr, fmt.Errorf("stat %s: %w", root, err))
			continue
		}
		if !info.IsDir() {
			if isLogPath(root) {
				paths = append(paths, root)
			}
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			pathErr = errors.Join(pathErr, fmt.Errorf("read %s: %w", root, err))
		}
		for _, entry := range entries {
			if entry.IsDir() || !isLogPath(entry.Name()) {
				continue
			}
			path := filepath.Join(root, entry.Name())
			paths = append(paths, path)
		}
	}
	normalized := paths[:0]
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			pathErr = errors.Join(pathErr, fmt.Errorf("resolve %s: %w", path, err))
			continue
		}
		normalized = append(normalized, abs)
	}
	slices.Sort(normalized)
	return slices.Compact(normalized), pathErr
}

func isLogPath(path string) bool {
	return strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".jsonl.zst")
}

// checkFile closes the reader before returning a version eligible for deletion.
func checkFile(path string, cutoff time.Time, resolver agent.Backends) (result fileCheck, err error) {
	r, err := openLog(path)
	if err != nil {
		return fileCheck{}, err
	}
	defer func() {
		if closeErr := r.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", path, closeErr))
		}
	}()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), taskslog.MaxRecordSize)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return fileCheck{}, fmt.Errorf("read %s: %w", path, err)
		}
		return fileCheck{}, fmt.Errorf("validate %s:1: missing metadata header", path)
	}

	version, harnessID, err := inspectHeader(sc.Bytes())
	if err != nil {
		return fileCheck{}, fmt.Errorf("validate %s:1: %w", path, err)
	}
	if version != agent.LogVersionV3 {
		return fileCheck{version: version}, nil
	}
	if !cutoff.IsZero() {
		info, err := os.Stat(path)
		if err != nil {
			return fileCheck{}, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.ModTime().Before(cutoff) {
			return fileCheck{version: version}, nil
		}
	}

	checker := &messageChecker{path: path, harness: harnessID, line: 1, openCodeMethods: make(map[openCodeRequestKey]acp.Method), claudeTools: claudeBackgroundTools{names: make(map[claudeToolKey]string)}}
	parser, err := agent.NewLogRecordParser(checker)
	if err != nil {
		return fileCheck{}, fmt.Errorf("create task-log parser: %w", err)
	}
	// Replay caic's own parsers as task loading does: the full wire parser
	// interprets every record, and the metadata projection reports versions.
	h := agentharness.Name(harnessID)
	wireParser, err := resolver.ResolveParser(h)
	if err != nil {
		return fileCheck{findings: []finding{{path: path, line: 1, harness: harnessID, dto: "harness dispatch", err: err, hint: "add: its backend to backends.Default and its DTO checker dispatch"}}, version: version, checked: true, harness: harnessID}, nil
	}
	wire, err := newReplay(wireParser, "caic wire parser")
	if err != nil {
		return fileCheck{}, err
	}
	metadataParser, err := resolver.ResolveMetadataParser(h)
	if err != nil {
		return fileCheck{}, err
	}
	metadata, err := newReplay(metadataParser, "caic metadata parser")
	if err != nil {
		return fileCheck{}, err
	}
	versions := &versionCollector{replay: metadata}
	for ; ; checker.line++ {
		if _, err := parser.ParseRecord(sc.Bytes()); err != nil {
			return fileCheck{}, fmt.Errorf("validate %s:%d: %w", path, checker.line, err)
		}
		wire.parse(sc.Bytes(), checker.line)
		versions.parse(sc.Bytes(), checker.line)
		if !sc.Scan() {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return fileCheck{}, fmt.Errorf("read %s: %w", path, err)
	}
	findings := checker.findings
	for _, r := range []*replay{wire, versions.replay} {
		// A record the checker rejected already explains the parser failure.
		if r.err != nil && !slices.ContainsFunc(checker.findings, func(f finding) bool { return f.line == r.line }) {
			findings = append(findings, finding{path: path, line: r.line, harness: harnessID, dto: r.dto, err: r.err, hint: "fix: caic's " + harnessID + " parser under backend/internal/agent"})
		}
	}
	return fileCheck{findings: findings, version: version, checked: true, harness: harnessID, agentVersions: versions.seen}, nil
}

// replay runs one caic parser over a log. A rejected record would fail the
// task, and leaves the parser state undefined, so replay stops there and
// retains the record's line and error.
type replay struct {
	parser *agent.LogRecordParser
	dto    string
	line   int
	err    error // Set once the parser rejects a record; replay then stops.
}

func newReplay(native agent.MessageParser, dto string) (*replay, error) {
	parser, err := agent.NewLogRecordParser(native)
	if err != nil {
		return nil, fmt.Errorf("create task-log %s: %w", dto, err)
	}
	return &replay{parser: parser, dto: dto}, nil
}

// parse returns the record's messages and whether the parser accepted it.
func (r *replay) parse(record []byte, line int) (agent.ParsedRecord, bool) {
	if r.err != nil {
		return agent.ParsedRecord{}, false
	}
	parsed, err := r.parser.ParseRecord(record)
	if err != nil {
		r.line, r.err = line, err
		return agent.ParsedRecord{}, false
	}
	return parsed, true
}

// versionCollector folds session metadata to list each distinct reported
// version.
type versionCollector struct {
	*replay

	session agent.SessionMetadata
	seen    []string
}

func (v *versionCollector) parse(record []byte, line int) {
	parsed, ok := v.replay.parse(record, line)
	if !ok {
		return
	}
	for i := range parsed.Messages {
		v.session.Apply(parsed.Messages[i].Message)
	}
	if v.session.AgentVersion != "" && !slices.Contains(v.seen, v.session.AgentVersion) {
		v.seen = append(v.seen, v.session.AgentVersion)
	}
}

// messageChecker validates native DTOs in the direction selected by the log parser.
type messageChecker struct {
	path            string
	harness         string
	line            int
	openCodeMethods map[openCodeRequestKey]acp.Method
	claudeTools     claudeBackgroundTools
	findings        []finding
}

func (c *messageChecker) ParseInput(message []byte) ([]agent.Message, error) {
	return c.check(message, true)
}
func (c *messageChecker) ParseOutput(message []byte) ([]agent.Message, error) {
	return c.check(message, false)
}
func (c *messageChecker) check(message []byte, input bool) ([]agent.Message, error) {
	if f := checkMessage(c.path, c.line, c.harness, message, input, c.openCodeMethods); f != nil {
		c.findings = append(c.findings, *f)
	}
	if c.harness == "claude" && !input {
		if err := c.claudeTools.observe(message); err != nil {
			c.findings = append(c.findings, finding{
				path:    c.path,
				line:    c.line,
				harness: c.harness,
				dto:     "*claudecode.OutputSystemMsg",
				err:     err,
				hint:    "update: backend/internal/agent/claudecode/native_subagent.go",
			})
		}
	}
	return nil, nil
}

// openCodeRequestKey keeps client and server request IDs in separate namespaces.
type openCodeRequestKey struct {
	input bool
	id    string
}

// inspectHeader proves the format identity before a log can be deleted.
func inspectHeader(data []byte) (version int, harness string, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	opening, err := dec.Token()
	if err != nil {
		return 0, "", err
	}
	if opening != json.Delim('{') {
		return 0, "", errors.New("metadata header must be a JSON object")
	}
	seen := make(map[string]struct{})
	var token, legacyToken string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, "", err
		}
		field, ok := key.(string)
		if !ok {
			return 0, "", errors.New("metadata field name must be a string")
		}
		for _, name := range []string{"t", "type", "version"} {
			if field != name && strings.EqualFold(field, name) {
				return 0, "", fmt.Errorf("metadata identity field %q must use exact casing", field)
			}
		}
		if _, duplicate := seen[field]; duplicate {
			return 0, "", fmt.Errorf("duplicate metadata field %q", field)
		}
		seen[field] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return 0, "", err
		}
		switch field {
		case "version":
			err = json.Unmarshal(value, &version)
		case "t":
			err = json.Unmarshal(value, &token)
		case "type":
			err = json.Unmarshal(value, &legacyToken)
		case "harness":
			err = json.Unmarshal(value, &harness)
		}
		if err != nil {
			return 0, "", fmt.Errorf("metadata field %q: %w", field, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return 0, "", err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, "", errors.New("metadata header has trailing data")
	}
	_, hasType := seen["type"]
	_, hasToken := seen["t"]
	if hasType && hasToken {
		return 0, "", errors.New("metadata header has ambiguous type and t discriminators")
	}
	switch version {
	case 1:
		if legacyToken != "caic_meta" {
			return 0, "", errors.New("missing v1 metadata header")
		}
	case 2, agent.LogVersionV3:
		if token != "caic_meta" {
			return 0, "", fmt.Errorf("missing v%d metadata header", version)
		}
	default:
		return 0, "", fmt.Errorf("unsupported log version %d", version)
	}
	if version == agent.LogVersionV3 && harness == "" {
		return 0, "", errors.New("v3 metadata is missing harness")
	}
	return version, harness, nil
}

func openLog(path string) (io.ReadCloser, error) {
	// #nosec G304 -- path comes from an explicit command argument or the configured log directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if !strings.HasSuffix(path, ".zst") {
		return f, nil
	}
	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open zstd %s: %w", path, err), f.Close())
	}
	return &zstdLog{Decoder: zr, file: f}, nil
}

type zstdLog struct {
	*zstd.Decoder

	file *os.File
}

func (z *zstdLog) Close() error {
	z.Decoder.Close()
	return z.file.Close()
}

func checkClaudeInput(data []byte) (string, error) {
	t, err := agent.JSONString(data, "type")
	if err != nil {
		return "InputType", err
	}
	dst := claudedto.NewInputMsg(claudedto.InputType(t))
	if dst == nil {
		return "InputType", fmt.Errorf("unrecognized Claude Code input type %q; add its DTO to claudecode.NewInputMsg", t)
	}
	return fmt.Sprintf("%T", dst), strictDecode(data, dst)
}

func checkCodexInput(data []byte) (string, error) {
	// JSONRPCRequest is a superset of JSONRPCNotification; request IDs start at 1.
	var request codexdto.JSONRPCRequest
	if err := strictDecode(data, &request); err != nil {
		return "JSONRPCRequest", err
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return "JSONRPCRequest", errors.New("codex input is missing JSON-RPC 2.0 method")
	}
	if request.Method == "initialized" {
		if request.ID != 0 || len(request.Params) != 0 {
			return "JSONRPCNotification", errors.New("codex initialized input must be a parameterless notification")
		}
		return "JSONRPCNotification", nil
	}
	if request.ID == 0 || isEmptyJSON(request.Params) {
		return "JSONRPCRequest", fmt.Errorf("codex request %q is missing id or params", request.Method)
	}
	dst := codexdto.NewRequestParams(request.Method)
	if dst == nil {
		return "JSONRPCRequest", fmt.Errorf("unrecognized Codex request method %q; add its DTO to codex.NewRequestParams", request.Method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(request.Params, dst)
}

func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func checkClaude(data []byte) (string, error) {
	var probe claudedto.OutputTypeProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "OutputTypeProbe", err
	}
	dst := claudedto.NewOutputMsg(&probe)
	if dst == nil {
		if probe.Type == claudedto.OutputSystem {
			return "OutputSystemMsg", fmt.Errorf("unrecognized Claude Code system subtype %q; add its DTO to claudecode.NewOutputMsg", probe.Subtype)
		}
		return "OutputTypeProbe", fmt.Errorf("unrecognized Claude Code output type %q; add its DTO to claudecode.NewOutputMsg", probe.Type)
	}
	return fmt.Sprintf("%T", dst), strictDecode(data, dst)
}

func checkCodex(data []byte) (string, error) {
	var msg codexdto.JSONRPCMessage
	if err := strictDecode(data, &msg); err != nil {
		return "JSONRPCMessage", err
	}
	if msg.Method == "" {
		if msg.ID != nil {
			return "JSONRPCMessage", nil
		}
		return "JSONRPCMessage", errors.New("codex JSON-RPC message is missing method")
	}
	if isEmptyJSON(msg.Params) {
		return "JSONRPCMessage", fmt.Errorf("codex notification %q is missing params", msg.Method)
	}
	dst := codexdto.NewNotificationParams(msg.Method)
	if dst == nil {
		return "JSONRPCMessage", fmt.Errorf("unrecognized Codex notification method %q; add its DTO to codex.NewNotificationParams", msg.Method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(msg.Params, dst)
}

func checkPi(data []byte) (string, error) {
	var probe pidto.LineProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "LineProbe", err
	}
	dst := pidto.NewEvent(probe.Type)
	if dst == nil {
		return "LineProbe", fmt.Errorf("unrecognized Pi output event type %q; add its DTO to pi.NewEvent", probe.Type)
	}
	dto := fmt.Sprintf("%T", dst)
	if err := strictDecode(data, dst); err != nil {
		return dto, err
	}
	return dto, validatePiStrictContentBlocks(probe.Type, data)
}

func checkPiInput(data []byte) (string, error) {
	var probe pidto.LineProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "LineProbe", err
	}
	dst := pidto.NewCommand(probe.Type)
	if dst == nil {
		return "LineProbe", fmt.Errorf("unrecognized Pi input command type %q; add its DTO to pi.NewCommand", probe.Type)
	}
	return fmt.Sprintf("%T", dst), strictDecode(data, dst)
}

func checkOpenCode(data []byte, input bool, methods map[openCodeRequestKey]acp.Method) (string, error) {
	var probe acp.MessageProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return "MessageProbe", err
	}
	if probe.Type != "" {
		return "JSONRPCMessage", fmt.Errorf("unexpected native type %q", probe.Type)
	}

	var msg acp.JSONRPCMessage
	if err := strictDecode(data, &msg); err != nil {
		return "JSONRPCMessage", err
	}
	if msg.Method != "" {
		if isEmptyJSON(msg.Params) {
			return "JSONRPCMessage", fmt.Errorf("OpenCode request or notification %q is missing params", msg.Method)
		}
		if msg.Method.FromAgent() == input {
			return "JSONRPCMessage", fmt.Errorf("OpenCode method %q in the wrong direction", msg.Method)
		}
		dto, err := checkOpenCodeParams(msg.Method, msg.Params)
		if err != nil || msg.ID == nil {
			return dto, err
		}
		if methods != nil {
			methods[openCodeRequestKey{input: input, id: string(msg.ID)}] = msg.Method
		}
		return dto, nil
	}
	if msg.ID == nil {
		return "JSONRPCMessage", errors.New("OpenCode JSON-RPC message is missing method and id")
	}
	if methods == nil {
		return "JSONRPCMessage", fmt.Errorf("unmatched OpenCode response id %s", msg.ID)
	}
	key := openCodeRequestKey{input: !input, id: string(msg.ID)}
	method, ok := methods[key]
	delete(methods, key)
	if !ok {
		return "JSONRPCMessage", fmt.Errorf("unmatched OpenCode response id %s", msg.ID)
	}
	if msg.Error != nil {
		return "JSONRPCMessage", nil
	}
	if isEmptyJSON(msg.Result) {
		return "JSONRPCMessage", fmt.Errorf("OpenCode response for %q is missing result", method)
	}
	return checkOpenCodeResponse(method, msg.Result)
}

func isEmptyJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// checkOpenCodeParams validates the params of a request or notification in
// either direction, and the variant inside a session/update.
func checkOpenCodeParams(method acp.Method, params json.RawMessage) (string, error) {
	if method != acp.MethodSessionUpdate {
		dst := acp.NewParams(method)
		if dst == nil {
			return "JSONRPCMessage", fmt.Errorf("unrecognized OpenCode request method %q; add its DTO to acp.NewParams", method)
		}
		return fmt.Sprintf("%T", dst), strictDecode(params, dst)
	}
	var updateParams acp.SessionUpdateParams
	if err := strictDecode(params, &updateParams); err != nil {
		return "*acp.SessionUpdateParams", err
	}
	if isEmptyJSON(updateParams.Update) {
		return "*acp.SessionUpdateParams", errors.New("session/update params are missing update")
	}
	var probe acp.UpdateProbe
	if err := json.Unmarshal(updateParams.Update, &probe); err != nil {
		return "UpdateProbe", err
	}
	dst := acp.NewSessionUpdate(probe.SessionUpdate)
	if dst == nil {
		return "UpdateProbe", fmt.Errorf("unrecognized OpenCode session update %q; add its DTO to acp.NewSessionUpdate", probe.SessionUpdate)
	}
	return fmt.Sprintf("%T", dst), strictDecode(updateParams.Update, dst)
}

func checkOpenCodeResponse(method acp.Method, result json.RawMessage) (string, error) {
	dst := acp.NewResult(method)
	if dst == nil {
		return "JSONRPCMessage", fmt.Errorf("unrecognized OpenCode response method %q; add its DTO to acp.NewResult", method)
	}
	return fmt.Sprintf("%T", dst), strictDecode(result, dst)
}

func dedupe(in []finding) []finding {
	seen := map[string]struct{}{}
	out := make([]finding, 0, len(in))
	for _, f := range in {
		key := strings.Join([]string{f.harness, f.dto, f.err.Error()}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path == out[j].path {
			return out[i].line < out[j].line
		}
		return out[i].path < out[j].path
	})
	return out
}
