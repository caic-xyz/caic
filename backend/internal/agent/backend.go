package agent

import (
	"context"
	"io"
)

// Backend launches and communicates with a coding agent process.
// Each implementation translates its native wire format into the shared
// Message types so the rest of the system (task, eventconv, SSE, frontend)
// remains agent-agnostic.
type Backend interface {
	// Start launches the agent in the given container. Messages are emitted
	// to msgCh as normalized agent.Message values. logW receives raw
	// wire-format lines for debugging/replay.
	Start(ctx context.Context, opts *Options, msgCh chan<- Message, logW io.Writer) (*Session, error)

	// AttachRelay connects to an already-running relay daemon in the
	// container. opts.RelayOffset specifies the byte offset into
	// output.jsonl to replay from (use 0 for full replay).
	// opts.ResumeSessionID is the known agent session ID, used by stateful
	// wire formats (e.g. codex) that need it before the first replay message.
	AttachRelay(ctx context.Context, opts *Options, msgCh chan<- Message, logW io.Writer) (*Session, error)

	// ReadRelayOutput reads the complete output.jsonl from the container's
	// relay and parses it into Messages. Also returns the byte count for
	// use as an offset in AttachRelay.
	ReadRelayOutput(ctx context.Context, container string) ([]Message, int64, error)

	// ParseMessage decodes a single wire-format line into one or more
	// normalized Messages. A single wire line (e.g. Claude content blocks)
	// may produce multiple semantic messages.
	ParseMessage(line []byte) ([]Message, error)

	// Harness returns the harness identifier ("claude", "gemini", etc.)
	Harness() Harness

	// Models returns the list of model names supported by this backend.
	Models() []string

	// SupportsImages reports whether this backend accepts image content blocks.
	SupportsImages() bool

	// SupportsCompact reports whether this backend supports context compaction.
	SupportsCompact() bool

	// ContextWindowLimit returns the API prompt token limit for the given model.
	// The model parameter is the model name reported by the agent at runtime.
	ContextWindowLimit(model string) int
}

// Base provides default implementations for most Backend methods. Embed it in
// backend-specific types to inherit the boilerplate. Each backend must provide
// its own Start method.
type Base struct {
	HarnessID     Harness
	ModelList     []string
	Images        bool
	ContextWindow int
	Wire          WireFormat                      // Used by StartRelay and AttachRelay.
	Parse         func([]byte) ([]Message, error) // Used by ParseMessage and ReadRelayOutput.
}

// Harness implements Backend.
func (b *Base) Harness() Harness { return b.HarnessID }

// Models implements Backend.
func (b *Base) Models() []string { return b.ModelList }

// SupportsImages implements Backend.
func (b *Base) SupportsImages() bool { return b.Images }

// SupportsCompact implements Backend by checking if Wire implements CompactCommand.
func (b *Base) SupportsCompact() bool {
	_, ok := b.Wire.(CompactCommand)
	return ok
}

// ContextWindowLimit implements Backend.
func (b *Base) ContextWindowLimit(string) int { return b.ContextWindow }

// ParseMessage implements Backend by delegating to Parse.
func (b *Base) ParseMessage(line []byte) ([]Message, error) { return b.Parse(line) }

// ReadRelayOutput implements Backend.
func (b *Base) ReadRelayOutput(ctx context.Context, container string) ([]Message, int64, error) {
	return ReadRelayOutput(ctx, container, b.Parse)
}

// AttachRelay implements Backend.
func (b *Base) AttachRelay(ctx context.Context, opts *Options, msgCh chan<- Message, logW io.Writer) (*Session, error) {
	return AttachRelaySession(ctx, opts.Container, opts.RelayOffset, msgCh, logW, b.Wire)
}
