//go:build e2e

// Package fake implements agent.Backend for e2e testing with a Python script
// that emits flat NDJSON messages.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend using a Python subprocess that cycles
// through canned responses in a flat NDJSON wire format.
type Backend struct {
	agent.Base
}

var _ agent.Backend = (*Backend)(nil)

// New creates a fake backend for e2e testing.
func New() *Backend {
	return &Backend{Base: agent.Base{
		HarnessID:     "fake",
		ModelList:     []string{"fake-model"},
		Images:        true,
		Compact:       true,
		ContextWindow: 180_000,
	}}
}

// WritePrompt writes the prompt as plain text. The fake Python agent reads
// lines from stdin and matches keywords — it does not parse JSON input.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	return agent.PlainTextWritePrompt(w, p, logW)
}

// ParseMessage decodes a single flat NDJSON line from the fake agent.
func (*Backend) ParseMessage(line []byte) ([]agent.Message, error) {
	return parseMessage(line)
}

// Start launches the embedded fake Python agent as a subprocess.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "python3", "-u", "-c", string(Script)) //nolint:gosec // Script is an embedded constant
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, b), stdout, opts.MsgCh, nil)
	if opts.InitialPrompt.Text != "" {
		if err := s.SendPrompt(opts.InitialPrompt); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// AgentArgs implements agent.Backend. Returns nil because the fake agent is
// embedded and not launched via record-trace.
func (*Backend) AgentArgs(_ agent.HarnessArgs) []string {
	return nil
}

// AttachRelay implements agent.Backend.
func (*Backend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("fake backend does not support relay")
}

// NewWire implements agent.Backend.
func (b *Backend) NewWire() agent.WireFormat {
	return b
}
