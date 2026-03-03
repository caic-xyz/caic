// Package gemini implements agent.Backend for Gemini CLI.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Gemini CLI.
type Backend struct{}

var _ agent.Backend = (*Backend)(nil)

// Wire is the wire format for Gemini CLI (stream-json over stdin/stdout).
var Wire agent.WireFormat = &Backend{}

// Harness returns the harness identifier.
func (b *Backend) Harness() agent.Harness { return agent.Gemini }

// Models returns the model names supported by Gemini CLI.
//
// TODO: Figure out a way to generate this list at runtime.
func (b *Backend) Models() []string { return []string{"gemini-3.1-pro", "gemini-3-flash"} }

// SupportsImages reports that Gemini CLI does not yet accept image input.
func (b *Backend) SupportsImages() bool { return false }

// ContextWindowLimit returns the API prompt token limit for Gemini models.
func (b *Backend) ContextWindowLimit(model string) int { return 1_000_000 }

// Start launches a Gemini CLI process via the relay daemon in the given
// container.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	geminiArgs := buildArgs(opts)

	sshArgs := make([]string, 0, 7+len(geminiArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--")
	sshArgs = append(sshArgs, geminiArgs...)

	slog.Info("gemini: launching via relay", "container", opts.Container, "args", geminiArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Prefix: "relay serve-attach", Container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	log := slog.With("container", opts.Container)
	s := agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire, log)
	if opts.InitialPrompt.Text != "" {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, container string, offset int64, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, container, offset, msgCh, logW, Wire)
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses it into Messages.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	return agent.ReadRelayOutput(ctx, container, ParseMessage)
}

// ParseMessage decodes a single Gemini CLI stream-json line into a typed Message.
func (b *Backend) ParseMessage(line []byte) (agent.Message, error) {
	return ParseMessage(line)
}

// WritePrompt writes a single user message to Gemini CLI's stdin.
// Gemini CLI in -p mode reads plain text lines from stdin. Images are ignored.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	return agent.PlainTextWritePrompt(w, p, logW)
}

// buildArgs constructs the Gemini CLI arguments.
func buildArgs(opts *agent.Options) []string {
	args := []string{
		"gemini", "-p",
		"--output-format", "stream-json",
		"--yolo",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args
}
