// Package agent defines shared types and infrastructure for coding agent
// backends. Backend implementations live in sub-packages (e.g. agent/claude).
//
// # Relay shutdown protocol
//
// Each agent runs inside a container behind a relay daemon (relay.py) that
// survives SSH disconnects. Graceful shutdown uses a null-byte (\x00)
// sentinel written to stdin:
//
// Flow 1 — One task is purged (user action or container death):
//
//	Server calls Runner.Cleanup → Session.Close writes \x00 → attach_client
//	forwards it through the Unix socket → relay daemon closes proc.stdin →
//	agent exits → server kills the container.
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
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/relay"
)

// ImageData carries a single base64-encoded image for multi-modal input.
type ImageData struct {
	MediaType string // e.g. "image/png", "image/jpeg"
	Data      string // base64-encoded
}

// Prompt bundles user text with optional images for a single interaction.
type Prompt struct {
	Text   string      `json:"text"`
	Images []ImageData `json:"images,omitempty"`
}

// Options configures an agent session launch.
type Options struct {
	Container       string
	Dir             string // Working directory inside the container.
	Model           string // Model alias ("opus", "sonnet", "haiku") or full ID. Empty = default.
	InitialPrompt   Prompt // Initial prompt; never mutated after creation.
	ResumeSessionID string
	RelayOffset     int64 // Byte offset into relay output.jsonl for AttachRelay.
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair WritePrompt and ParseMessage
// for the same protocol.
type WireFormat interface {
	// WritePrompt writes a user prompt to the agent's stdin in the
	// backend's wire format. logW receives a copy (may be nil).
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

// Session manages a running agent process.
type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	logW      io.Writer
	wire      WireFormat
	log       *slog.Logger
	mu        sync.Mutex // serializes stdin writes
	closeOnce sync.Once
	done      chan struct{} // closed when readMessages goroutine exits
	result    *ResultMessage
	err       error
}

// NewSession creates a Session from an already-started command. Messages read
// from stdout are parsed and sent to msgCh. logW receives raw NDJSON lines
// (may be nil). wire defines the backend's wire protocol.
//
// A background goroutine reads stdout until EOF, then waits for the process to
// exit. The done channel is closed when both are complete. Callers should use
// Done() to detect session end and Wait() to retrieve the result.
//
// Error priority: parse errors take precedence over wait errors, since a
// parse error indicates corrupted output while the process may still exit 0.
// If neither parse nor wait errors occur but no ResultMessage was seen, the
// session reports "agent exited without a result message".
func NewSession(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, msgCh chan<- Message, logW io.Writer, wire WireFormat, log *slog.Logger) *Session {
	if log == nil {
		log = slog.Default()
	}
	s := &Session{
		cmd:   cmd,
		stdin: stdin,
		logW:  logW,
		wire:  wire,
		log:   log,
		done:  make(chan struct{}),
	}

	go func() {
		defer close(s.done)
		result, parseErr := readMessages(stdout, msgCh, logW, wire.ParseMessage)
		waitErr := cmd.Wait()
		// Store the result and first non-nil error.
		s.result = result
		switch {
		case result != nil:
			log.Info("session done", "result", result.Subtype)
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
			s.err = errors.New("agent exited without a result message")
			log.Error("session no result")
		}
	}()

	return s
}

// Send writes a user message to the agent's stdin. It is safe for concurrent
// use. The first call typically provides the initial task prompt.
func (s *Session) Send(p Prompt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire.WritePrompt(s.stdin, p, s.logW)
}

// SendRaw writes pre-encoded NDJSON bytes to the agent's stdin. It is safe for
// concurrent use. Use this for control messages that bypass WritePrompt.
func (s *Session) SendRaw(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.stdin.Write(data); err != nil {
		return err
	}
	if s.logW != nil {
		_, _ = s.logW.Write(data)
	}
	return nil
}

// SendCompact sends a compact command to the agent. Returns an error if the
// backend's wire format does not implement CompactCommand.
func (s *Session) SendCompact(instructions string) error {
	cc, ok := s.wire.(CompactCommand)
	if !ok {
		return errors.New("compact not supported by this backend")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cc.WriteCompact(s.stdin, instructions, s.logW)
}

// Close sends the null-byte sentinel to the relay daemon (triggering graceful
// subprocess shutdown) and then closes stdin. Idempotent.
//
// The sentinel must be written explicitly here rather than inferred from stdin
// EOF in the attach client, because EOF also occurs on SSH drops and backend
// restarts where the container should keep running.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		// Best-effort write with timeout — the pipe may already be broken
		// or blocked (e.g. the SSH process is gone).
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = s.stdin.Write([]byte{0})
		}()
		t := time.NewTimer(2 * time.Second)
		select {
		case <-done:
			t.Stop()
		case <-t.C:
		}
		_ = s.stdin.Close()
	})
}

// Done returns a channel that is closed when the agent process exits.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// Wait blocks until the agent process exits and returns the result.
func (s *Session) Wait() (*ResultMessage, error) {
	<-s.done
	return s.result, s.err
}

// readMessages reads NDJSON lines from r, dispatches to msgCh, and returns
// the terminal ResultMessage. If logW is non-nil, each raw line is written to it.
func readMessages(r io.Reader, msgCh chan<- Message, logW io.Writer, parseFn func([]byte) ([]Message, error)) (*ResultMessage, error) {
	scanner := bufio.NewScanner(r)
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	slog.Debug("reading agent stdout")
	var n int
	var result *ResultMessage
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		n++
		if logW != nil {
			_, _ = logW.Write(line)
			_, _ = logW.Write([]byte{'\n'})
		}
		msgs, err := parseFn(line)
		if err != nil {
			slog.Warn("unparseable message", "err", err, "line", string(line))
			if msgCh != nil {
				msgCh <- &ParseErrorMessage{Err: err.Error(), Line: string(line)}
			}
			continue
		}
		for _, msg := range msgs {
			if n <= 3 {
				slog.Debug("parsed message", "n", n, "type", fmt.Sprintf("%T", msg))
			}
			if msgCh != nil {
				msgCh <- msg
			}
			if rm, ok := msg.(*ResultMessage); ok {
				result = rm
			}
		}
	}
	slog.Debug("read loop done", "n", n, "result", result != nil, "err", scanner.Err())
	return result, scanner.Err()
}

// Relay paths inside the container.
const (
	RelayDir        = "/tmp/caic-relay"
	RelayScriptPath = RelayDir + "/relay.py"
	RelaySockPath   = RelayDir + "/relay.sock"
	RelayOutputPath = RelayDir + "/output.jsonl"
	RelayLogPath    = RelayDir + "/relay.log"
)

// DeployRelay uploads the relay script into the container. Idempotent.
func DeployRelay(ctx context.Context, container string) error {
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", container, //nolint:gosec // container is not user-controlled
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
	cmd := exec.CommandContext(ctx, "ssh", container, "sh", "-c", check) //nolint:gosec // container is not user-controlled
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
	arg := fmt.Sprintf("tail -c %d %s 2>/dev/null", maxBytes, RelayLogPath)
	cmd := exec.CommandContext(ctx, "ssh", container, arg) //nolint:gosec // container is not user-controlled
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
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // args are not user-controlled.
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

// StartRelay deploys the relay, launches an agent via relay serve-attach with
// the given CLI args, and sends the initial prompt. Used by backends that
// follow the standard relay protocol (claude, gemini, kilo).
func StartRelay(ctx context.Context, opts *Options, agentArgs []string, msgCh chan<- Message, logW io.Writer, wire WireFormat) (*Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	tStart := time.Now()
	if err := DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}
	slog.Debug("startup", "phase", "deploy_relay", "ctr", opts.Container, "dur", time.Since(tStart))

	sshArgs := make([]string, 0, 7+len(agentArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--")
	sshArgs = append(sshArgs, agentArgs...)

	slog.Debug("relay", "msg", "launch", "ctr", opts.Container, "args", agentArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &SlogWriter{Prefix: "relay serve-attach", Container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}
	slog.Info("startup", "phase", "relay_started", "ctr", opts.Container, "dur", time.Since(tStart))

	log := slog.With("ctr", opts.Container)
	s := NewSession(cmd, stdin, stdout, msgCh, logW, wire, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses each line using parseFn.
func ReadRelayOutput(ctx context.Context, container string, parseFn func([]byte) ([]Message, error)) (msgs []Message, size int64, err error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "cat", RelayOutputPath) //nolint:gosec // args are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("read relay output: %w", err)
	}
	size = int64(len(out))
	scanner := bufio.NewScanner(bytes.NewReader(out))
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		parsed, parseErr := parseFn(line)
		if parseErr != nil {
			slog.Warn("relay", "msg", "skipping unparseable output line", "ctr", container, "err", parseErr)
			continue
		}
		msgs = append(msgs, parsed...)
	}
	return msgs, size, scanner.Err()
}

// AttachRelaySession connects to an already-running relay in the container
// and returns a new Session. It waits briefly for the attach process to
// confirm connectivity; if the process exits immediately (e.g. relay socket
// is stale), an error is returned so the caller can fall back to --resume.
func AttachRelaySession(ctx context.Context, container string, offset int64, msgCh chan<- Message, logW io.Writer, wire WireFormat) (*Session, error) {
	sshArgs := []string{
		container, "python3", RelayScriptPath, "attach",
		"--offset", strconv.FormatInt(offset, 10),
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
	cmd.Stderr = &SlogWriter{Prefix: "relay attach", Container: container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	log := slog.With("ctr", container)
	return NewSession(cmd, stdin, stdout, msgCh, logW, wire, log), nil
}

// PlainTextWritePrompt writes a user prompt as a plain text line on stdin
// and logs it as NDJSON. Used by backends whose agent reads plain text
// (gemini, kilo).
func PlainTextWritePrompt(w io.Writer, p Prompt, logW io.Writer) error {
	data := []byte(p.Text + "\n")
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		entry, _ := json.Marshal(map[string]string{
			"type":    "user_input",
			"content": p.Text,
		})
		_, _ = logW.Write(append(entry, '\n'))
	}
	return nil
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
