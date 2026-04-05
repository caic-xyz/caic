// Package claudecode implements agent.Backend for Claude Code.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
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
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	pluginFS, err := fs.Sub(WidgetPlugin, "widget-plugin")
	if err != nil {
		return nil, fmt.Errorf("widget plugin fs: %w", err)
	}
	if err := agent.DeployEmbeddedDir(ctx, opts.Container, pluginFS, agent.WidgetPluginDir); err != nil {
		return nil, err
	}
	sess, err := agent.StartRelay(ctx, opts, buildArgs(opts), msgCh, logW, b)
	if err != nil {
		return nil, err
	}
	// The relay strips ANTHROPIC_API_KEY from the subprocess environment so
	// Claude Code authenticates via OAuth. Re-inject it after auth completes
	// so tools (Bash, MCP servers) can still use it.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		msg := InputUpdateEnvVarsMsg{
			Type:      InputUpdateEnvVars,
			Variables: map[string]string{"ANTHROPIC_API_KEY": key},
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal env vars: %w", err)
		}
		data = append(data, '\n')
		if err := sess.SendRaw(data); err != nil {
			return nil, fmt.Errorf("send env vars: %w", err)
		}
	}
	return sess, nil
}

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	var blocks []InputContentBlock
	for _, img := range p.Images {
		blocks = append(blocks, InputContentBlock{
			Type: "image",
			Source: InputImageSource{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      img.Data,
			},
		})
	}
	if p.Text != "" {
		blocks = append(blocks, InputContentBlock{Type: "text", Text: p.Text})
	}
	msg := InputUserMsg{
		Type:    InputUser,
		Message: InputUserContent{Role: "user", Content: blocks},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
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
