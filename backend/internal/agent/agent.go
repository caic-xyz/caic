// Package agent defines shared types and infrastructure for coding agent
// backends. Backend implementations live in sub-packages (e.g. agent/claudecode).
//
// # Message dispatch
//
// Conn.ReadMessages reads agent stdout and forwards parsed messages to
// Options.MsgCh. The task runner drains this channel in a separate
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
//	Server calls Runner.Cleanup → Session.Stop writes \x00\n then closes
//	stdin → attach_client forwards sentinel through Unix socket, sees stdin
//	EOF, exits → _client_reader sets shutdown_event → _shutdown_watchdog
//	closes proc.stdin, sends SIGINT, escalates to SIGTERM/SIGKILL →
//	reader_thread sees stdout EOF → server kills container.
//
// Flow 2 — Backend restarts (upgrade, crash):
//
//	SSH connections are severed → attach_client sees stdin EOF and disconnects
//	(no \x00 sent) → relay daemon + agent keep running → on restart, server
//	discovers the container via adoptOne(), reads output.jsonl to restore
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
	"path/filepath"
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
	Target          runtime.ConnectionTarget
	Dir             string // Working directory inside the runtime.
	Model           string // Model alias ("opus", "sonnet", "haiku") or full ID. Empty = default.
	Effort          string // Thinking effort (e.g. "low", "medium", "high", "max"). Empty = default.
	InitialPrompt   Prompt // Initial prompt; never mutated after creation.
	ResumeSessionID string
	RelayOffset     int64          // Byte offset into relay output.jsonl for AttachRelay.
	MsgCh           chan<- Message // Receives parsed messages from the agent.
	LogW            io.Writer      // Raw wire-format log; use io.Discard if unused.
	StripEnv        []string       // Env var names for relay to strip from subprocess and emit as caic_stripped_env.
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair WritePrompt and ParseMessage
// for the same protocol.
type WireFormat interface {
	// WritePrompt writes a user prompt to the agent's stdin in the
	// backend's wire format. logW receives a copy.
	WritePrompt(w io.Writer, p Prompt, logW io.Writer) error

	// ParseMessage decodes a single NDJSON line into one or more typed
	// Messages. A single wire line may produce multiple semantic messages.
	ParseMessage(line []byte) ([]Message, error)
}

// CompactCommand is an optional interface for WireFormat implementations that
// support context compaction. The server checks for this capability to
// conditionally enable the compact button in the UI.
type CompactCommand interface {
	WriteCompact(w io.Writer, instructions string, logW io.Writer) error
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
	ReadMessages(r io.Reader, msgCh chan<- Message) error
	// SendStop sends the null-byte sentinel to trigger graceful agent shutdown.
	// Best-effort: returns when ctx is done if the write blocks.
	SendStop(ctx context.Context)
	// Close closes the stdin pipe.
	Close() error
}

// conn is the default Conn implementation.
type conn struct {
	stdin io.WriteCloser
	logW  io.Writer
	wire  WireFormat
	mu    sync.Mutex // serializes stdin writes
}

// NewConn creates a Conn from an stdin pipe, log writer, and wire format.
func NewConn(stdin io.WriteCloser, logW io.Writer, wire WireFormat) Conn {
	return &conn{stdin: stdin, logW: logW, wire: wire}
}

func (c *conn) SendPrompt(p Prompt) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire.WritePrompt(c.stdin, p, c.logW)
}

func (c *conn) SendRaw(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return err
	}
	_, err := c.logW.Write(data)
	return err
}

func (c *conn) SendCompact(instructions string) error {
	cc, ok := c.wire.(CompactCommand)
	if !ok {
		return errors.New("compact not supported by this backend")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cc.WriteCompact(c.stdin, instructions, c.logW)
}

func (c *conn) ReadMessages(r io.Reader, msgCh chan<- Message) error {
	return DefaultReadMessages(r, func(m Message) { msgCh <- m }, c.logW, c.wire.ParseMessage)
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
func NewSession(cmd *exec.Cmd, c Conn, stdout io.Reader, msgCh chan<- Message, log *slog.Logger) *Session {
	if log == nil {
		log = slog.Default()
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
			log.Error("session parse error", "err", parseErr)
		case waitErr != nil:
			s.err = fmt.Errorf("agent exited: %w", waitErr)
			// Signal-based exits (SIGKILL, SIGTERM) are expected when
			// containers are purged. Log at Info, not Error.
			if isSignalExit(waitErr) {
				log.Info("session killed", "err", waitErr)
			} else {
				log.Warn("session exit error", "err", waitErr)
			}
		default:
			log.Info("session done")
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

// DefaultReadMessages is the standard message read loop. It reads NDJSON lines
// from r, parses them via parseFn, writes each raw line to logW, and forwards
// parsed messages via dispatch.
//
// Conn wrappers that override ReadMessages can call this for delegation with a
// custom dispatch function.
func DefaultReadMessages(r io.Reader, dispatch func(Message), logW io.Writer, parseFn func([]byte) ([]Message, error)) error {
	scanner := bufio.NewScanner(r)
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	slog.Debug("reading agent stdout")
	var n int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		n++
		record := append(append([]byte(nil), line...), '\n')
		if _, err := logW.Write(record); err != nil {
			return fmt.Errorf("write log: %w", err)
		}
		msgs, err := parseFn(line)
		if err != nil {
			slog.Warn("unparseable message", "err", err, "line", string(line))
			dispatch(&ParseErrorMessage{Err: err.Error(), Line: string(line)})
			continue
		}
		for _, msg := range msgs {
			if n <= 3 {
				slog.Debug("parsed message", "n", n, "type", fmt.Sprintf("%T", msg))
			}
			dispatch(msg)
		}
	}
	slog.Debug("read loop done", "n", n, "err", scanner.Err())
	return scanner.Err()
}

// Relay paths inside the container.
const (
	RelayDir        = "/tmp/caic-relay"
	RelayScriptPath = RelayDir + "/relay.py"
	RelaySockPath   = RelayDir + "/relay.sock"
	RelayOutputPath = RelayDir + "/output.jsonl"
	RelayLogPath    = RelayDir + "/relay.log"
)

// DeployRelay uploads the relay script into the runtime target. Idempotent.
func DeployRelay(ctx context.Context, target runtime.ConnectionTarget) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, //nolint:gosec // target is not user-controlled
		"mkdir -p "+RelayDir+" && cat > "+RelayScriptPath)
	cmd.Stdin = bytes.NewReader(relay.Script)
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
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
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
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 0 {
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
	Prefix    string
	Container string
	buf       []byte
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
			slog.Warn("stderr", "src", w.Prefix, "ctr", w.Container, "line", line)
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
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	tStart := time.Now()
	if err := DeployRelay(ctx, opts.Target); err != nil {
		return nil, err
	}
	slog.Debug("startup", "phase", "deploy_relay", "target", sshHost, "dur", time.Since(tStart))

	sshArgs := make([]string, 0, 7+2*len(opts.StripEnv)+len(agentArgs))
	sshArgs = append(sshArgs, sshHost, "python3", RelayScriptPath, "serve-attach", "--dir", opts.Dir)
	for _, key := range opts.StripEnv {
		sshArgs = append(sshArgs, "--strip-env", key)
	}
	sshArgs = append(sshArgs, "--")
	sshArgs = append(sshArgs, agentArgs...)

	slog.Debug("relay", "msg", "launch", "target", sshHost, "args", agentArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &SlogWriter{Prefix: "relay serve-attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}
	slog.Info("startup", "phase", "relay_started", "target", sshHost, "dur", time.Since(tStart))
	return &RelayProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout}, nil
}

// StartRelay is a convenience that calls PrepareRelay, creates a default Conn,
// and sends the initial prompt.
func StartRelay(ctx context.Context, opts *Options, agentArgs []string, wire WireFormat) (*Session, error) {
	rp, err := PrepareRelay(ctx, opts, agentArgs)
	if err != nil {
		return nil, err
	}
	return StartSession(rp, NewConn(rp.Stdin, opts.LogW, wire), opts)
}

// StartSession creates a Session from a RelayProcess and Conn, sends the
// initial prompt if present, and returns the session.
func StartSession(rp *RelayProcess, c Conn, opts *Options) (*Session, error) {
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	log := slog.With("target", sshHost)
	s := NewSession(rp.Cmd, c, rp.Stdout, opts.MsgCh, log)
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
// and no multi-GB transfer occurs during adoption.
func ReadRelayTail(ctx context.Context, container string, parseFn func([]byte) ([]Message, error), maxBytes int64) (msgs []Message, size int64, err error) {
	// Get total file size — instant stat call.
	size, err = RelayOutputSize(ctx, container)
	if err != nil {
		return nil, 0, err
	}
	for m, e := range StreamRelay(ctx, container, parseFn, maxBytes, size) {
		if e != nil {
			return msgs, size, e
		}
		msgs = append(msgs, m)
	}
	return msgs, size, nil
}

// StreamRelay streams NDJSON messages from the relay output.jsonl in the
// container over SSH, yielding each in order. When tailBytes > 0 and the file
// is larger, only the last tailBytes are transferred (via tail -c) and the
// partial first line is skipped; otherwise the whole file is streamed (cat).
//
// The SSH stdout is read incrementally, so memory usage is O(1) regardless of
// file size. If the consumer stops early the ssh process is killed and reaped,
// so no process leaks per abandoned reader.
func StreamRelay(ctx context.Context, container string, parseFn func([]byte) ([]Message, error), tailBytes, size int64) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		tailed := tailBytes > 0 && size > tailBytes
		var cmd *exec.Cmd
		if tailed {
			cmd = sshCmd(ctx, container, "tail", "-c", strconv.FormatInt(tailBytes, 10), RelayOutputPath)
		} else {
			cmd = sshCmd(ctx, container, "cat", RelayOutputPath)
		}
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			yield(nil, fmt.Errorf("relay stdout pipe: %w", err))
			return
		}
		if err := cmd.Start(); err != nil {
			yield(nil, fmt.Errorf("start relay read: %w", err))
			return
		}
		broke := false
		for m, e := range yieldMessages(pipe, parseFn, tailed, container) {
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
			yield(nil, fmt.Errorf("relay read: %w", err))
		}
	}
}

// ndjsonScanner returns a bufio.Scanner configured for relay and log NDJSON.
// Lines can be large: user input with base64 images produces multi-MB records,
// so the buffer is capped at 32 MiB.
func ndjsonScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1<<20), 32<<20)
	return s
}

// yieldMessages parses NDJSON from r, yielding each parsed message in order.
//
// When skipFirst is set the first non-empty line is dropped — it is a partial
// record left by tailing or seeking into the middle of a file. Unparseable
// lines are logged (tagged with src) and skipped. A scanner read error is
// reported as a terminal (nil, err) pair after the last good message.
//
// The reader is consumed lazily one line at a time, so memory usage is O(1)
// regardless of total size. This is the shared core behind StreamLogFile
// (disk) and StreamRelay (SSH).
func yieldMessages(r io.Reader, parseFn func([]byte) ([]Message, error), skipFirst bool, src string) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		scanner := ndjsonScanner(r)
		first := skipFirst
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if first {
				first = false
				continue
			}
			parsed, parseErr := parseFn(line)
			if parseErr != nil {
				slog.Warn("relay", "msg", "skipping unparseable output line", "src", src, "err", parseErr)
				continue
			}
			for _, m := range parsed {
				if !yield(m, nil) {
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, err)
		}
	}
}

// StreamLogFile streams NDJSON messages from a local file, yielding each in
// order. When offset > 0 the file is seeked there first and the partial first
// line is skipped, so only the tail of a large log is replayed. Memory usage
// is O(1) regardless of file size.
func StreamLogFile(path string, parseFn func([]byte) ([]Message, error), offset int64) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		f, err := os.Open(filepath.Clean(path))
		if err != nil {
			yield(nil, fmt.Errorf("open %s: %w", path, err))
			return
		}
		defer func() { _ = f.Close() }()
		if offset > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				yield(nil, fmt.Errorf("seek %s to %d: %w", path, offset, err))
				return
			}
		}
		for m, e := range yieldMessages(f, parseFn, offset > 0, path) {
			if !yield(m, e) {
				return
			}
		}
	}
}

// AttachRelaySession connects to an already-running relay in the container
// and returns a new Session. It waits briefly for the attach process to
// confirm connectivity; if the process exits immediately (e.g. relay socket
// is stale), an error is returned so the caller can fall back to --resume.
func AttachRelaySession(ctx context.Context, opts *Options, wire WireFormat) (*Session, error) {
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
	cmd.Stderr = &SlogWriter{Prefix: "relay attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	log := slog.With("target", sshHost)
	return NewSession(cmd, NewConn(stdin, opts.LogW, wire), stdout, opts.MsgCh, log), nil
}

// PlainTextWritePrompt writes a user prompt as a plain text line on stdin
// and logs it as NDJSON. Used by backends whose agent reads plain text
// (gemini, kilo).
func PlainTextWritePrompt(w io.Writer, p Prompt, logW io.Writer) error {
	data := []byte(p.Text + "\n")
	if _, err := w.Write(data); err != nil {
		return err
	}
	entry, err := json.Marshal(map[string]string{
		"type":    "user_input",
		"content": p.Text,
	})
	if err != nil {
		return err
	}
	_, err = logW.Write(append(entry, '\n'))
	return err
}

// isSignalExit reports whether err indicates the process was killed by a
// signal (e.g. SIGKILL from container purge).
func isSignalExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
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
