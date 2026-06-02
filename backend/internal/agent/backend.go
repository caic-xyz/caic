// Defines the Backend interface that all coding-agent implementations must satisfy.

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
	// Start launches the agent in the given container. Parsed messages are
	// forwarded to opts.MsgCh; opts.LogW receives raw wire-format lines.
	Start(ctx context.Context, opts *Options) (*Session, error)

	// AttachRelay connects to an already-running relay daemon in the
	// container. opts.RelayOffset specifies the byte offset into
	// output.jsonl to replay from (use 0 for full replay).
	// opts.ResumeSessionID is the known agent session ID, used by stateful
	// wire formats (e.g. codex) that need it before the first replay message.
	AttachRelay(ctx context.Context, opts *Options) (*Session, error)

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

	// AgentArgs returns the CLI arguments for launching this backend's agent
	// subprocess, including the executable name and all session-specific flags
	// derived from a. Empty fields in a are treated as "use default".
	AgentArgs(a HarnessArgs) []string

	// NewWire creates a fresh WireFormat for this backend. Each call returns
	// independent state; suitable for use outside the normal container transport.
	NewWire() WireFormat

	// ContextWindowLimit returns the API prompt token limit for the given model.
	// The model parameter is the model name reported by the agent at runtime.
	ContextWindowLimit(model string) int
}

// ModelFetcher is an optional backend capability for discovering available models.
type ModelFetcher interface {
	FetchModels(ctx context.Context, instance string, env []string) ([]string, error)
}

// HarnessArgs holds the session-specific parameters that influence the CLI
// arguments passed to an agent subprocess.
type HarnessArgs struct {
	Model           string // Model name or alias; empty means use backend default.
	Effort          string // Thinking effort level (e.g. "low", "high"); empty means default.
	ResumeSessionID string // Agent session ID to resume; empty starts a new session.
}

// PrePromptWriter is implemented by backends that send initialization
// commands to stdin before the first user prompt (e.g. Pi's set_model).
type PrePromptWriter interface {
	WritePrePrompt(w io.Writer, model string, logW io.Writer) error
}

// RecordHandshaker is implemented by backends that perform a bidirectional
// handshake over stdin/stdout before writing prompts (e.g. ACP-based agents
// like OpenCode). The returned io.Reader replaces the original stdout for
// subsequent reads (it may be a buffered reader that consumed bytes beyond
// the handshake response).
type RecordHandshaker interface {
	RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.Reader, model string) (WireFormat, io.Reader, error)
}

// Base provides default implementations for metadata-only Backend methods.
// Embed it in backend-specific types to inherit the boilerplate. Each backend
// must implement Start and AttachRelay itself using the package-level helpers
// (StartRelay, AttachRelaySession).
type Base struct {
	HarnessID     Harness
	ModelList     []string
	Images        bool
	ContextWindow int
	Compact       bool
}

// Harness implements Backend.
func (b *Base) Harness() Harness { return b.HarnessID }

// Models implements Backend.
func (b *Base) Models() []string { return b.ModelList }

// SetModels implements Backend.
func (b *Base) SetModels(models []string) { b.ModelList = models }

// SupportsImages implements Backend.
func (b *Base) SupportsImages() bool { return b.Images }

// SupportsCompact implements Backend.
func (b *Base) SupportsCompact() bool { return b.Compact }

// ContextWindowLimit implements Backend.
func (b *Base) ContextWindowLimit(string) int { return b.ContextWindow }
