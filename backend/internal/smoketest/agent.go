// Fake agent backend for smoke and e2e tests.

package smoketest

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// fakeScript is the fake agent that cycles through jokes in Claude Code streaming JSON format.
//
//go:embed fake_agent.py
var fakeScript []byte

// FakeBackend implements agent.Backend using a Python subprocess that cycles
// through canned responses carried in canonical v2 task-log envelopes.
type FakeBackend struct {
	agent.Base
}

var _ agent.Backend = (*FakeBackend)(nil)

// NewFakeBackend creates a fake backend for smoke and e2e testing.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{Base: agent.Base{
		HarnessID:     "fake",
		Inventory:     agent.ModelInventory{Models: []agent.Model{{ID: "fake-model"}}},
		Images:        true,
		Compact:       true,
		ContextWindow: 180_000,
	}}
}

// WritePrompt writes the prompt as plain text.
//
// The fake Python agent reads lines from stdin and matches keywords; it does
// not parse JSON input.
func (*FakeBackend) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
	return agent.PlainTextWritePrompt(w, p, log)
}

// ParseMessage decodes a single flat NDJSON line from the fake agent.
func (*FakeBackend) ParseMessage(line []byte) ([]agent.Message, error) {
	return parseMessage(line)
}

// Start launches the embedded fake Python agent as a subprocess.
func (b *FakeBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "python3", "-u", "-c", string(fakeScript)) //nolint:gosec // fakeScript is an embedded constant
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
	s := agent.NewSession(cmd, agent.NewConn(stdin, opts.Log, b), stdout, opts.MsgCh, nil)
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
func (*FakeBackend) AgentArgs(_ agent.HarnessArgs) []string {
	return nil
}

// AttachRelay implements agent.Backend.
func (*FakeBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("fake backend does not support relay")
}

// NewWire implements agent.Backend.
func (b *FakeBackend) NewWire() agent.WireFormat {
	return b
}
