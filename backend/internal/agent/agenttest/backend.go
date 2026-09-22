// Configurable agent.Backend test double.

package agenttest

import (
	"context"
	"errors"
	"io"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// Wire is a configurable agent.WireFormat test double.
//
// Its zero value ignores prompts and parses no messages. Set Parse to model
// the normalized messages a test needs from one native log line.
type Wire struct {
	Parse func([]byte) ([]agent.Message, error)
}

// WritePrompt implements agent.WireFormat.
func (*Wire) WritePrompt(io.Writer, agent.Prompt, agent.LogSink) error { return nil }

// ParseMessage implements agent.WireFormat.
func (w *Wire) ParseMessage(line []byte) ([]agent.Message, error) {
	if w.Parse == nil {
		return nil, nil
	}
	return w.Parse(line)
}

// FakeBackend is a configurable agent.Backend for tests. The zero value is
// usable: Start and AttachRelay return an error so a stray launch fails loudly,
// and the metadata methods report benign defaults. Set the exported fields to
// configure metadata; to drive a real session, embed it and override Start or
// AttachRelay.
type FakeBackend struct {
	HarnessName     harness.Name
	QuotaProviderID agent.QuotaProvider
	Inventory       agent.ModelInventory
	Images          bool
	Compact         bool
	// WireFactory, when set, backs NewWire. Set it (e.g. to a harness's real
	// parser) for tests that replay stored wire output; the default is a fresh
	// no-op Wire, since agenttest cannot import a specific harness without a
	// cycle.
	WireFactory func() agent.WireFormat
}

// Harness implements agent.Backend, reporting HarnessName or "fake" if unset.
func (f *FakeBackend) Harness() harness.Name {
	if f.HarnessName == "" {
		return "fake"
	}
	return f.HarnessName
}

// QuotaProvider implements agent.Backend.
func (f *FakeBackend) QuotaProvider() agent.QuotaProvider { return f.QuotaProviderID }

// Start implements agent.Backend.
func (f *FakeBackend) Start(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("agenttest: Start not implemented")
}

// AttachRelay implements agent.Backend.
func (f *FakeBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("agenttest: AttachRelay not implemented")
}

// SupportsImages implements agent.Backend.
func (f *FakeBackend) SupportsImages() bool { return f.Images }

// SupportsCompact implements agent.Backend.
func (f *FakeBackend) SupportsCompact() bool { return f.Compact }

// ModelInventory implements agent.Backend.
func (f *FakeBackend) ModelInventory() agent.ModelInventory { return f.Inventory }

// SetModelInventory implements agent.Backend.
func (f *FakeBackend) SetModelInventory(inventory agent.ModelInventory) {
	f.Inventory = inventory
}

// AgentArgs implements agent.Backend.
func (f *FakeBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

// NewWire implements agent.Backend, delegating to WireFactory when set and
// otherwise returning a no-op wire.
func (f *FakeBackend) NewWire() agent.WireFormat {
	if f.WireFactory != nil {
		return f.WireFactory()
	}
	return &Wire{}
}

// Ensure the fake satisfies the interface at compile time.
var _ agent.Backend = (*FakeBackend)(nil)
