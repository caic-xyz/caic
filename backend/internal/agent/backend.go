// Defines the Backend interface that all coding-agent implementations must satisfy.

package agent

import "context"

// Backend launches and communicates with a coding agent process.
// Each implementation translates its native wire format into the shared
// Message types so the rest of the system (task, eventconv, SSE, frontend)
// remains agent-agnostic.
type Backend interface {
	// Start launches the agent in the given container. Parsed messages are
	// forwarded to opts.MsgCh; opts.LogW receives raw wire-format lines.
	Start(ctx context.Context, opts *Options) (*Session, error)

	// AttachRelay connects to an already-running relay daemon in the
	// container. opts.RelayOffset specifies the byte offset into
	// output.jsonl to replay from (use 0 for full replay).
	// opts.ResumeSessionID is the known agent session ID, used by stateful
	// wire formats (e.g. codex) that need it before the first replay message.
	AttachRelay(ctx context.Context, opts *Options) (*Session, error)

	// ReadRelayOutput reads the complete output.jsonl from the container's
	// relay and parses it into Messages. Also returns the byte count for
	// use as an offset in AttachRelay.
	ReadRelayOutput(ctx context.Context, container string) ([]Message, int64, error)

	// Harness returns the harness identifier ("claude", "gemini", etc.)
	Harness() Harness

	// Models returns the list of model names supported by this backend.
	Models() []string

	// SetModels replaces the model list. Used by the server to push
	// dynamically-fetched models into all runners.
	SetModels(models []string)

	// SupportsImages reports whether this backend accepts image content blocks.
	SupportsImages() bool

	// SupportsCompact reports whether this backend supports context compaction.
	SupportsCompact() bool

	// ContextWindowLimit returns the API prompt token limit for the given model.
	// The model parameter is the model name reported by the agent at runtime.
	ContextWindowLimit(model string) int

	// NewParser returns a fresh parse function for offline log replay.
	// Each call creates independent dedup state.
	NewParser() func([]byte) ([]Message, error)
}

// Base provides default implementations for metadata-only Backend methods.
// Embed it in backend-specific types to inherit the boilerplate. Each backend
// must implement Start, AttachRelay, and ReadRelayOutput itself using the
// package-level helpers (StartRelay, AttachRelaySession, ReadRelayOutput).
type Base struct {
	HarnessID     Harness
	ModelList     []string
	Images        bool
	ContextWindow int
	Wire          WireFormat // Used by SupportsCompact.
}

// Harness implements Backend.
func (b *Base) Harness() Harness { return b.HarnessID }

// Models implements Backend.
func (b *Base) Models() []string { return b.ModelList }

// SetModels implements Backend.
func (b *Base) SetModels(models []string) { b.ModelList = models }

// SupportsImages implements Backend.
func (b *Base) SupportsImages() bool { return b.Images }

// SupportsCompact implements Backend by checking if Wire implements CompactCommand.
func (b *Base) SupportsCompact() bool {
	_, ok := b.Wire.(CompactCommand)
	return ok
}

// ContextWindowLimit implements Backend.
func (b *Base) ContextWindowLimit(string) int { return b.ContextWindow }
