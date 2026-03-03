// Package claude implements agent.Backend for Claude Code.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Claude Code.
type Backend struct{}

var _ agent.Backend = (*Backend)(nil)

// Wire is the wire format for Claude Code (stream-json over stdin/stdout).
var Wire agent.WireFormat = &Backend{}

// Harness returns the harness identifier.
func (b *Backend) Harness() agent.Harness { return agent.Claude }

// Models returns the model aliases supported by Claude Code CLI.
//
// TODO: Figure out a way to generate this list at runtime.
func (b *Backend) Models() []string { return []string{"opus", "sonnet", "haiku"} }

// SupportsImages reports that Claude Code accepts image content blocks.
func (b *Backend) SupportsImages() bool { return true }

// ContextWindowLimit returns the API prompt token limit for Claude models.
func (b *Backend) ContextWindowLimit(model string) int { return 180_000 }

// Start launches a Claude Code process via the relay daemon in the given
// container. It deploys the relay script and starts claude via serve-attach.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	claudeArgs := buildArgs(opts)

	// Build the ssh command: ssh <container> python3 relay.py serve-attach --dir <dir> -- claude ...
	sshArgs := make([]string, 0, 7+len(claudeArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--")
	sshArgs = append(sshArgs, claudeArgs...)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Prefix: "relay serve-attach", Container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	log := slog.With("container", opts.Container)
	s := agent.NewSession(cmd, stdin, stdout, msgCh, logW, Wire, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, container string, offset int64, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, container, offset, msgCh, logW, Wire)
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses it into Messages.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	return agent.ReadRelayOutput(ctx, container, agent.ParseMessage)
}

// ParseMessage decodes a single Claude Code NDJSON line into a typed Message.
func (b *Backend) ParseMessage(line []byte) (agent.Message, error) {
	return agent.ParseMessage(line)
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
