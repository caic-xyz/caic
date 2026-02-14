// Package claude implements agent.Backend for Claude Code.
package claude

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

	"github.com/maruel/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Claude Code.
type Backend struct{}

var _ agent.Backend = (*Backend)(nil)

// Wire is the wire format for Claude Code (stream-json over stdin/stdout).
var Wire agent.WireFormat = &Backend{}

// Harness returns the harness identifier.
func (b *Backend) Harness() agent.Harness { return agent.Claude }

// Models returns the model aliases supported by Claude Code CLI.
func (b *Backend) Models() []string { return []string{"opus", "sonnet", "haiku"} }

// Start launches a Claude Code process via the relay daemon in the given
// container. It deploys the relay script and starts claude via serve-attach.
func (b *Backend) Start(ctx context.Context, opts agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	claudeArgs := buildArgs(opts)

	// Build the ssh command: ssh <container> python3 relay.py serve-attach --dir <dir> -- claude ...
	sshArgs := make([]string, 0, 7+len(claudeArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--")
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

	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire), nil
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, container string, offset int64, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	sshArgs := []string{
		container, "python3", agent.RelayScriptPath, "attach",
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

	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire), nil
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses it into Messages.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) (msgs []agent.Message, size int64, err error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "cat", agent.RelayOutputPath) //nolint:gosec // args are not user-controlled.
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
		msg, parseErr := b.ParseMessage(line)
		if parseErr != nil {
			slog.Warn("skipping unparseable relay output line", "container", container, "err", parseErr)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, size, scanner.Err()
}

// ParseMessage decodes a single Claude Code NDJSON line into a typed Message.
func (b *Backend) ParseMessage(line []byte) (agent.Message, error) {
	return agent.ParseMessage(line)
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

// WritePrompt writes a single user message in Claude Code's stdin format.
func (*Backend) WritePrompt(w io.Writer, prompt string, logW io.Writer) error {
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

// buildArgs constructs the Claude Code CLI arguments.
func buildArgs(opts agent.Options) []string {
	args := []string{
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
	return args
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
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Warn("stderr", "source", w.prefix, "container", w.container, "line", line)
		}
	}
	return len(p), nil
}
