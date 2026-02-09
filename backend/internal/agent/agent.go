// Package agent manages Claude Code processes via the streaming JSON protocol.
package agent

import (
	"bufio"
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
)

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
// initial prompt, then Wait for the result. If resumeSessionID is non-empty,
// the session is resumed via --resume.
func Start(ctx context.Context, container string, maxTurns int, msgCh chan<- Message, logW io.Writer, resumeSessionID string) (*Session, error) {
	args := []string{
		container,
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if maxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
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

	return s, nil
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
func Run(ctx context.Context, container, task string, maxTurns int, msgCh chan<- Message, logW io.Writer) (*ResultMessage, error) {
	s, err := Start(ctx, container, maxTurns, msgCh, logW, "")
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
		msg, err := parseMessage(line)
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

// parseMessage decodes a single NDJSON line into a typed Message.
func parseMessage(line []byte) (Message, error) {
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

// FindRunningSession checks whether a claude process is running in the
// container. If one is found and was started with --resume, the session ID
// is returned. Returns ("", false, nil) when no process is running.
func FindRunningSession(ctx context.Context, container string) (sessionID string, running bool, err error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "pgrep", "-a", "claude")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// Exit code 1 means no process matched.
			return "", false, nil
		}
		return "", false, fmt.Errorf("pgrep: %w", err)
	}
	sid, _ := parsePgrepOutput(string(out))
	return sid, true, nil
}

// KillRunning kills any running claude process in the container.
func KillRunning(ctx context.Context, container string) error {
	cmd := exec.CommandContext(ctx, "ssh", container, "pkill", "-x", "claude")
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// Exit code 1 means no process matched — not an error.
			return nil
		}
		return fmt.Errorf("pkill: %w", err)
	}
	return nil
}

// parsePgrepOutput extracts the --resume session ID from pgrep -a output. If
// no --resume flag is found, sessionID is empty. Returns found=true if the
// output is non-empty (i.e. at least one process matched).
func parsePgrepOutput(output string) (sessionID string, found bool) {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		found = true
		// Look for "--resume <id>" in the command line.
		if _, rest, ok := strings.Cut(line, "--resume "); ok {
			sessionID, _, _ = strings.Cut(rest, " ")
			return sessionID, true
		}
	}
	return "", found
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
