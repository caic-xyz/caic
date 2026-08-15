// Package agent defines shared types and infrastructure for coding agent
// backends. Backend implementations live in sub-packages (e.g. agent/claudecode).
//
// # Message dispatch
//
// Conn.ReadMessages reads agent stdout and forwards parsed messages to
// Options.MsgCh. The task checkout drains this channel in a separate
// goroutine (startMessageDispatch) that performs blocking side-effects: git
// fetch, diff stat, branch locking. The channel decouples the fast reader
// from these slow operations — without it, a blocked git fetch would
// backpressure the agent's stdout pipe and risk deadlock.
//
// Conn is an interface. Backends that need to intercept messages (e.g.
// injecting control commands after initialization) wrap the default Conn
// returned by NewConn to override ReadMessages.
//
// # Relay shutdown protocol
//
// Each agent runs inside a container behind a relay daemon (relay.py) that
// survives SSH disconnects. Graceful shutdown uses a null-byte (\x00)
// sentinel written to stdin:
//
// Flow 1 — One task is purged (user action or container death):
//
//	Server calls Checkout.Cleanup → Session.Stop writes \x00\n then closes
//	stdin → attach_client forwards sentinel through Unix socket, sees stdin
//	EOF, exits → _client_reader sets shutdown_event → _shutdown_watchdog
//	closes proc.stdin, sends SIGINT, escalates to SIGTERM/SIGKILL →
//	reader_thread sees stdout EOF → server kills container.
//
// Flow 2 — Backend restarts (upgrade, crash):
//
//	SSH connections are severed → attach_client sees stdin EOF and disconnects
//	(no \x00 sent) → relay daemon + agent keep running → on restart, server
//	discovers the container during task import, reads output.jsonl to restore
//	conversation state, and calls relay.py attach --offset N to reconnect.
package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/relay"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// ImageData carries a single base64-encoded image for multi-modal input.
type ImageData struct {
	MediaType string `json:"media_type"` // e.g. "image/png", "image/jpeg"
	Data      string `json:"data"`       // base64-encoded
}

// Prompt bundles user text with optional images for a single interaction.
type Prompt struct {
	Text   string      `json:"text"`
	Images []ImageData `json:"images,omitempty,omitzero"`
}

// Options configures an agent session launch.
type Options struct {
	Logger          *slog.Logger // Non-nil session logger.
	Target          runtime.ConnectionTarget
	Dir             string // Working directory inside the runtime.
	Model           string // Model alias ("opus", "sonnet", "haiku", "fable") or full ID. Empty = default.
	Effort          string // Thinking effort (e.g. "low", "medium", "high", "max"). Empty = default.
	InitialPrompt   Prompt // Initial prompt; never mutated after creation.
	ResumeSessionID string
	RelayOffset     int64 // Byte offset into relay output.jsonl for AttachRelay.
	// PendingUserActions is the restored user-facing work that still needs
	// input after AttachRelay reconnects. It must not contain backend-only
	// protocol state such as keepalive, auto-allow, or environment control
	// messages.
	PendingUserActions []PendingUserAction
	MsgCh              chan<- ParsedMessage // Receives parsed physical records from the agent.
	Log                LogSink              // Non-nil task-owned physical task-log authority; use DiscardLogSink{Version: version} when persistence is unnecessary.
	StripEnv           []string             // Env var names for relay to strip from subprocess and emit as caic_stripped_env.
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair WritePrompt and ParseMessage
// for the same protocol.
type WireFormat interface {
	// WritePrompt writes a user prompt to the agent's stdin in the
	// backend's wire format. logW receives a copy.
	WritePrompt(w io.Writer, p Prompt, log LogSink) error

	// ParseMessage decodes a single NDJSON line into one or more typed
	// Messages. A single wire line may produce multiple semantic messages.
	ParseMessage(line []byte) ([]Message, error)
}

// CompactCommand is an optional interface for WireFormat implementations that
// support context compaction. The server checks for this capability to
// conditionally enable the compact button in the UI.
type CompactCommand interface {
	WriteCompact(w io.Writer, instructions string, log LogSink) error
}

// Conn handles wire-format I/O for a single agent session. It is safe for
// concurrent use. Backends can wrap a Conn to intercept messages (e.g.
// injecting control commands after initialization).
type Conn interface {
	// SendPrompt writes a user message to the agent's stdin.
	SendPrompt(p Prompt) error
	// SendRaw writes pre-encoded NDJSON bytes to the agent's stdin.
	SendRaw(data []byte) error
	// SendCompact sends a compact/context-reduction command.
	SendCompact(instructions string) error
	// ReadMessages runs the message read loop: reads NDJSON lines from r,
	// parses them, writes raw lines to the log, and forwards parsed messages
	// to msgCh.
	ReadMessages(r io.Reader, msgCh chan<- ParsedMessage) error
	// SendStop sends the null-byte sentinel to trigger graceful agent shutdown.
	// Best-effort: returns when ctx is done if the write blocks.
	SendStop(ctx context.Context)
	// Close closes the stdin pipe.
	Close() error
}

// conn is the default Conn implementation.
type conn struct {
	ctx     context.Context
	logger  *slog.Logger
	stdin   io.WriteCloser
	log     LogSink
	version LogVersion
	wire    WireFormat
	mu      sync.Mutex // serializes stdin writes
}

// NewConn creates a connection using the task log's physical record version.
// log and sink must be non-nil; use DiscardLogSink{Version: version} when
// persistence is unnecessary.
func NewConn(ctx context.Context, log *slog.Logger, stdin io.WriteCloser, sink LogSink, wire WireFormat) Conn {
	if log == nil {
		panic("logger is required")
	}
	return &conn{ctx: ctx, logger: log, stdin: stdin, log: sink, version: sink.LogVersion(), wire: wire}
}

func (c *conn) SendPrompt(p Prompt) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire.WritePrompt(c.stdin, p, c.log)
}

func (c *conn) SendRaw(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return err
	}
	return AppendNativeRecord(c.log, c.version, data)
}

func (c *conn) SendCompact(instructions string) error {
	cc, ok := c.wire.(CompactCommand)
	if !ok {
		return errors.New("compact not supported by this backend")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cc.WriteCompact(c.stdin, instructions, c.log)
}

func (c *conn) ReadMessages(r io.Reader, msgCh chan<- ParsedMessage) error {
	return DefaultReadMessages(c.ctx, c.logger, r, func(m ParsedMessage) { msgCh <- m }, c.log, c.version, c.wire.ParseMessage)
}

func (c *conn) SendStop(ctx context.Context) {
	// Best-effort — the pipe may be broken or blocked (e.g. the SSH
	// process is gone). Returns when ctx is done if the write blocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.stdin.Write([]byte{0, '\n'})
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *conn) Close() error {
	return c.stdin.Close()
}

// Session manages a running agent process. It embeds Conn for wire I/O.
type Session struct {
	Conn

	cmd  *exec.Cmd
	log  *slog.Logger
	done chan struct{} // closed when readMessages goroutine exits
	err  error
}

// NewSession creates a Session from an already-started command. Messages read
// from stdout are parsed and forwarded to msgCh through the Conn's
// ReadMessages method.
//
// A background goroutine reads stdout until EOF, then waits for the process to
// exit. The done channel is closed when both are complete. Callers should use
// Done() to detect session end and Wait() to retrieve the error.
//
// Error priority: parse errors take precedence over wait errors, since a
// parse error indicates corrupted output while the process may still exit 0.
func NewSession(ctx context.Context, cmd *exec.Cmd, c Conn, stdout io.Reader, msgCh chan<- ParsedMessage, log *slog.Logger) *Session {
	if log == nil {
		panic("logger is required")
	}
	s := &Session{
		Conn: c,
		cmd:  cmd,
		log:  log,
		done: make(chan struct{}),
	}

	go func() {
		defer close(s.done)
		parseErr := s.ReadMessages(stdout, msgCh)
		waitErr := cmd.Wait()
		switch {
		case parseErr != nil:
			s.err = fmt.Errorf("parse: %w", parseErr)
			log.ErrorContext(ctx, "session parse error", "err", parseErr)
		case waitErr != nil:
			s.err = fmt.Errorf("agent exited: %w", waitErr)
			// Signal-based exits (SIGKILL, SIGTERM) are expected when
			// containers are purged. Log at Info, not Error.
			if isSignalExit(waitErr) {
				log.InfoContext(ctx, "session killed", "err", waitErr)
			} else {
				log.WarnContext(ctx, "session exit error", "err", waitErr)
			}
		default:
			log.InfoContext(ctx, "session done")
		}
	}()

	return s
}

// Stop sends the null-byte sentinel, closes stdin, and waits for the agent
// process to exit or the context to expire. Returns nil on clean exit, the
// process exit error on abnormal exit, or the context error on timeout.
//
// Closing stdin after the sentinel is critical: the attach_client's main
// thread is blocked on stdin.read1(). Without the close, attach_client never
// exits, SSH never exits, and cmd.Wait() never returns — deadlocking Stop.
//
// The sentinel must be written explicitly rather than inferred from stdin EOF
// in the attach client, because EOF also occurs on SSH drops and backend
// restarts where the container should keep running.
func (s *Session) Stop(ctx context.Context) error {
	s.log.Debug("session: sending stop sentinel")
	s.SendStop(ctx)
	// Close stdin so attach_client sees EOF and exits. The sentinel is
	// already in the pipe buffer; the OS delivers it before the EOF.
	_ = s.Close()
	s.log.Debug("session: waiting for process exit")
	select {
	case <-s.done:
		s.log.Debug("session: process exited", "err", s.err)
		return s.err
	case <-ctx.Done():
		s.log.Debug("session: context timeout while waiting for process exit", "err", ctx.Err())
		return ctx.Err()
	}
}

// Done returns a channel that is closed when the agent process exits.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// Wait blocks until the agent process exits.
func (s *Session) Wait() error {
	<-s.done
	return s.err
}

// LogRecordParser decodes versioned physical task-log records around one
// harness-native parser. A parser holds ordered log state and is not safe for
// concurrent use.
type LogRecordParser struct {
	version       LogVersion
	parseNative   func([]byte) ([]Message, error)
	contextWindow int
}

// ParsedRecord is a parser-owned semantic task-log record. Control reports
// whether the version-specific discriminator identifies a caic-owned control.
type ParsedRecord struct {
	Messages []ParsedMessage
	Control  bool
}

// RelayRecordReader reads one physical relay record at a time. Agent payloads
// are returned as their unchanged native JSON; caic controls are returned only
// as parsed controls and never presented as native handshake data. logW is the
// caller's explicit persistence policy: use io.Discard for unpersisted reads.
type RelayRecordReader struct {
	r      *bufio.Reader
	parser *LogRecordParser
	log    LogSink
	native []byte
}

// NewRelayRecordReader creates a version-aware physical relay reader. version
// must be the caller-validated task-log version. log must be non-nil; use
// DiscardLogSink{Version: version} when persistence is unnecessary.
func NewRelayRecordReader(r io.Reader, version LogVersion, log LogSink) (*RelayRecordReader, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	reader := &RelayRecordReader{r: br, log: log}
	parser, err := NewLogRecordParser(version, func(line []byte) ([]Message, error) {
		reader.native = append(reader.native[:0], line...)
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	reader.parser = parser
	return reader, nil
}

// Reader returns the buffered source positioned after records consumed by ReadRecord.
func (r *RelayRecordReader) Reader() *bufio.Reader {
	return r.r
}

// ReadRecord reads the next non-empty physical relay record. It persists the
// original encoded bytes once according to the reader's policy before strict
// parsing. Native is non-nil only for an agent payload; controls are separate.
func (r *RelayRecordReader) ReadRecord() (native []byte, controls []ParsedMessage, err error) {
	for {
		encoded, readErr := readNDJSONRecord(r.r)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, nil, readErr
			}
			return nil, nil, fmt.Errorf("read relay record: %w", readErr)
		}
		line := encoded[:len(encoded)-1]
		if len(line) == 0 {
			continue
		}
		r.native = r.native[:0]
		if r.parser.version == LogVersionV1 {
			if writeErr := r.log.AppendNative(encoded); writeErr != nil {
				return nil, nil, fmt.Errorf("write log: %w", writeErr)
			}
		}
		parsed, parseErr := r.parser.ParseRecord(line)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		if r.parser.version == LogVersionV2 {
			if writeErr := r.log.AppendNative(encoded); writeErr != nil {
				return nil, nil, fmt.Errorf("write log: %w", writeErr)
			}
		}
		if len(r.native) > 0 {
			return slices.Clone(r.native), nil, nil
		}
		if parsed.Control {
			return nil, parsed.Messages, nil
		}
	}
}

// NewLogRecordParser constructs a parser for an already-validated physical log
// version. parseNative is called synchronously and must parse and validate the
// supplied harness-native JSON before returning.
//
// The callback input may alias scanner-owned record memory. parseNative must not
// retain or use the input after it returns.
func NewLogRecordParser(version LogVersion, parseNative func([]byte) ([]Message, error)) (*LogRecordParser, error) {
	if err := version.Validate(); err != nil {
		return nil, err
	}
	if parseNative == nil {
		return nil, errors.New("native message parser is nil")
	}
	return &LogRecordParser{version: version, parseNative: parseNative}, nil
}

// ParseRecord decodes one physical task-log record according to the parser's
// exact version. Classification belongs to this parser so task-log consumers
// never duplicate or guess control vocabulary.
func (p *LogRecordParser) ParseRecord(line []byte) (ParsedRecord, error) {
	if p.version == LogVersionV2 {
		return parseV2Record(p, line)
	}
	return parseV1Record(p, line)
}

type logControlKind int

const (
	logControlMeta logControlKind = iota
	logControlDiffStat
	logControlExit
	logControlStrippedEnv
	logControlSession
	logControlLegacyInit
	logControlModelInfo
	logControlPR
	logControlResult
	logControlPendingUserAction
	logControlProvisioningLog
	logControlContextCleared
	logControlText
	logControlUserInput
)

var v1LogControlKinds = map[string]logControlKind{
	"caic_meta":                  logControlMeta,
	"caic_diff_stat":             logControlDiffStat,
	"caic_exit":                  logControlExit,
	"caic_stripped_env":          logControlStrippedEnv,
	"caic_session":               logControlSession,
	"caic_init":                  logControlLegacyInit,
	"caic_model_info":            logControlModelInfo,
	"caic_pr":                    logControlPR,
	"caic_result":                logControlResult,
	PendingUserActionMessageType: logControlPendingUserAction,
	"caic_log":                   logControlProvisioningLog,
}

var v2LogControlKinds = map[string]logControlKind{
	"caic_meta":           logControlMeta,
	"diff_stat":           logControlDiffStat,
	"exit":                logControlExit,
	"stripped_env":        logControlStrippedEnv,
	"session":             logControlSession,
	"model_info":          logControlModelInfo,
	"pr":                  logControlPR,
	"result":              logControlResult,
	"pending_user_action": logControlPendingUserAction,
	"log":                 logControlProvisioningLog,
	"context_cleared":     logControlContextCleared,
	"text":                logControlText,
	"user_input":          logControlUserInput,
}

type modelInfoLogRecord struct {
	ContextWindow int `json:"context_window"`
}

type legacyInitLogRecord struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Version   string `json:"version"`
}

func (p *LogRecordParser) controlKind(token string) (logControlKind, bool) {
	if p.version == LogVersionV1 {
		kind, ok := v1LogControlKinds[token]
		return kind, ok
	}
	kind, ok := v2LogControlKinds[token]
	return kind, ok
}

func (p *LogRecordParser) parseControl(kind logControlKind, token string, line []byte) ([]Message, error) {
	switch kind {
	case logControlMeta:
		var m MetaMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		if LogVersion(m.Version) != p.version {
			return nil, fmt.Errorf("decode %s: header version %d does not match parser version %d", token, m.Version, p.version)
		}
		return []Message{&m}, nil
	case logControlDiffStat:
		var m DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_diff_stat"
		return []Message{&m}, nil
	case logControlExit:
		var m ExitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_exit"
		return []Message{&m}, nil
	case logControlStrippedEnv:
		var m StrippedEnvMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_stripped_env"
		return []Message{&m}, nil
	case logControlSession:
		var m MetaSessionMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		return []Message{&InitMessage{SessionID: m.SessionID, Model: m.Model, Version: m.AgentVersion}}, nil
	case logControlLegacyInit:
		var m legacyInitLogRecord
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		return []Message{&InitMessage{SessionID: m.SessionID, Model: m.Model, Version: m.Version}}, nil
	case logControlModelInfo:
		var m modelInfoLogRecord
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		if m.ContextWindow > 0 {
			p.contextWindow = m.ContextWindow
		}
		return nil, nil
	case logControlPR:
		var m MetaPRMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_pr"
		return []Message{&m}, nil
	case logControlResult:
		var m MetaResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_result"
		return []Message{&m}, nil
	case logControlPendingUserAction:
		var m PendingUserActionMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = PendingUserActionMessageType
		return []Message{&m}, nil
	case logControlProvisioningLog:
		var m LogMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		m.MessageType = "caic_log"
		return []Message{&m}, nil
	case logControlContextCleared:
		return []Message{&SystemMessage{MessageType: "system", Subtype: "context_cleared"}}, nil
	case logControlText:
		var m TextMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		return []Message{&m}, nil
	case logControlUserInput:
		var m UserInputMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", token, err)
		}
		return []Message{&m}, nil
	default:
		return nil, fmt.Errorf("decode %s: unknown control kind %d", token, kind)
	}
}

func (p *LogRecordParser) parseAndApplyNative(line []byte) ([]Message, error) {
	msgs, err := p.parseNative(line)
	if err != nil {
		return nil, err
	}
	return p.applyMessageState(msgs)
}

func wrapParsedMessages(msgs []Message, producerTime time.Time) []ParsedMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]ParsedMessage, len(msgs))
	for i, msg := range msgs {
		out[i] = ParsedMessage{Message: msg, ProducerTime: producerTime}
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
		if m, ok := msg.(*UsageMessage); ok && m.ContextWindow == 0 && p.contextWindow > 0 {
			m.ContextWindow = p.contextWindow
		}
		out = append(out, msg)
	}
	return out, nil
}

func messageIsNil(msg Message) bool {
	if msg == nil {
		return true
	}
	value := reflect.ValueOf(msg)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// DefaultReadMessages reads physical relay records, persists each
// exactly once, and forwards the parser's original ParsedMessage wrappers.
func DefaultReadMessages(ctx context.Context, log *slog.Logger, r io.Reader, dispatch func(ParsedMessage), sink LogSink, version LogVersion, parseNative func([]byte) ([]Message, error)) error {
	if log == nil {
		return errors.New("logger is required")
	}
	parser, err := NewLogRecordParser(version, parseNative)
	if err != nil {
		return fmt.Errorf("construct relay record parser: %w", err)
	}
	reader := bufio.NewReaderSize(r, 1<<20)
	log.DebugContext(ctx, "reading agent stdout")
	var n int
	for {
		record, readErr := readNDJSONRecord(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read agent record: %w", readErr)
		}
		line := record[:len(record)-1]
		if len(line) == 0 {
			continue
		}
		n++
		parsed, err := parser.ParseRecord(line)
		if version == LogVersionV2 && err != nil {
			return fmt.Errorf("parse v2 relay record: %w", err)
		}
		if writeErr := sink.AppendNative(record); writeErr != nil {
			return fmt.Errorf("write log: %w", writeErr)
		}
		if err != nil {
			log.WarnContext(ctx, "unparseable message", "err", err, "line", string(line))
			dispatch(ParsedMessage{Message: &ParseErrorMessage{Err: err.Error(), Line: string(line)}})
			continue
		}
		for _, msg := range parsed.Messages {
			if n <= 3 {
				log.DebugContext(ctx, "parsed message", "n", n, "type", fmt.Sprintf("%T", msg.Message))
			}
			dispatch(msg)
		}
	}
	log.DebugContext(ctx, "read loop done", "n", n)
	return nil
}

// Relay paths inside the container.
const (
	RelayDir        = "/tmp/caic-relay"
	RelayScriptPath = RelayDir + "/relay.py"
	RelaySockPath   = RelayDir + "/relay.sock"
	RelayOutputPath = RelayDir + "/output.jsonl"
	RelayLogPath    = RelayDir + "/relay.log"
)

// RelayScript selects the embedded script for a validated log version.
func RelayScript(version LogVersion) ([]byte, error) {
	if err := version.Validate(); err != nil {
		return nil, err
	}
	if version == LogVersionV2 {
		return relay.ScriptV2, nil
	}
	return relay.Script, nil
}

// DeployRelay uploads the selected relay script into the runtime target. Idempotent.
func DeployRelay(ctx context.Context, target runtime.ConnectionTarget, version LogVersion) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, //nolint:gosec // target is not user-controlled
		"mkdir -p "+RelayDir+" && cat > "+RelayScriptPath)
	script, err := RelayScript(version)
	if err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy relay: %w: %s", err, out)
	}
	return nil
}

// WidgetPluginDir is the container path where the widget plugin is deployed.
const WidgetPluginDir = RelayDir + "/widget-plugin"

// DeployEmbeddedDir writes all files from an embed.FS to a target directory
// in the container via a single SSH + tar invocation. Idempotent.
func DeployEmbeddedDir(ctx context.Context, container string, fsys fs.FS, targetDir string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		if writeErr := tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(data)),
		}); writeErr != nil {
			return writeErr
		}
		_, writeErr := tw.Write(data)
		return writeErr
	}); err != nil {
		return fmt.Errorf("build tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	cmd := exec.CommandContext(ctx, "ssh", container, //nolint:gosec // container is not user-controlled
		"mkdir -p "+targetDir+" && tar xf - -C "+targetDir)
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy %s: %w: %s", targetDir, err, out)
	}
	return nil
}

// StopRelay sends the relay shutdown sentinel through a fresh attachment and
// waits for the attach client to exit. It terminates the persistent relay and
// its agent subprocess without treating an SSH disconnect as a shutdown.
func StopRelay(ctx context.Context, target runtime.ConnectionTarget) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	cmd := sshCmd(ctx, target.SSHHost, "python3", RelayScriptPath, "attach")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("relay shutdown stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relay shutdown attach: %w", err)
	}
	_, writeErr := stdin.Write([]byte{0, '\n'})
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	return errors.Join(writeErr, closeErr, waitErr)
}

// CleanRelayState removes the relay state directory in the container so that
// a subsequent StartRelay begins with a clean output.jsonl. Used by fork to
// prevent the source task's message history from leaking into the forked task.
func CleanRelayState(ctx context.Context, container string) error {
	cmd := exec.CommandContext(ctx, "ssh", container, "rm", "-rf", RelayDir) //nolint:gosec // container is not user-controlled
	return cmd.Run()
}

// HasRelayDir checks whether the caic relay directory exists in the container.
// Its presence proves caic deployed the relay at some point.
func HasRelayDir(ctx context.Context, container string) (bool, error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-d", RelayDir) //nolint:gosec // container is not user-controlled
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("test relay dir: %w", err)
	}
	return true, nil
}

// RelayStatus checks relay socket + PID liveness and returns diagnostic detail.
func RelayStatus(ctx context.Context, container string) (alive bool, detail string, err error) {
	pidPath := RelayDir + "/pid"
	check := fmt.Sprintf(
		`sock=0; [ -S %[1]s ] && sock=1; `+
			`pid=""; [ -f %[2]s ] && pid=$(cat %[2]s 2>/dev/null); `+
			`killok=0; if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then killok=1; fi; `+
			`echo "sock=$sock pid=$pid kill=$killok"; `+
			`[ "$sock" -eq 1 ] && [ "$killok" -eq 1 ]`,
		RelaySockPath, pidPath)
	cmd := sshCmd(ctx, container, "sh", "-c", check)
	out, err := cmd.CombinedOutput()
	detail = strings.TrimSpace(string(out))
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() != 0 {
			return false, detail, nil
		}
		return false, detail, fmt.Errorf("test relay: %w", err)
	}
	return true, detail, nil
}

// IsRelayRunning checks whether the relay socket exists in the container.
func IsRelayRunning(ctx context.Context, container string) (bool, error) {
	alive, _, err := RelayStatus(ctx, container)
	return alive, err
}

// ReadRelayLog reads the last maxBytes of the relay daemon's log file from the
// container. Returns empty string on any error (missing file, SSH failure).
func ReadRelayLog(ctx context.Context, container string, maxBytes int) string {
	// Use tail -c to cap the output; the log can be large after long sessions.
	cmd := sshCmd(ctx, container, "tail", "-c", strconv.Itoa(maxBytes), RelayLogPath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ReadPlan reads a plan file from the container by invoking relay.py read-plan
// over SSH. If planFile is non-empty, that specific file is read; otherwise the
// most recently modified .md file in ~/.claude/plans/ is used.
func ReadPlan(ctx context.Context, container, planFile string) (string, error) {
	if container == "" {
		return "", errors.New("read plan: container is required")
	}
	args := []string{container, "python3", RelayScriptPath, "read-plan"}
	if planFile != "" {
		args = append(args, planFile)
	}
	cmd := sshCmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read plan: %w", err)
	}
	return string(out), nil
}

// SlogWriter is an io.Writer that logs each line via slog.Warn. It is used
// as cmd.Stderr for SSH relay subprocesses across all backends.
type SlogWriter struct {
	Context   context.Context
	Logger    *slog.Logger
	Prefix    string
	Container string

	buf []byte
}

func (w *SlogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.Logger.WarnContext(w.Context, "stderr", "src", w.Prefix, "ctr", w.Container, "line", line)
		}
	}
	return len(p), nil
}

// RelayProcess holds the started relay SSH command and its I/O pipes.
type RelayProcess struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.Reader
}

// PrepareRelay deploys the relay script and starts the SSH serve-attach
// process. The caller creates a Conn and Session from the returned process.
func PrepareRelay(ctx context.Context, opts *Options, agentArgs []string) (*RelayProcess, error) {
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	tStart := time.Now()
	version := opts.Log.LogVersion()
	if err := version.Validate(); err != nil {
		return nil, fmt.Errorf("relay log version: %w", err)
	}
	if err := DeployRelay(ctx, opts.Target, version); err != nil {
		return nil, err
	}
	opts.Logger.DebugContext(ctx, "startup", "phase", "deploy_relay", "target", sshHost, "dur", time.Since(tStart))

	sshArgs := make([]string, 0, 8+2*len(opts.StripEnv)+len(agentArgs))
	sshArgs = append(sshArgs, sshHost, "python3", RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin")
	for _, key := range opts.StripEnv {
		sshArgs = append(sshArgs, "--strip-env", key)
	}
	sshArgs = append(sshArgs, "--")
	sshArgs = append(sshArgs, agentArgs...)

	opts.Logger.DebugContext(ctx, "relay", "msg", "launch", "target", sshHost, "args", agentArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &SlogWriter{Context: ctx, Logger: opts.Logger, Prefix: "relay serve-attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}
	opts.Logger.InfoContext(ctx, "startup", "phase", "relay_started", "target", sshHost, "dur", time.Since(tStart))
	return &RelayProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout}, nil
}

// StartRelay is a convenience that calls PrepareRelay, creates a default Conn,
// and sends the initial prompt.
func StartRelay(ctx context.Context, opts *Options, agentArgs []string, wire WireFormat) (*Session, error) {
	rp, err := PrepareRelay(ctx, opts, agentArgs)
	if err != nil {
		return nil, err
	}
	return StartSession(ctx, rp, NewConn(ctx, opts.Logger, rp.Stdin, opts.Log, wire), opts)
}

// StartSession creates a Session from a RelayProcess and Conn, sends the
// initial prompt if present, and returns the session.
func StartSession(ctx context.Context, rp *RelayProcess, c Conn, opts *Options) (*Session, error) {
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	log := opts.Logger.With("target", sshHost)
	s := NewSession(ctx, rp.Cmd, c, rp.Stdout, opts.MsgCh, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.SendPrompt(opts.InitialPrompt); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// sshTimeoutArgs are the default SSH options for relay operations.
var sshTimeoutArgs = []string{"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2"}

// sshCmd creates an exec.Cmd with standard timeout args prepended.
func sshCmd(ctx context.Context, extraArgs ...string) *exec.Cmd {
	args := make([]string, 0, len(sshTimeoutArgs)+len(extraArgs))
	args = append(args, sshTimeoutArgs...)
	args = append(args, extraArgs...)
	return exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // args are not user-controlled.
}

// RelayOutputSize returns the byte size of the relay output.jsonl in the
// container. This is fast (single stat call) and avoids transferring the file.
func RelayOutputSize(ctx context.Context, container string) (int64, error) {
	cmd := sshCmd(ctx, container, "stat", "-c", "%s", RelayOutputPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("stat relay output: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse relay size %q: %w", string(out), err)
	}
	return n, nil
}

// ReadRelayTail reads only the tail of the relay output.jsonl from the
// container and returns the parsed messages plus the total file size (for
// RelayOffset). It streams the SSH output directly, so memory usage is O(1)
// and no multi-GB transfer occurs during runtime import.
func ReadRelayTail(ctx context.Context, container string, parser *LogRecordParser, maxBytes int64) (msgs []ParsedMessage, size int64, err error) {
	size, err = RelayOutputSize(ctx, container)
	if err != nil {
		return nil, 0, err
	}
	cmd, start := relaySnapshotCommand(ctx, container, maxBytes, size)
	skipFirst, err := relayTailNeedsLeadingSkip(ctx, container, start)
	if err != nil {
		return nil, start, err
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, start, fmt.Errorf("relay stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, start, fmt.Errorf("start relay read: %w", err)
	}
	msgs, offset, readErr := readRelayTailRecords(pipe, parser, start, skipFirst, container)
	if readErr != nil {
		_ = cmd.Wait()
		return msgs, offset, readErr
	}
	if err := cmd.Wait(); err != nil {
		return msgs, offset, fmt.Errorf("relay read: %w", err)
	}
	return msgs, offset, nil
}

// readRelayTailRecords parses one stat-bounded relay snapshot and returns the
// exact physical offset consumed, including a skipped partial tail record.
func readRelayTailRecords(r io.Reader, parser *LogRecordParser, start int64, skipFirst bool, src string) (msgs []ParsedMessage, offset int64, err error) {
	reader := bufio.NewReaderSize(r, 1<<20)
	offset = start
	for {
		record, readErr := readNDJSONRecord(reader)
		offset += int64(len(record))
		if errors.Is(readErr, io.EOF) {
			return msgs, offset, nil
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return msgs, offset - int64(len(record)), nil
		}
		if readErr != nil {
			return msgs, offset, fmt.Errorf("read relay record: %w", readErr)
		}
		line := record[:len(record)-1]
		if skipFirst {
			skipFirst = false
			continue
		}
		if len(line) == 0 {
			continue
		}
		parsed, parseErr := parser.ParseRecord(line)
		if parseErr != nil {
			if parser.version == LogVersionV2 || parsed.Control {
				return msgs, offset, fmt.Errorf("parse relay record: %w", parseErr)
			}
			slog.Warn("relay", "msg", "skipping unparseable output line", "src", src, "err", parseErr)
			continue
		}
		msgs = append(msgs, parsed.Messages...)
	}
}

func tailNeedsLeadingSkip(start int64, previous byte) bool {
	return start > 0 && previous != '\n'
}

// relayTailNeedsLeadingSkip reports whether a tail begins mid-record.
func relayTailNeedsLeadingSkip(ctx context.Context, container string, start int64) (bool, error) {
	if start == 0 {
		return false, nil
	}
	command := fmt.Sprintf("dd if=%s bs=1 skip=%d count=1 status=none", RelayOutputPath, start-1)
	out, err := sshCmd(ctx, container, command).Output()
	if err != nil {
		return false, fmt.Errorf("read relay tail boundary: %w", err)
	}
	if len(out) != 1 {
		return false, fmt.Errorf("read relay tail boundary: got %d bytes", len(out))
	}
	return tailNeedsLeadingSkip(start, out[0]), nil
}

// relaySnapshotCommand bounds a relay read to the size observed by stat. The
// returned start is the physical output-file offset of the transferred bytes.
func relaySnapshotCommand(ctx context.Context, container string, tailBytes, size int64) (cmd *exec.Cmd, start int64) {
	if tailBytes > 0 && size > tailBytes {
		start := size - tailBytes
		command := fmt.Sprintf("head -c %d %s | tail -c %d", size, RelayOutputPath, tailBytes)
		return sshCmd(ctx, container, command), start
	}
	command := fmt.Sprintf("head -c %d %s", size, RelayOutputPath)
	return sshCmd(ctx, container, command), 0
}

// StreamRelay streams NDJSON messages from the relay output.jsonl in the
// container over SSH, yielding each in order. When tailBytes > 0 and the file
// is larger, only the last tailBytes are transferred (via tail -c) and the
// partial first line is skipped; otherwise the whole file is streamed (cat).
//
// The SSH stdout is read incrementally, so memory usage is O(1) regardless of
// file size. If the consumer stops early the ssh process is killed and reaped,
// so no process leaks per abandoned reader.
func StreamRelay(ctx context.Context, container string, parser *LogRecordParser, tailBytes, size int64) iter.Seq2[ParsedMessage, error] {
	return func(yield func(ParsedMessage, error) bool) {
		cmd, start := relaySnapshotCommand(ctx, container, tailBytes, size)
		tailed, boundaryErr := relayTailNeedsLeadingSkip(ctx, container, start)
		if boundaryErr != nil {
			yield(ParsedMessage{}, boundaryErr)
			return
		}
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			yield(ParsedMessage{}, fmt.Errorf("relay stdout pipe: %w", err))
			return
		}
		if err := cmd.Start(); err != nil {
			yield(ParsedMessage{}, fmt.Errorf("start relay read: %w", err))
			return
		}
		broke := false
		for m, e := range yieldMessages(pipe, parser, tailed, container) {
			if !yield(m, e) {
				broke = true
				break
			}
		}
		if broke {
			// Consumer stopped early: kill the process so Wait returns and the
			// ssh child is reaped instead of lingering.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return
		}
		if err := cmd.Wait(); err != nil {
			yield(ParsedMessage{}, fmt.Errorf("relay read: %w", err))
		}
	}
}

const maxNDJSONRecordLen = 32 << 20

// readNDJSONRecord reads one LF-terminated physical record without inventing a
// delimiter. It retains the prior 32 MiB maximum line limit.
func readNDJSONRecord(r *bufio.Reader) ([]byte, error) {
	var record []byte
	for {
		fragment, err := r.ReadSlice('\n')
		record = append(record, fragment...)
		if len(record) > maxNDJSONRecordLen {
			return record, fmt.Errorf("NDJSON record exceeds %d bytes", maxNDJSONRecordLen)
		}
		switch {
		case err == nil:
			return record, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(record) == 0 {
				return nil, io.EOF
			}
			return record, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// yieldMessages parses NDJSON from r, yielding each parsed message in order.
//
// When skipFirst is set the first physical record is dropped — it is a partial
// record left by tailing or seeking into the middle of a file. Unparseable
// lines are logged (tagged with src) and skipped. A scanner read error is
// reported as a terminal (nil, err) pair after the last good message.
//
// The reader is consumed lazily one line at a time, so memory usage is O(1)
// regardless of total size. StreamRelay owns the live SSH relay-output stream;
// this helper only decodes its NDJSON lines.
func yieldMessages(r io.Reader, parser *LogRecordParser, skipFirst bool, src string) iter.Seq2[ParsedMessage, error] {
	return func(yield func(ParsedMessage, error) bool) {
		reader := bufio.NewReaderSize(r, 1<<20)
		first := skipFirst
		for {
			encoded, readErr := readNDJSONRecord(reader)
			if errors.Is(readErr, io.EOF) {
				return
			}
			if readErr != nil {
				yield(ParsedMessage{}, readErr)
				return
			}
			line := encoded[:len(encoded)-1]
			if first {
				first = false
				continue
			}
			if len(line) == 0 {
				continue
			}
			record, parseErr := parser.ParseRecord(line)
			if parseErr != nil {
				if parser.version == LogVersionV2 || record.Control {
					yield(ParsedMessage{}, fmt.Errorf("parse relay record: %w", parseErr))
					return
				}
				slog.Warn("relay", "msg", "skipping unparseable output line", "src", src, "err", parseErr)
				continue
			}
			for _, m := range record.Messages {
				if !yield(m, nil) {
					return
				}
			}
		}
	}
}

// AttachRelaySession connects to an already-running relay in the container
// and returns a new Session. It waits briefly for the attach process to
// confirm connectivity; if the process exits immediately (e.g. relay socket
// is stale), an error is returned so the caller can fall back to --resume.
// Backends may pass wrap to intercept the default Conn before message reading
// starts.
func AttachRelaySession(ctx context.Context, opts *Options, wire WireFormat, wrap func(Conn) (Conn, error)) (*Session, error) {
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	sshArgs := []string{
		sshHost, "python3", RelayScriptPath, "attach",
		"--offset", strconv.FormatInt(opts.RelayOffset, 10),
	}
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := opts.Log.LogVersion().Validate(); err != nil {
		return nil, fmt.Errorf("relay log version: %w", err)
	}
	c := NewConn(ctx, opts.Logger, stdin, opts.Log, wire)
	if wrap != nil {
		c, err = wrap(c)
		if err != nil {
			return nil, err
		}
	}
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	cmd.Stderr = &SlogWriter{Context: ctx, Logger: opts.Logger, Prefix: "relay attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	log := opts.Logger.With("target", sshHost)
	return NewSession(ctx, cmd, c, stdout, opts.MsgCh, log), nil
}

// PlainTextWritePrompt writes a user prompt as a plain text line on stdin
// and logs it as NDJSON.
func PlainTextWritePrompt(w io.Writer, p Prompt, log LogSink) error {
	data := []byte(p.Text + "\n")
	if _, err := w.Write(data); err != nil {
		return err
	}
	return log.AppendMessage(&UserInputMessage{Text: p.Text})
}

// isSignalExit reports whether err indicates the process was killed by a
// signal (e.g. SIGKILL from container purge).
func isSignalExit(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	// On Unix, ExitCode() returns -1 when the process was killed by a signal.
	// ProcessState.Sys() returns syscall.WaitStatus with signal details.
	if exitErr.ExitCode() == -1 {
		return true
	}
	// Also check for specific signals via os.ProcessState.
	if ps := exitErr.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(interface{ Signal() os.Signal }); ok {
			return ws.Signal() != nil
		}
	}
	return false
}
