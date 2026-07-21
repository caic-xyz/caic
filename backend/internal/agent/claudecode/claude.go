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

// Backend implements agent.Backend for Claude Code.
type Backend struct {
	agent.Base

	widgetTracker                *WidgetTracker
	fieldWarner                  *jsonutil.FieldWarner
	pendingReasoningOutputTokens int
	pendingReasoningEstimate     int
}

// ParseMessage wraps ParseMessage with widget tracking for streaming deltas.
func (b *Backend) ParseMessage(line []byte) ([]agent.Message, error) {
	if estimate, ok := systemThinkingTokenEstimate(line); ok {
		b.pendingReasoningEstimate += estimate
	}
	msgs, err := parseMessageWithTracker(line, b.widgetTracker, b.fieldWarner)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.UsageMessage:
			b.pendingReasoningOutputTokens += m.Usage.ReasoningOutputTokens
		case *agent.ResultMessage:
			// Claude result records can omit output_tokens_details even when
			// preceding events reported thinking tokens. Prefer actual
			// message_delta usage over system/thinking_tokens estimates.
			if m.Usage.ReasoningOutputTokens == 0 {
				m.Usage.ReasoningOutputTokens = b.pendingReasoningOutputTokens
				if m.Usage.ReasoningOutputTokens == 0 {
					m.Usage.ReasoningOutputTokens = b.pendingReasoningEstimate
				}
			}
			b.pendingReasoningOutputTokens = 0
			b.pendingReasoningEstimate = 0
		}
	}
	return msgs, nil
}

var _ agent.Backend = (*Backend)(nil)

// New creates a Claude Code backend with wire format and parser configured.
func New() *Backend {
	b := &Backend{
		widgetTracker: NewWidgetTracker(),
		fieldWarner:   &jsonutil.FieldWarner{},
	}
	b.Base = agent.Base{
		HarnessID:     harness.Claude,
		ModelList:     []string{"opus", "sonnet", "haiku", "fable"},
		Efforts:       []string{claudecode.EffortLow, claudecode.EffortMedium, claudecode.EffortHigh, claudecode.EffortXHigh, claudecode.EffortMax},
		Images:        true,
		Compact:       true,
		ContextWindow: 180_000,
	}
	return b
}

// Wire is the wire format for Claude Code (stream-json over stdin/stdout).
var Wire agent.WireFormat = New()

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
	c := agent.Conn(&controlConn{Conn: agent.NewConn(rp.Stdin, opts.LogW, b)})
	return agent.StartSession(rp, c, opts)
}

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
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
	_, err = logW.Write(data)
	return err
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
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, b, func(c agent.Conn) (agent.Conn, error) {
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
	return &Backend{widgetTracker: NewWidgetTracker()}
}

// WriteCompact implements agent.CompactCommand by sending /compact as a user
// message. Claude Code recognizes this as a slash command in -p mode.
func (b *Backend) WriteCompact(w io.Writer, instructions string, logW io.Writer) error {
	text := "/compact"
	if instructions != "" {
		text = "/compact " + instructions
	}
	return b.WritePrompt(w, agent.Prompt{Text: text}, logW)
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
