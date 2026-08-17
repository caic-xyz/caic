// Package claudecode implements agent.Backend for Claude Code.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/maruel/genai/providers/anthropic"
	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// Backend implements agent.Backend for Claude Code. It is registered once per
// process (see backends.Default) and shared across every concurrent Claude
// Code task, so it must hold no per-session mutable state. Start, AttachRelay,
// and NewWire each construct a fresh wireFormat instead.
type Backend struct {
	agent.Base
}

var _ agent.Backend = (*Backend)(nil)

// wireFormat holds per-session Claude Code parsing state: widget tracking,
// unknown-field warnings, and reasoning-token accounting. A fresh instance is
// created for every session so this state can't leak between concurrent
// Claude Code tasks sharing the registered Backend singleton.
type wireFormat struct {
	widgetTracker                *WidgetTracker
	fieldWarner                  *jsonutil.FieldWarner
	pendingReasoningOutputTokens int
	pendingReasoningEstimate     int
}

var _ agent.WireFormat = (*wireFormat)(nil)
var _ agent.CompactCommand = (*wireFormat)(nil)

// newWireFormat builds a live wireFormat with a fresh widget tracker and
// field warner, for use by Start and AttachRelay.
func newWireFormat() *wireFormat {
	return &wireFormat{
		widgetTracker: NewWidgetTracker(),
		fieldWarner:   &jsonutil.FieldWarner{},
	}
}

// ParseMessage wraps ParseMessage with widget tracking for streaming deltas.
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	if estimate, ok := systemThinkingTokenEstimate(line); ok {
		w.pendingReasoningEstimate += estimate
	}
	msgs, err := parseMessageWithTracker(line, w.widgetTracker, w.fieldWarner)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.UsageMessage:
			w.pendingReasoningOutputTokens += m.Usage.ReasoningOutputTokens
		case *agent.ResultMessage:
			// Claude result records can omit output_tokens_details even when
			// preceding events reported thinking tokens. Prefer actual
			// message_delta usage over system/thinking_tokens estimates.
			if m.Usage.ReasoningOutputTokens == 0 {
				m.Usage.ReasoningOutputTokens = w.pendingReasoningOutputTokens
				if m.Usage.ReasoningOutputTokens == 0 {
					m.Usage.ReasoningOutputTokens = w.pendingReasoningEstimate
				}
			}
			w.pendingReasoningOutputTokens = 0
			w.pendingReasoningEstimate = 0
		}
	}
	return msgs, nil
}

var claudeEffortOptions = []string{
	claudecode.EffortLow,
	claudecode.EffortMedium,
	claudecode.EffortHigh,
	claudecode.EffortXHigh,
	claudecode.EffortMax,
}

// New creates a Claude Code backend descriptor.
func New() *Backend {
	b := &Backend{
		Base: agent.Base{
			HarnessID:     harness.Claude,
			Images:        true,
			Compact:       true,
			ContextWindow: 180_000,
		},
	}
	b.SetModelInventory(claudeModelInventory())
	return b
}

func claudeModelInventory() agent.ModelInventory {
	models := [...]string{"fable", "opus", "sonnet", "haiku"}
	inventory := make([]agent.Model, 0, len(models))
	for _, model := range models {
		inventory = append(inventory, agent.Model{
			ID:            model,
			EffortOptions: append([]string(nil), claudeEffortOptions...),
		})
	}
	return agent.ModelInventory{Models: inventory}
}

// Start launches a Claude Code process via the relay daemon. It deploys the
// widget plugin to the container before starting the relay so Claude Code
// picks up the show_widget MCP tool and the widget design skill.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	pluginFS, err := fs.Sub(WidgetPlugin, "widget-plugin")
	if err != nil {
		return nil, fmt.Errorf("widget plugin fs: %w", err)
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if err := agent.DeployEmbeddedDir(ctx, sshHost, pluginFS, agent.WidgetPluginDir); err != nil {
		return nil, err
	}
	// When Claude Code has an OAuth session, strip ANTHROPIC_API_KEY from the
	// agent subprocess so it authenticates via the subscription instead of
	// silently billing API credits. The key is deliberately NOT re-injected
	// afterward: re-injecting it made Claude Code switch authentication to API
	// billing mid-session (observed in traces). The tradeoff is that
	// in-container tools and MCP servers do not receive ANTHROPIC_API_KEY.
	if hasOAuth() {
		opts.StripEnv = []string{"ANTHROPIC_API_KEY"}
	}
	args := b.AgentArgs(agent.HarnessArgs{Model: opts.Model, Effort: opts.Effort, ResumeSessionID: opts.ResumeSessionID})
	rp, err := agent.PrepareRelay(ctx, opts, args)
	if err != nil {
		return nil, err
	}
	c := agent.Conn(&controlConn{Conn: agent.NewConn(ctx, opts.Logger, rp.Stdin, opts.Log, newWireFormat())})
	return agent.StartSession(ctx, rp, c, opts)
}

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*wireFormat) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
	var blocks []claudecode.InputContentBlock
	for _, img := range p.Images {
		blocks = append(blocks, claudecode.InputContentBlock{
			Type: "image",
			Source: anthropic.Source{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      img.Data,
			},
		})
	}
	if p.Text != "" {
		blocks = append(blocks, claudecode.InputContentBlock{Type: "text", Text: p.Text})
	}
	msg := claudecode.InputUserMsg{
		Type:    claudecode.InputUser,
		Message: claudecode.InputUserContent{Role: "user", Content: blocks},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	return agent.AppendNativeRecord(log, log.LogVersion(), data)
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(a agent.HarnessArgs) []string {
	args := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--plugin-dir", agent.WidgetPluginDir,
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.ResumeSessionID != "" {
		args = append(args, "--resume", a.ResumeSessionID)
	}
	return args
}

// AttachRelay implements agent.Backend.
func (*Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, newWireFormat(), func(c agent.Conn) (agent.Conn, error) {
		cc := &controlConn{Conn: c}
		if err := cc.restorePendingActions(opts.PendingUserActions); err != nil {
			return nil, err
		}
		return cc, nil
	})
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Log replay can parse large histories; skip development-only unknown-field scans.
	return &wireFormat{widgetTracker: NewWidgetTracker()}
}

// WriteCompact implements agent.CompactCommand by sending /compact as a user
// message. Claude Code recognizes this as a slash command in -p mode.
func (w *wireFormat) WriteCompact(wr io.Writer, instructions string, log agent.LogSink) error {
	text := "/compact"
	if instructions != "" {
		text = "/compact " + instructions
	}
	return w.WritePrompt(wr, agent.Prompt{Text: text}, log)
}

// hasOAuth reports whether Claude Code has an OAuth session configured
// in ~/.claude/claude.json. When true, ANTHROPIC_API_KEY should be
// stripped so Claude Code authenticates via OAuth instead of API key.
func hasOAuth() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "claude.json")) //nolint:gosec // Path components are hardcoded.
	if err != nil {
		return false
	}
	// Quick key check avoids unmarshaling the full config.
	var m map[string]json.RawMessage
	return json.Unmarshal(data, &m) == nil && m["oauthAccount"] != nil
}
