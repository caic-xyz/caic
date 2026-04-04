// Package kilo implements agent.Backend for Kilo Code.
//
// Kilo Code exposes an HTTP+SSE API via `kilo serve`. A Python bridge script
// (bridge.py) runs as the relay subprocess, handling process management, SSE
// I/O, permission auto-approval, and deduplication. It emits native kilo SSE
// event JSON on stdout, which parseMessage translates into agent.Message types.
//
// Per-session state (accumulated step cost/usage, turn-close terminal event)
// is managed by kiloWireFormat, which wraps the stateless parseMessage function.
// A fresh kiloWireFormat is created for every Start, AttachRelay, and
// ReadRelayOutput call so that accumulators reset between sessions and replays.
package kilo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

const bridgeScriptPath = agent.RelayDir + "/kilo_bridge.py"

// Backend implements agent.Backend for Kilo Code.
type Backend struct {
	agent.Base
	modelsMu sync.RWMutex
}

var _ agent.Backend = (*Backend)(nil)

// NewParser implements agent.Backend.
func (*Backend) NewParser() func([]byte) ([]agent.Message, error) {
	fw := &jsonutil.FieldWarner{}
	return func(line []byte) ([]agent.Message, error) { return parseMessage(line, fw) }
}

var defaultModels = []string{
	"anthropic/claude-opus-4.6",
	"anthropic/claude-sonnet-4.6",
	"google/gemini-3.1-pro-preview",
	"google/gemini-3-flash-preview",
	"openai/gpt-5.3-codex",
}

// New creates a Kilo Code backend with wire format and parser configured.
// Models default to a small hardcoded list; call SetModels to update
// asynchronously after discovery.
func New() *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     agent.Kilo,
		ModelList:     defaultModels,
		ContextWindow: 200_000,
	}
	return b
}

// Models returns the available model list (thread-safe).
func (b *Backend) Models() []string {
	b.modelsMu.RLock()
	defer b.modelsMu.RUnlock()
	return b.ModelList
}

// SetModels replaces the model list (thread-safe).
func (b *Backend) SetModels(models []string) {
	b.modelsMu.Lock()
	defer b.modelsMu.Unlock()
	b.ModelList = models
}

// Start deploys relay and bridge scripts, then launches via relay serve-attach.
// A fresh kiloWireFormat is used so per-session accumulators start at zero.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if err := deployBridge(ctx, opts.Container); err != nil {
		return nil, err
	}
	return agent.StartRelay(ctx, opts, buildBridgeArgs(opts), msgCh, logW, &kiloWireFormat{fw: &jsonutil.FieldWarner{}})
}

// AttachRelay connects to an already-running relay using a fresh kiloWireFormat
// so that accumulated state from a prior session does not bleed in.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts.Container, opts.RelayOffset, msgCh, logW, &kiloWireFormat{fw: &jsonutil.FieldWarner{}})
}

// ReadRelayOutput reads output.jsonl using a fresh kiloWireFormat so that
// step-finish messages are correctly converted to UsageMessages and the
// turn.close event produces the terminal ResultMessage.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	return agent.ReadRelayOutput(ctx, container, (&kiloWireFormat{fw: &jsonutil.FieldWarner{}}).ParseMessage)
}

// WritePrompt writes a single user message to the bridge's stdin.
// The bridge reads plain text lines (like Gemini CLI).
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	return agent.PlainTextWritePrompt(w, p, logW)
}

// deployBridge uploads the bridge script into the container. Idempotent.
func deployBridge(ctx context.Context, container string) error {
	cmd := exec.CommandContext(ctx, "ssh", container, //nolint:gosec // container is not user-controlled
		"mkdir -p "+agent.RelayDir+" && cat > "+bridgeScriptPath)
	cmd.Stdin = bytes.NewReader(BridgeScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy kilo bridge: %w: %s", err, out)
	}
	return nil
}

// buildBridgeArgs constructs the command to run the bridge script.
func buildBridgeArgs(opts *agent.Options) []string {
	args := []string{"python3", "-u", bridgeScriptPath}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return args
}

// kiloWireFormat is a stateful WireFormat for kilo sessions. It wraps the
// stateless parseMessage function, intercepting three event types:
//
//   - message.part.updated: records partID → partType so that subsequent deltas
//     for reasoning parts can be routed to ThinkingDeltaMessage.
//
//   - message.part.delta for a known reasoning part: emitted as ThinkingDeltaMessage
//     instead of TextDeltaMessage so the UI streams thinking blocks correctly.
//
//   - step-finish ResultMessage (IsError=false): accumulated into totalCostUSD
//     and totalUsage, then re-emitted as a UsageMessage so the UI can show
//     per-step cost without treating each step as a terminal event.
//
//   - session.turn.close RawMessage: converted to the actual terminal
//     ResultMessage, carrying the accumulated cost and usage for the turn.
//     "error" turn-closes are suppressed (session.error already sent the result).
//
// State resets after each turn-close so multi-turn kilo sessions accumulate
// correctly per prompt.
type kiloWireFormat struct {
	mu           sync.Mutex
	totalCostUSD float64
	totalUsage   agent.Usage
	errorSeen    bool              // true after a session.error ResultMessage
	partTypes    map[string]string // partID → partType for delta routing
	fw           *jsonutil.FieldWarner
}

// WritePrompt implements agent.WireFormat.
func (w *kiloWireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	return agent.PlainTextWritePrompt(wr, p, logW)
}

// ParseMessage implements agent.WireFormat. It delegates to the stateless
// parseMessage function and post-processes step-finish, turn-close, and
// reasoning delta events.
func (w *kiloWireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	// Pre-pass: record part types from message.part.updated so deltas can be
	// routed correctly. We decode lazily here since most lines aren't part events.
	var rec Record
	if jsonErr := json.Unmarshal(line, &rec); jsonErr == nil {
		switch rec.Type {
		case TypePartUpdated:
			if pu, err := rec.AsPartUpdated(); err == nil {
				id, pt := pu.Properties.Part.ID, pu.Properties.Part.Type
				if id != "" && pt != "" {
					w.mu.Lock()
					if w.partTypes == nil {
						w.partTypes = make(map[string]string)
					}
					w.partTypes[id] = pt
					w.mu.Unlock()
				}
			}
		case TypePartDelta:
			if pd, err := rec.AsPartDelta(); err == nil && pd.Properties.Delta != "" {
				w.mu.Lock()
				pt := w.partTypes[pd.Properties.PartID]
				w.mu.Unlock()
				if pt == PartTypeReasoning {
					return []agent.Message{&agent.ThinkingDeltaMessage{Text: pd.Properties.Delta}}, nil
				}
			}
		}
	}

	msgs, err := parseMessage(line, w.fw)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Message, 0, len(msgs))
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.ResultMessage:
			if m.IsError {
				// session.error — pass through and mark the turn as errored so
				// the subsequent turn.close does not emit a duplicate result.
				w.mu.Lock()
				w.errorSeen = true
				w.mu.Unlock()
				out = append(out, m)
			} else {
				// step-finish — accumulate cost/usage and emit as UsageMessage
				// so the UI sees per-step token counts without treating this as
				// the terminal event.
				w.mu.Lock()
				w.totalCostUSD += m.TotalCostUSD
				w.totalUsage.InputTokens += m.Usage.InputTokens
				w.totalUsage.OutputTokens += m.Usage.OutputTokens
				w.totalUsage.CacheCreationInputTokens += m.Usage.CacheCreationInputTokens
				w.totalUsage.CacheReadInputTokens += m.Usage.CacheReadInputTokens
				w.totalUsage.ReasoningOutputTokens += m.Usage.ReasoningOutputTokens
				w.mu.Unlock()
				out = append(out, &agent.UsageMessage{Usage: m.Usage})
			}
		case *agent.RawMessage:
			if m.MessageType != TypeTurnClose {
				out = append(out, m)
				continue
			}
			// Decode the close reason from the raw bytes.
			var rec TurnCloseRecord
			if err := json.Unmarshal(m.Raw, &rec); err != nil {
				// Malformed close — pass through as-is.
				out = append(out, m)
				continue
			}
			w.mu.Lock()
			errSeen := w.errorSeen
			cost := w.totalCostUSD
			usage := w.totalUsage
			// Reset accumulators for the next turn.
			w.totalCostUSD = 0
			w.totalUsage = agent.Usage{}
			w.errorSeen = false
			w.mu.Unlock()
			switch rec.Properties.Reason {
			case "completed":
				if !errSeen {
					out = append(out, &agent.ResultMessage{
						MessageType:  "result",
						Subtype:      "result",
						TotalCostUSD: cost,
						Usage:        usage,
					})
				}
			case "interrupted":
				out = append(out, &agent.ResultMessage{
					MessageType:  "result",
					Subtype:      "interrupted",
					IsError:      true,
					TotalCostUSD: cost,
					Usage:        usage,
				})
			case "error":
				// session.error already sent the ResultMessage; suppress.
			}
		default:
			out = append(out, m)
		}
	}
	return out, nil
}
