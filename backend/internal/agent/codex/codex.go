// Package codex implements agent.Backend for Codex CLI.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"

	"github.com/maruel/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Codex CLI.
type Backend struct{}

var _ agent.Backend = (*Backend)(nil)

// Wire is the wire format for Codex CLI (exec --json over stdout).
var Wire agent.WireFormat = &Backend{}

// Harness returns the harness identifier.
func (b *Backend) Harness() agent.Harness { return agent.Codex }

// Models returns the model names supported by Codex CLI.
func (b *Backend) Models() []string { return []string{"o4-mini", "codex-mini-latest"} }

// SupportsImages reports that Codex CLI does not accept image input.
func (b *Backend) SupportsImages() bool { return false }

// Start launches a Codex CLI process via the relay daemon in the given
// container.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	codexArgs := buildArgs(opts)

	sshArgs := make([]string, 0, 7+len(codexArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--")
	sshArgs = append(sshArgs, codexArgs...)

	slog.Info("codex: launching via relay", "container", opts.Container, "args", codexArgs)
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

	log := slog.With("container", opts.Container)
	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire, log), nil
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

	log := slog.With("container", container)
	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire, log), nil
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
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
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

// ParseMessage decodes a single Codex CLI exec --json line into a typed Message.
func (b *Backend) ParseMessage(line []byte) (agent.Message, error) {
	return ParseMessage(line)
}

// WritePrompt returns an error because Codex exec mode is non-interactive.
// It does not read follow-up prompts from stdin.
func (*Backend) WritePrompt(_ io.Writer, _ string, _ []agent.ImageData, _ io.Writer) error {
	return errors.New("codex exec mode does not support follow-up prompts")
}

// buildArgs constructs the Codex CLI arguments.
func buildArgs(opts *agent.Options) []string {
	args := []string{
		"codex", "exec", "--json",
		"--full-auto",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.Prompt != "" {
		args = append(args, opts.Prompt)
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
