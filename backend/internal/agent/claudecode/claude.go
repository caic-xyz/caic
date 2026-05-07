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
	// Temporarily disabled; I look at the traces and claude code switches midway authentication when it
	// receives the key. What a monster.
	// The only way to fix this is to open claude code while it's running and to manually deny it the use of the
	// API key.
	stripAndInject := false && hasOAuth()
	if stripAndInject {
		opts.StripEnv = []string{"ANTHROPIC_API_KEY"}
	}
	rp, err := agent.PrepareRelay(ctx, opts, buildArgs(opts))
	if err != nil {
		return nil, err
	}
	c := agent.NewConn(rp.Stdin, opts.LogW, b)
	if stripAndInject {
		c = &envInjectorConn{Conn: c}
	}
	return agent.StartSession(rp, c, opts)
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

// envInjectorConn wraps a Conn to re-inject environment variables that the
// relay stripped for OAuth authentication. The relay emits a
// caic_stripped_env event after the first subprocess output (system/init)
// which confirms auth succeeded. This conn intercepts the event and
// immediately sends an InputUpdateEnvVars message so tools and MCP servers
// can use the key.
type envInjectorConn struct {
	agent.Conn
}

func (c *envInjectorConn) ReadMessages(r io.Reader, msgCh chan<- agent.Message) error {
	proxy := make(chan agent.Message, 1)
	errc := make(chan error, 1)
	go func() {
		defer close(errc)
		for m := range proxy {
			if v, ok := m.(*agent.StrippedEnvMessage); ok {
				payload, err := json.Marshal(cc.InputUpdateEnvVarsMsg{
					Type:      cc.InputUpdateEnvVars,
					Variables: v.Variables,
				})
				if err != nil {
					errc <- fmt.Errorf("marshal InputUpdateEnvVars: %w", err)
					return
				}
				payload = append(payload, '\n')
				if err := c.SendRaw(payload); err != nil {
					errc <- fmt.Errorf("inject stripped env vars: %w", err)
					return
				}
				continue // Don't forward to consumer.
			}
			msgCh <- m
		}
	}()
	err := c.Conn.ReadMessages(r, proxy)
	close(proxy)
	if injErr := <-errc; injErr != nil {
		return errors.Join(err, injErr)
	}
	return err
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
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args
}
