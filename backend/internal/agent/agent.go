// Package agent defines shared types and infrastructure for coding agent
// backends. Backend implementations live in sub-packages (e.g. agent/claude).
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
	"os/exec"
	"strings"
	"sync"

	"github.com/maruel/caic/backend/internal/agent/relay"
)

// Options configures an agent session launch.
type Options struct {
	Container       string
	Dir             string // Working directory inside the container.
	MaxTurns        int
	Model           string // Model alias ("opus", "sonnet", "haiku") or full ID. Empty = default.
	ResumeSessionID string
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair WritePrompt and ParseMessage
// for the same protocol.
type WireFormat interface {
	// WritePrompt writes a user prompt to the agent's stdin in the
	// backend's wire format. logW receives a copy (may be nil).
	WritePrompt(w io.Writer, prompt string, logW io.Writer) error

	// ParseMessage decodes a single NDJSON line into a typed Message.
	ParseMessage(line []byte) (Message, error)
}

// Session manages a running agent process.
type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	logW      io.Writer
	wire      WireFormat
	mu        sync.Mutex // serializes stdin writes
	closeOnce sync.Once
	done      chan struct{} // closed when readMessages goroutine exits
	result    *ResultMessage
	err       error
}

// NewSession creates a Session from an already-started command. Messages read
// from stdout are parsed and sent to msgCh. logW receives raw NDJSON lines
// (may be nil). wire defines the backend's wire protocol.
func NewSession(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, msgCh chan<- Message, logW io.Writer, wire WireFormat) *Session {
	s := &Session{
		cmd:   cmd,
		stdin: stdin,
		logW:  logW,
		wire:  wire,
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
			slog.Info("agent session completed", "result", result.Subtype)
		case parseErr != nil:
			s.err = fmt.Errorf("parse: %w", parseErr)
			slog.Error("agent session parse error", "err", parseErr)
		case waitErr != nil:
			s.err = fmt.Errorf("agent exited: %w", waitErr)
			slog.Error("agent session exited with error", "err", waitErr)
		default:
			s.err = errors.New("agent exited without a result message")
			slog.Error("agent session exited without result message")
		}
	}()

	return s
}

// Send writes a user message to the agent's stdin. It is safe for concurrent
// use. The first call typically provides the initial task prompt.
func (s *Session) Send(prompt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire.WritePrompt(s.stdin, prompt, s.logW)
}

// Close closes stdin so the agent process can exit. Idempotent.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
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
	// Agents can produce long lines (e.g., base64 images in tool results).
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

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
			slog.Warn("skipping unparseable message", "err", err, "line", string(line))
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
	slog.Debug("readMessages: loop exited", "linesRead", n, "hasResult", result != nil, "scanErr", scanner.Err())
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
	default:
		// stream_event, tool_progress, etc. — pass through as raw.
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
)

// DeployRelay uploads the relay script into the container. Idempotent.
func DeployRelay(ctx context.Context, container string) error {
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", container,
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
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-d", RelayDir)
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
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-S", RelaySockPath)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("test relay socket: %w", err)
	}
	return true, nil
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
