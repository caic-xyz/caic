// Fake agent sessions and shared model inventories for smoke and e2e captures.

package smoketest

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
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

// NewFakeBackend creates a Claude-compatible fake backend for smoke and e2e testing.
func NewFakeBackend() *FakeBackend {
	b := &FakeBackend{
		HarnessID:       harness.Claude,
		QuotaProviderID: agent.QuotaProviderClaudeCode,
		Images:          true,
		Compact:         true,
	}
	b.SetModelInventory(agent.ModelInventory{Models: []agent.Model{{ID: "fake-model", ContextWindow: 180_000}}})
	return b
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
	model := opts.Model
	if model == "" && os.Getenv("CAIC_E2E_VISUALS") == "1" {
		model = VisualModelInventory(b.Harness()).Models[0].ID
	}
	cmd.Env = append(os.Environ(), "CAIC_FAKE_MODEL="+model, "CAIC_FAKE_EFFORT="+opts.Effort, "CAIC_FAKE_HARNESS="+string(b.Harness()))
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
	s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Log, b), stdout, opts.MsgCh, opts.Logger)
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

// VisualModelInventory defines the selected documentation models and their
// supported effort controls for both discovery and the persisted harness cache.
func VisualModelInventory(h harness.Name) agent.ModelInventory {
	m := agent.Model{ID: "fake-model", ContextWindow: 200_000}
	switch h {
	case harness.Antigravity:
		m.ID = "gemini-3.8-flash-high"
	case harness.Claude:
		m.ID = "opus-5.5"
		m.EffortOptions = []string{"low", "medium", "high"}
	case harness.Codex:
		m.ID = "gpt-6.1-sol"
		m.ContextWindow = 272_000
		m.EffortOptions = []string{"low", "medium", "high"}
	case harness.OpenCode:
		// OpenCode keeps the general fixture model; it is not a capture example.
	case harness.Pi:
		m.ID = "deepseek/deepseek-flashh"
		m.EffortOptions = []string{"low", "medium", "high"}
	}
	return agent.ModelInventory{Models: []agent.Model{m}}
}

var _ agent.Backend = (*FakeBackend)(nil)
