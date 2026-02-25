// Package codex implements agent.Backend for Codex CLI.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Codex CLI using the app-server
// JSON-RPC 2.0 protocol.
type Backend struct{}

var _ agent.Backend = (*Backend)(nil)

// Harness returns the harness identifier.
func (b *Backend) Harness() agent.Harness { return agent.Codex }

// Models returns the model names supported by Codex CLI.
//
// TODO: Figure out a way to generate this list at runtime.
func (b *Backend) Models() []string {
	return []string{
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
	}
}

// SupportsImages reports that Codex CLI does not accept image input.
func (b *Backend) SupportsImages() bool { return false }

// ContextWindowLimit returns the API prompt token limit for Codex models.
func (b *Backend) ContextWindowLimit(model string) int { return 200_000 }

// Start launches a Codex CLI app-server process via the relay daemon in the
// given container. It performs the JSON-RPC handshake (initialize →
// initialized → thread/start) before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	codexArgs := buildArgs(opts)

	sshArgs := make([]string, 0, 8+len(codexArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin", "--")
	sshArgs = append(sshArgs, codexArgs...)

	slog.Info("codex: launching via relay", "container", opts.Container, "args", codexArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &slogWriter{prefix: "relay serve-attach", container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(stdout, 1<<16)

	wire, err := handshake(stdin, br, opts)
	if err != nil {
		// Kill the process on handshake failure.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("codex handshake: %w", err)
	}

	log := slog.With("container", opts.Container)
	s := agent.NewSession(cmd, stdin, br, msgCh, logW, wire, log)
	if opts.InitialPrompt.Text != "" {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, container string, offset int64, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	sshArgs := []string{
		container, "python3", agent.RelayScriptPath, "attach",
		"--offset", strconv.FormatInt(offset, 10),
	}
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &slogWriter{prefix: "relay attach", container: container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	// On reconnect, thread ID is unknown until we see thread/started in the
	// replayed output. wireFormat.ParseMessage captures it.
	wire := &wireFormat{}

	log := slog.With("container", container)
	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, wire, log), nil
}

// ReadRelayOutput reads the complete output.jsonl from the container's relay
// and parses it into Messages.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) (msgs []agent.Message, size int64, err error) {
	cmd := exec.CommandContext(ctx, "ssh", container, "cat", agent.RelayOutputPath) //nolint:gosec // args are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("read relay output: %w", err)
	}
	size = int64(len(out))
	scanner := bufio.NewScanner(bytes.NewReader(out))
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, parseErr := b.ParseMessage(line)
		if parseErr != nil {
			slog.Warn("skipping unparseable relay output line", "container", container, "err", parseErr)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, size, scanner.Err()
}

// ParseMessage decodes a single Codex CLI app-server line into a typed Message.
func (b *Backend) ParseMessage(line []byte) (agent.Message, error) {
	return ParseMessage(line)
}

// wireFormat implements agent.WireFormat for the codex app-server JSON-RPC
// protocol. It holds per-session state: the thread ID, a request ID counter,
// and the most recent token usage from thread/tokenUsage/updated.
type wireFormat struct {
	threadID  string
	nextID    atomic.Int64
	mu        sync.Mutex
	lastUsage agent.Usage // accumulated from thread/tokenUsage/updated
}

// WritePrompt sends a turn/start JSON-RPC request to begin a new turn with
// the given user message. Images are ignored (Codex does not support them).
func (w *wireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threadID == "" {
		return errors.New("codex: no thread ID (handshake not completed)")
	}
	id := w.nextID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "turn/start",
		"params": map[string]any{
			"threadId": w.threadID,
			"input":    []map[string]any{{"type": "text", "text": p.Text}},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := wr.Write(data); err != nil {
		return err
	}
	// Don't log to logW — stdin is not logged with --no-log-stdin.
	return nil
}

// ParseMessage wraps the package-level ParseMessage, intercepting
// thread/tokenUsage/updated to accumulate usage and injecting it into
// ResultMessage. It also captures the thread ID from thread/started.
func (w *wireFormat) ParseMessage(line []byte) (agent.Message, error) {
	// Intercept thread/tokenUsage/updated before general parsing so we can
	// accumulate usage without adding a new agent.Message type.
	var probe struct {
		Method string `json:"method,omitempty"`
	}
	_ = json.Unmarshal(line, &probe)
	if probe.Method == MethodTokenUsageUpdated {
		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err == nil {
			var p TokenUsageUpdatedParams
			if err := json.Unmarshal(msg.Params, &p); err == nil {
				w.mu.Lock()
				w.lastUsage = agent.Usage{
					InputTokens:          int(p.TokenUsage.Last.InputTokens),
					CacheReadInputTokens: int(p.TokenUsage.Last.CachedInputTokens),
					OutputTokens:         int(p.TokenUsage.Last.OutputTokens),
				}
				w.mu.Unlock()
			}
		}
		return &agent.RawMessage{MessageType: MethodTokenUsageUpdated, Raw: append([]byte(nil), line...)}, nil
	}

	msg, err := ParseMessage(line)
	if err != nil {
		return nil, err
	}
	// Capture thread ID from SystemInitMessage (produced by thread/started).
	if init, ok := msg.(*agent.SystemInitMessage); ok && init.SessionID != "" {
		w.mu.Lock()
		w.threadID = init.SessionID
		w.mu.Unlock()
	}
	// Inject accumulated usage into ResultMessage (turn/completed has no usage in v2).
	if rm, ok := msg.(*agent.ResultMessage); ok {
		w.mu.Lock()
		rm.Usage = w.lastUsage
		w.mu.Unlock()
	}
	return msg, nil
}

// handshake performs the JSON-RPC initialize → initialized → thread/start
// (or thread/resume) sequence and returns a wireFormat with the thread ID set.
func handshake(stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*wireFormat, error) {
	w := &wireFormat{}

	// 1. Send initialize request.
	initID := w.nextID.Add(1)
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      initID,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "caic",
				"version": "1.0.0",
			},
			"capabilities": map[string]any{},
		},
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	if _, err := readJSONRPCResponse(stdout); err != nil {
		return nil, fmt.Errorf("read initialize response: %w", err)
	}

	// 2. Send initialized notification.
	initdNotif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}
	if err := writeJSON(stdin, initdNotif); err != nil {
		return nil, fmt.Errorf("write initialized: %w", err)
	}

	// 3. Send thread/start or thread/resume.
	threadID := w.nextID.Add(1)
	var threadReq map[string]any
	if opts.ResumeSessionID != "" {
		threadReq = map[string]any{
			"jsonrpc": "2.0",
			"id":      threadID,
			"method":  "thread/resume",
			"params": map[string]any{
				"threadId": opts.ResumeSessionID,
			},
		}
	} else {
		params := map[string]any{}
		if opts.Model != "" {
			params["model"] = opts.Model
		}
		threadReq = map[string]any{
			"jsonrpc": "2.0",
			"id":      threadID,
			"method":  "thread/start",
			"params":  params,
		}
	}
	if err := writeJSON(stdin, threadReq); err != nil {
		return nil, fmt.Errorf("write thread/start: %w", err)
	}

	// Read thread/start response — contains the thread info.
	resp, err := readJSONRPCResponse(stdout)
	if err != nil {
		return nil, fmt.Errorf("read thread/start response: %w", err)
	}

	// Extract thread ID from the response result.
	var result struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse thread/start result: %w", err)
	}
	if result.Thread.ID == "" {
		return nil, errors.New("thread/start response missing thread.id")
	}
	w.threadID = result.Thread.ID
	return w, nil
}

// writeJSON marshals v as JSON and writes it followed by a newline.
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// readJSONRPCResponse reads lines from r until it finds a JSON-RPC response
// (has "id" field). Notifications encountered during the handshake are logged
// and skipped.
func readJSONRPCResponse(r *bufio.Reader) (*JSONRPCMessage, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		if msg.IsResponse() {
			if msg.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			return &msg, nil
		}
		// Skip notifications during handshake.
		slog.Debug("codex handshake: skipping notification", "method", msg.Method)
	}
}

// buildArgs constructs the Codex CLI app-server arguments.
func buildArgs(_ *agent.Options) []string {
	return []string{"codex", "app-server"}
}

// slogWriter is an io.Writer that logs each line via slog.Warn.
type slogWriter struct {
	prefix    string
	container string
	buf       []byte
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Warn("stderr", "source", w.prefix, "container", w.container, "line", line)
		}
	}
	return len(p), nil
}
