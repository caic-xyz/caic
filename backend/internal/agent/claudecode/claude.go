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

	"github.com/maruel/genai/providers/claudecode"

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
		Compact:       true,
		ContextWindow: 180_000,
	}
	return b
}

// ExportDiscussion reads a JSONL log and returns the conversation as markdown.
func (b *Backend) ExportDiscussion(path string) (string, error) {
	return agent.ExportDiscussion(path, b.NewWire().ParseMessage)
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
	args := b.AgentArgs(agent.HarnessArgs{Model: opts.Model, Effort: opts.Effort, ResumeSessionID: opts.ResumeSessionID})
	rp, err := agent.PrepareRelay(ctx, opts, args)
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
	var blocks []claudecode.InputContentBlock
	for _, img := range p.Images {
		blocks = append(blocks, claudecode.InputContentBlock{
			Type: "image",
			Source: claudecode.InputImageSource{
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
		"--dangerously-skip-permissions",
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
	return agent.AttachRelaySession(ctx, opts, b)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	return New()
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
				payload, err := json.Marshal(claudecode.InputUpdateEnvVarsMsg{
					Type:      claudecode.InputUpdateEnvVars,
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
