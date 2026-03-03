// Package claude implements agent.Backend for Claude Code.
package claude

import (
	"context"
	"encoding/json"
	"io"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Claude Code.
type Backend struct {
	agent.Base
}

var _ agent.Backend = (*Backend)(nil)

// New creates a Claude Code backend with wire format and parser configured.
func New() *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     agent.Claude,
		ModelList:     []string{"opus", "sonnet", "haiku"},
		Images:        true,
		ContextWindow: 180_000,
		Parse:         ParseMessage,
	}
	b.Wire = b
	return b
}

// Wire is the wire format for Claude Code (stream-json over stdin/stdout).
var Wire agent.WireFormat = New()

// Start launches a Claude Code process via the relay daemon.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	return agent.StartRelay(ctx, opts, buildArgs(opts), msgCh, logW, b)
}

// userInputMessage is the NDJSON message sent to Claude Code via stdin.
type userInputMessage struct {
	Type    string           `json:"type"`
	Message userInputContent `json:"message"`
}

type userInputContent struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentBlock
}

// contentBlock is a single block in the content array sent to Claude Code.
type contentBlock struct {
	Type   string       `json:"type"`
	Source *imageSource `json:"source,omitempty"`
	Text   string       `json:"text,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	var content any
	if len(p.Images) == 0 {
		content = p.Text
	} else {
		blocks := make([]contentBlock, 0, len(p.Images)+1)
		for _, img := range p.Images {
			blocks = append(blocks, contentBlock{
				Type: "image",
				Source: &imageSource{
					Type:      "base64",
					MediaType: img.MediaType,
					Data:      img.Data,
				},
			})
		}
		if p.Text != "" {
			blocks = append(blocks, contentBlock{Type: "text", Text: p.Text})
		}
		content = blocks
	}
	msg := userInputMessage{
		Type:    "user",
		Message: userInputContent{Role: "user", Content: content},
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

// buildArgs constructs the Claude Code CLI arguments.
func buildArgs(opts *agent.Options) []string {
	args := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--include-partial-messages",
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args
}
