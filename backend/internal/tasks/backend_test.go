// Test fake agent.Backend for Manager lifecycle tests.

package tasks

import (
	"context"
	"errors"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/harness"
)

// fakeBackend is a minimal agent.Backend for Manager lifecycle tests. Only the
// metadata methods (Models, SupportsImages) are exercised by the synchronous
// validation paths; the process-launching methods return errors so any
// background goroutine that reaches them fails cleanly.
type fakeBackend struct {
	models         []string
	supportsImages bool
}

func (b *fakeBackend) Start(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("fake backend cannot start")
}

func (b *fakeBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("fake backend cannot attach relay")
}

func (b *fakeBackend) Harness() harness.Name { return "fake" }

func (b *fakeBackend) Models() []string { return b.models }

func (b *fakeBackend) SetModels([]string) {}

func (b *fakeBackend) SupportsImages() bool { return b.supportsImages }

func (b *fakeBackend) SupportsCompact() bool { return false }

func (b *fakeBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

func (b *fakeBackend) NewWire() agent.WireFormat { return claudecode.New() }

func (b *fakeBackend) ContextWindowLimit(string) int { return 200_000 }
