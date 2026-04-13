// Package claudecode implements agent.Backend for Claude Code.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	cc "github.com/maruel/genai/providers/claudecode"
)

// Backend implements agent.Backend for Claude Code.
type Backend struct {
	agent.Base
	widgetTracker *WidgetTracker
	fieldWarner   *jsonutil.FieldWarner
}

// ParseMessage wraps ParseMessage with widget tracking for streaming deltas.
func (b *Backend) ParseMessage(line []byte) ([]agent.Message, error) {
	return parseMessageWithTracker(line, b.widgetTracker, b.fieldWarner)
}

var _ agent.Backend = (*Backend)(nil)

// NewParser implements agent.Backend.
func (*Backend) NewParser() func([]byte) ([]agent.Message, error) {
	fw := &jsonutil.FieldWarner{}
	return func(line []byte) ([]agent.Message, error) { return parseMessage(line, fw) }
}

// New creates a Claude Code backend with wire format and parser configured.
func New() *Backend {
	b := &Backend{
		widgetTracker: NewWidgetTracker(),
		fieldWarner:   &jsonutil.FieldWarner{},
	}
	b.Base = agent.Base{
		HarnessID:     agent.Claude,
		ModelList:     []string{"opus", "sonnet", "haiku"},
		Images:        true,
		ContextWindow: 180_000,
	}
	b.Wire = b
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
	if err := agent.DeployEmbeddedDir(ctx, opts.Container, pluginFS, agent.WidgetPluginDir); err != nil {
		return nil, err
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return agent.StartRelay(ctx, opts, buildArgs(opts), b)
	}
	// The relay strips ANTHROPIC_API_KEY when OAuth is configured so Claude
	// Code authenticates via OAuth. Re-inject it via InputUpdateEnvVars after
	// the first message — at that point OAuth authentication is complete — so
	// tools and MCP servers can use the key without affecting Claude Code's
	// own billing method.
	envMsg, err := json.Marshal(cc.InputUpdateEnvVarsMsg{
		Type:      cc.InputUpdateEnvVars,
		Variables: map[string]string{"ANTHROPIC_API_KEY": apiKey},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal env vars: %w", err)
	}
	envMsg = append(envMsg, '\n')
	var sess *agent.Session
	startOpts := *opts
	origDispatch := opts.Dispatch
	first := true
	startOpts.Dispatch = func(m agent.Message) {
		if first {
			first = false
			if err := sess.SendRaw(envMsg); err != nil {
				slog.Warn("inject ANTHROPIC_API_KEY", "err", err)
			}
		}
		origDispatch(m)
	}
	sess, err = agent.StartRelay(ctx, &startOpts, buildArgs(opts), b)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	var blocks []cc.InputContentBlock
	for _, img := range p.Images {
		blocks = append(blocks, cc.InputContentBlock{
			Type: "image",
			Source: cc.InputImageSource{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      img.Data,
			},
		})
	}
	if p.Text != "" {
		blocks = append(blocks, cc.InputContentBlock{Type: "text", Text: p.Text})
	}
	msg := cc.InputUserMsg{
		Type:    cc.InputUser,
		Message: cc.InputUserContent{Role: "user", Content: blocks},
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

// AttachRelay implements agent.Backend.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, b)
}

// ReadRelayOutput implements agent.Backend.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	return agent.ReadRelayOutput(ctx, container, b.ParseMessage)
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

// buildArgs constructs the Claude Code CLI arguments.
func buildArgs(opts *agent.Options) []string {
	args := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--include-partial-messages",
		"--plugin-dir", agent.WidgetPluginDir,
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args
}
