// Package agent manages Claude Code processes via the streaming JSON protocol.
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
	"strconv"
	"strings"
	"sync"

	"github.com/maruel/caic/backend/internal/agent/relay"
)

// Options configures an agent session launch.
type Options struct {
	Container       string
	MaxTurns        int
	Model           string // Claude Code model alias ("opus", "sonnet", "haiku") or full ID. Empty = default.
	ResumeSessionID string
}

// Session manages a running Claude Code process. Use Start to create one.
type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	logW      io.Writer
	mu        sync.Mutex // serializes stdin writes
	closeOnce sync.Once
	done      chan struct{} // closed when readMessages goroutine exits
	result    *ResultMessage
	err       error
}

// Start launches a Claude Code process in the given container. Messages are
// sent to msgCh as they arrive. The caller must call Send to provide the
// initial prompt, then Wait for the result.
func Start(ctx context.Context, opts Options, msgCh chan<- Message, logW io.Writer) (*Session, error) {
	args := []string{
		opts.Container,
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}

	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = nil // Let stderr go to /dev/null; errors come via JSON.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	return NewSession(cmd, stdin, stdout, msgCh, logW), nil
}

// NewSession creates a Session from an already-started command. Messages read
// from stdout are sent to msgCh. logW receives raw NDJSON lines (may be nil).
func NewSession(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader, msgCh chan<- Message, logW io.Writer) *Session {
	s := &Session{
		cmd:   cmd,
		stdin: stdin,
		logW:  logW,
		done:  make(chan struct{}),
	}

	go func() {
		defer close(s.done)
		result, parseErr := readMessages(stdout, msgCh, logW)
		waitErr := cmd.Wait()
		// Store the result and first non-nil error.
		s.result = result
		switch {
		case result != nil:
			// Got a proper result — ignore exit errors.
		case parseErr != nil:
			s.err = fmt.Errorf("parse: %w", parseErr)
		case waitErr != nil:
			s.err = fmt.Errorf("claude exited: %w", waitErr)
		default:
			s.err = errors.New("claude exited without a result message")
		}
	}()

	return s
}

// Send writes a user message to the agent's stdin. It is safe for concurrent
// use. The first call typically provides the initial task prompt.
func (s *Session) Send(prompt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeMessage(s.stdin, prompt, s.logW)
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

// Run executes Claude Code over SSH in the given container with the task
// prompt. It streams NDJSON from stdout and returns the final Result.
//
// All intermediate messages are sent to msgCh for logging/observability.
// If logW is non-nil, every raw NDJSON line (input and output) is written to it.
func Run(ctx context.Context, opts Options, task string, msgCh chan<- Message, logW io.Writer) (*ResultMessage, error) {
	s, err := Start(ctx, opts, msgCh, logW)
	if err != nil {
		return nil, err
	}
	if err := s.Send(task); err != nil {
		return nil, fmt.Errorf("write prompt: %w", err)
	}
	s.Close()
	return s.Wait()
}

// userInputMessage is the NDJSON message sent to Claude Code via stdin.
type userInputMessage struct {
	Type    string           `json:"type"`
	Message userInputContent `json:"message"`
}

type userInputContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// writeMessage writes a single user message NDJSON line to w.
// If logW is non-nil, the same line is also written to the log.
func writeMessage(w io.Writer, prompt string, logW io.Writer) error {
	msg := userInputMessage{
		Type:    "user",
		Message: userInputContent{Role: "user", Content: prompt},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
}

// readMessages reads NDJSON lines from r, dispatches to msgCh, and returns
// the terminal ResultMessage. If logW is non-nil, each raw line is written to it.
func readMessages(r io.Reader, msgCh chan<- Message, logW io.Writer) (*ResultMessage, error) {
	scanner := bufio.NewScanner(r)
	// Claude can produce long lines (e.g., base64 images in tool results).
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var result *ResultMessage
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if logW != nil {
			_, _ = logW.Write(line)
			_, _ = logW.Write([]byte{'\n'})
		}
		msg, err := ParseMessage(line)
		if err != nil {
			slog.Warn("skipping unparseable message", "err", err, "line", string(line))
			continue
		}
		if msgCh != nil {
			msgCh <- msg
		}
		if rm, ok := msg.(*ResultMessage); ok {
			result = rm
		}
	}
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
	relayDir        = "/tmp/caic-relay"
	relayScriptPath = relayDir + "/relay.py"
	relaySockPath   = relayDir + "/relay.sock"
	relayOutputPath = relayDir + "/output.jsonl"
)

// DeployRelay uploads the relay script into the container. Idempotent.
func DeployRelay(ctx context.Context, container string) error {
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", container,
		"mkdir -p "+relayDir+" && cat > "+relayScriptPath)
	cmd.Stdin = bytes.NewReader(relay.Script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy relay: %w: %s", err, out)
	}
	return nil
}

// StartWithRelay deploys the relay script and starts claude via serve-attach.
// The relay daemon survives SSH disconnects; the returned Session talks to the
// attach half (bridging the socket to stdio over SSH).
func StartWithRelay(ctx context.Context, opts Options, msgCh chan<- Message, logW io.Writer) (*Session, error) {
	if err := DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	claudeArgs := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if opts.MaxTurns > 0 {
		claudeArgs = append(claudeArgs, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Model != "" {
		claudeArgs = append(claudeArgs, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		claudeArgs = append(claudeArgs, "--resume", opts.ResumeSessionID)
	}

	// Build the ssh command: ssh <container> python3 relay.py serve-attach -- claude ...
	sshArgs := make([]string, 0, 5+len(claudeArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", relayScriptPath, "serve-attach", "--")
	sshArgs = append(sshArgs, claudeArgs...)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &slogWriter{prefix: "relay serve-attach", container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	return NewSession(cmd, stdin, stdout, msgCh, logW), nil
}

// AttachRelay connects to an already-running relay in the container. The
// offset parameter specifies the byte offset into output.jsonl to replay from
// (use 0 for full replay).
func AttachRelay(ctx context.Context, container string, offset int64, msgCh chan<- Message, logW io.Writer) (*Session, error) {
	sshArgs := []string{
		container, "python3", relayScriptPath, "attach",
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
	cmd.Stderr = &slogWriter{prefix: "relay attach", container: container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	return NewSession(cmd, stdin, stdout, msgCh, logW), nil
}

// slogWriter is an io.Writer that logs each line via slog.Warn.
type slogWriter struct {
	prefix    string
	container string
	buf       []byte
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Warn("stderr", "source", w.prefix, "container", w.container, "line", line)
		}
	}
	return len(p), nil
}

// HasRelayDir checks whether the caic relay directory exists in the container.
// Its presence proves caic deployed the relay at some point.
func HasRelayDir(ctx context.Context, container string) (bool, error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-d", relayDir)
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
	cmd := exec.CommandContext(ctx, "ssh", container, "test", "-S", relaySockPath)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("test relay socket: %w", err)
	}
	return true, nil
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses it into Messages. Also returns the byte count for use as an
// offset in AttachRelay.
func ReadRelayOutput(ctx context.Context, container string) (msgs []Message, size int64, err error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "cat", relayOutputPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("read relay output: %w", err)
	}
	size = int64(len(out))
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, parseErr := ParseMessage(line)
		if parseErr != nil {
			slog.Warn("skipping unparseable relay output line", "container", container, "err", parseErr)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, size, scanner.Err()
}
