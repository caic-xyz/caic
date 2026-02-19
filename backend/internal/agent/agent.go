// Package agent defines shared types and infrastructure for coding agent
// backends. Backend implementations live in sub-packages (e.g. agent/claude).
//
// # Relay shutdown protocol
//
// Each agent runs inside a container behind a relay daemon (relay.py) that
// survives SSH disconnects. Graceful shutdown uses a null-byte (\x00)
// sentinel written to stdin:
//
// Flow 1 — One task is terminated (user action or container death):
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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/maruel/caic/backend/internal/agent/relay"
)

// ImageData carries a single base64-encoded image for multi-modal input.
type ImageData struct {
	MediaType string // e.g. "image/png", "image/jpeg"
	Data      string // base64-encoded
}

// Options configures an agent session launch.
type Options struct {
	Container       string
	Dir             string // Working directory inside the container.
	MaxTurns        int
	Model           string      // Model alias ("opus", "sonnet", "haiku") or full ID. Empty = default.
	Prompt          string      // Initial prompt. Backends deliver it at launch time.
	Images          []ImageData // Images sent with the initial prompt.
	ResumeSessionID string
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair WritePrompt and ParseMessage
// for the same protocol.
type WireFormat interface {
	// WritePrompt writes a user prompt to the agent's stdin in the
	// backend's wire format. images may be nil. logW receives a copy (may be nil).
	WritePrompt(w io.Writer, prompt string, images []ImageData, logW io.Writer) error

	// ParseMessage decodes a single NDJSON line into a typed Message.
	ParseMessage(line []byte) (Message, error)
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
			log.Info("agent session completed", "result", result.Subtype)
		case parseErr != nil:
			s.err = fmt.Errorf("parse: %w", parseErr)
			log.Error("agent session parse error", "err", parseErr)
		case waitErr != nil:
			s.err = fmt.Errorf("agent exited: %w", waitErr)
			// Signal-based exits (SIGKILL, SIGTERM) are expected when
			// containers are terminated. Log at Info, not Error.
			if isSignalExit(waitErr) {
				log.Info("agent session killed by signal", "err", waitErr)
			} else {
				log.Warn("agent session exited with error", "err", waitErr)
			}
		default:
			s.err = errors.New("agent exited without a result message")
			log.Error("agent session exited without result message")
		}
	}()

	return s
}

// Send writes a user message to the agent's stdin. It is safe for concurrent
// use. The first call typically provides the initial task prompt.
func (s *Session) Send(prompt string, images []ImageData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire.WritePrompt(s.stdin, prompt, images, s.logW)
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
func readMessages(r io.Reader, msgCh chan<- Message, logW io.Writer, parseFn func([]byte) (Message, error)) (*ResultMessage, error) {
	scanner := bufio.NewScanner(r)
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	slog.Debug("readMessages: started reading agent stdout")
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
		msg, err := parseFn(line)
		if err != nil {
			slog.Warn("skipping unparseable message", "err", err, "line", string(line)) //nolint:gosec // structured logging, no injection
			continue
		}
		if n <= 3 {
			slog.Debug("readMessages: parsed message", "n", n, "type", fmt.Sprintf("%T", msg))
		}
		if msgCh != nil {
			msgCh <- msg
		}
		if rm, ok := msg.(*ResultMessage); ok {
			result = rm
		}
	}
	slog.Debug("readMessages: loop exited", "linesRead", n, "hasResult", result != nil, "scanErr", scanner.Err()) //nolint:gosec // structured logging, no injection
	return result, scanner.Err()
}

// ParseMessage decodes a single NDJSON line into a typed Message.
func ParseMessage(line []byte) (Message, error) {
	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch envelope.Type {
	case "system":
		switch envelope.Subtype {
		case "init":
			var m SystemInitMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return &m, nil
		default:
			var m SystemMessage
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, err
			}
			return &m, nil
		}
	case "assistant":
		var m AssistantMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "user":
		var m UserMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "result":
		var m ResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "stream_event":
		var m StreamEvent
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "caic_diff_stat":
		var m DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		// tool_progress, etc. — pass through as raw.
		return &RawMessage{MessageType: envelope.Type, Raw: append([]byte(nil), line...)}, nil
	}
}

// TextFromAssistant extracts all text blocks from an assistant message's content.
func TextFromAssistant(m *AssistantMessage) string {
	var parts []string
	for _, b := range m.Message.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
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

// IsRelayRunning checks whether the relay socket exists in the container.
func IsRelayRunning(ctx context.Context, container string) (bool, error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-S", RelaySockPath) //nolint:gosec // container is not user-controlled
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("test relay socket: %w", err)
	}
	return true, nil
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

// isSignalExit reports whether err indicates the process was killed by a
// signal (e.g. SIGKILL from container termination).
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
