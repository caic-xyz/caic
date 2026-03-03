// Package gemini implements agent.Backend for Gemini CLI.
package gemini

import (
	"context"
	"io"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Gemini CLI.
type Backend struct {
	agent.Base
}

var _ agent.Backend = (*Backend)(nil)

// New creates a Gemini CLI backend with wire format and parser configured.
func New() *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     agent.Gemini,
		ModelList:     []string{"gemini-3.1-pro", "gemini-3-flash"},
		ContextWindow: 1_000_000,
		Parse:         ParseMessage,
	}
	b.Wire = b
	return b
}

// Start launches a Gemini CLI process via the relay daemon.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	return agent.StartRelay(ctx, opts, buildArgs(opts), msgCh, logW, b)
}

// WritePrompt writes a single user message to Gemini CLI's stdin.
// Gemini CLI in -p mode reads plain text lines from stdin. Images are ignored.
func (*Backend) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	return agent.PlainTextWritePrompt(w, p, logW)
}

// buildArgs constructs the Gemini CLI arguments.
func buildArgs(opts *agent.Options) []string {
	args := []string{
		"gemini", "-p",
		"--output-format", "stream-json",
		"--yolo",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	return args
}
