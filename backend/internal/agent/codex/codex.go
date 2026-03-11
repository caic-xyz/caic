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
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Backend implements agent.Backend for Codex CLI using the app-server
// JSON-RPC 2.0 protocol.
type Backend struct {
	agent.Base
	mu sync.Mutex
}

var _ agent.Backend = (*Backend)(nil)

// New creates a Codex CLI backend with parser configured.
// ModelList starts with a single known-good model; it is replaced with the
// live list returned by model/list on the first successful handshake.
func New() *Backend {
	return &Backend{Base: agent.Base{
		HarnessID:     agent.Codex,
		ModelList:     []string{"gpt-5.4"},
		Images:        true,
		ContextWindow: 200_000,
		Parse:         ParseMessage,
	}}
}

// Models returns the current model list, updated dynamically after each handshake.
func (b *Backend) Models() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ModelList
}

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

	slog.Debug("relay", "msg", "launch", "ctr", opts.Container, "args", codexArgs)
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

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(stdout, 1<<16)

	wire, models, err := handshake(ctx, stdin, br, opts)
	if err != nil {
		// Kill the process on handshake failure.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("codex handshake: %w", err)
	}
	if len(models) > 0 {
		b.mu.Lock()
		b.ModelList = models
		b.mu.Unlock()
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
// opts.ResumeSessionID is used to pre-populate the thread ID so that
// WritePrompt works immediately without waiting for thread/started replay.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	sshArgs := []string{
		opts.Container, "python3", agent.RelayScriptPath, "attach",
		"--offset", strconv.FormatInt(opts.RelayOffset, 10),
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
	cmd.Stderr = &agent.SlogWriter{Prefix: "relay attach", Container: opts.Container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	// Pre-populate thread ID from the known session so WritePrompt works
	// immediately. wireFormat.process() will update it again if thread/started
	// appears in the replayed output.
	wire := &wireFormat{threadID: opts.ResumeSessionID}

	log := slog.With("container", opts.Container)
	return agent.NewSession(cmd, stdin, stdout, msgCh, logW, wire, log), nil
}

// wireFormat implements agent.WireFormat for the codex app-server JSON-RPC
// protocol. It holds per-session state: the thread ID, a request ID counter,
// and accumulated token usage from thread/tokenUsage/updated.
type wireFormat struct {
	threadID   string
	nextID     atomic.Int64
	mu         sync.Mutex
	totalUsage agent.Usage // accumulated per-turn from thread/tokenUsage/updated
}

// WritePrompt sends a turn/start JSON-RPC request to begin a new turn with
// the given user message. Images are sent as data URL items after the text item.
func (w *wireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threadID == "" {
		return errors.New("codex: no thread ID (handshake not completed)")
	}
	id := w.nextID.Add(1)
	input := make([]turnInput, 0, 1+len(p.Images))
	input = append(input, turnInput{Type: "text", Text: p.Text})
	for _, img := range p.Images {
		input = append(input, turnInput{
			Type: "image",
			URL:  "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "turn/start",
		Params:  turnStartParams{ThreadID: w.threadID, Input: input},
	}
	// Don't log to logW — stdin is not logged with --no-log-stdin.
	return writeJSON(wr, req)
}

// ParseMessage wraps the package-level ParseMessage with two interceptions:
//
//   - thread/tokenUsage/updated → emits UsageMessage (incremental Last
//     breakdown); values are also accumulated into totalUsage. Not forwarded
//     to the package-level ParseMessage.
//   - ResultMessage (from turn/completed) has Usage populated from totalUsage,
//     then totalUsage is reset for the next turn.
//
// It also captures the thread ID from InitMessage (thread/started).
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	// Intercept thread/tokenUsage/updated: emit a UsageMessage with the
	// incremental (Last) usage and accumulate into totalUsage.
	var probe methodProbe
	_ = json.Unmarshal(line, &probe)
	if probe.Method == MethodTokenUsageUpdated {
		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("tokenUsage/updated: %w", err)
		}
		var p TokenUsageUpdatedParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, fmt.Errorf("tokenUsage/updated params: %w", err)
		}
		incremental := agent.Usage{
			InputTokens:           int(p.TokenUsage.Last.InputTokens),
			CacheReadInputTokens:  int(p.TokenUsage.Last.CachedInputTokens),
			OutputTokens:          int(p.TokenUsage.Last.OutputTokens),
			ReasoningOutputTokens: int(p.TokenUsage.Last.ReasoningOutputTokens),
		}
		w.mu.Lock()
		w.totalUsage.InputTokens += incremental.InputTokens
		w.totalUsage.CacheReadInputTokens += incremental.CacheReadInputTokens
		w.totalUsage.OutputTokens += incremental.OutputTokens
		w.totalUsage.ReasoningOutputTokens += incremental.ReasoningOutputTokens
		w.mu.Unlock()
		usageMsg := &agent.UsageMessage{Usage: incremental}
		if p.TokenUsage.ModelContextWindow != nil {
			usageMsg.ContextWindow = int(*p.TokenUsage.ModelContextWindow)
		}
		return []agent.Message{usageMsg}, nil
	}

	msgs, err := ParseMessage(line)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		// Capture thread ID from InitMessage (produced by thread/started).
		if init, ok := msg.(*agent.InitMessage); ok && init.SessionID != "" {
			w.mu.Lock()
			w.threadID = init.SessionID
			w.mu.Unlock()
		}
		// Inject accumulated usage into ResultMessage and reset for next turn.
		if rm, ok := msg.(*agent.ResultMessage); ok {
			w.mu.Lock()
			rm.Usage = w.totalUsage
			w.totalUsage = agent.Usage{}
			w.mu.Unlock()
		}
	}
	return msgs, nil
}

// handshake performs the JSON-RPC initialize → initialized → model/list →
// thread/start (or thread/resume) sequence and returns a wireFormat with the
// thread ID set, plus the model IDs from model/list (may be nil on error).
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*wireFormat, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	w := &wireFormat{}

	// 1. Send initialize request.
	initReq := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      w.nextID.Add(1),
		Method:  "initialize",
		Params: initializeParams{
			ClientInfo: clientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
			Capabilities: capabilities{
				OptOutNotificationMethods: []string{
					// Interactive terminal prompts (e.g. sudo password, interactive stdin);
					// caic does not forward interactive terminal I/O to the agent.
					MethodCommandTerminalInteract,
					// Incremental diff of a file being written; we surface the completed
					// diff via item/completed fileChange instead.
					MethodFileChangeOutputDelta,
					// Streaming pre-summary reasoning part markers; we prefer the
					// incremental text via item/reasoning/summaryTextDelta.
					MethodReasoningSummaryPartAdded,
					// Raw token-by-token reasoning text; we prefer the summarised form via
					// item/reasoning/summaryTextDelta which is more readable.
					MethodReasoningTextDelta,
					// Incremental plan text delta; we surface the final plan text via
					// item/completed plan instead.
					MethodPlanDelta,
					// Coarse git diff snapshot repeated on every file change; we use the
					// caic-injected caic_diff_stat from the relay watcher instead.
					MethodTurnDiffUpdated,
					// High-level plan snapshot updated on each tool call; redundant with
					// item/plan which gives us the final plan text.
					MethodTurnPlanUpdated,
					// Thread name set by the agent (cosmetic label); caic uses the user's
					// initial prompt as the task title instead.
					MethodThreadNameUpdated,
				},
			},
		},
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	if _, err := readJSONRPCResponse(ctx, stdout); err != nil {
		return nil, nil, fmt.Errorf("read initialize response: %w", err)
	}

	// 2. Send initialized notification.
	if err := writeJSON(stdin, jsonrpcNotification{JSONRPC: "2.0", Method: "initialized"}); err != nil {
		return nil, nil, fmt.Errorf("write initialized: %w", err)
	}

	// 3. Fetch model list so the UI offers only valid model IDs.
	var models []string
	if err := writeJSON(stdin, jsonrpcRequest{JSONRPC: "2.0", ID: w.nextID.Add(1), Method: "model/list"}); err != nil {
		return nil, nil, fmt.Errorf("write model/list: %w", err)
	}
	if mlResp, err := readJSONRPCResponse(ctx, stdout); err == nil && mlResp.Result != nil {
		var mlResult ModelListResult
		if json.Unmarshal(mlResp.Result, &mlResult) == nil {
			for _, m := range mlResult.Models {
				if m.ID != "" {
					models = append(models, m.ID)
				}
			}
		}
	}

	// 4. Send thread/start or thread/resume.
	var threadReq jsonrpcRequest
	if opts.ResumeSessionID != "" {
		threadReq = jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/resume",
			Params:  threadResumeParams{ThreadID: opts.ResumeSessionID},
		}
	} else {
		threadReq = jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/start",
			Params:  threadStartParams{Model: opts.Model},
		}
	}
	if err := writeJSON(stdin, threadReq); err != nil {
		return nil, nil, fmt.Errorf("write thread/start: %w", err)
	}

	// Read thread/start response — contains the thread info.
	resp, err := readJSONRPCResponse(ctx, stdout)
	if err != nil {
		return nil, nil, fmt.Errorf("read thread/start response: %w", err)
	}

	// Extract thread ID from the response result.
	var result threadStartResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, nil, fmt.Errorf("parse thread/start result: %w", err)
	}
	if result.Thread.ID == "" {
		return nil, nil, errors.New("thread/start response missing thread.id")
	}
	w.threadID = result.Thread.ID
	return w, models, nil
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
// and skipped. It returns an error if ctx is cancelled before a response arrives.
func readJSONRPCResponse(ctx context.Context, r *bufio.Reader) (*JSONRPCMessage, error) {
	type result struct {
		msg *JSONRPCMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				ch <- result{nil, fmt.Errorf("read response: %w", err)}
				return
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var msg JSONRPCMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				ch <- result{nil, fmt.Errorf("unmarshal response: %w", err)}
				return
			}
			if msg.IsResponse() {
				if msg.Error != nil {
					ch <- result{nil, fmt.Errorf("JSON-RPC error %d: %s", msg.Error.Code, msg.Error.Message)}
					return
				}
				ch <- result{&msg, nil}
				return
			}
			// Skip notifications during handshake.
			slog.Debug("codex handshake: skipping notification", "method", msg.Method)
		}
	}()
	select {
	case res := <-ch:
		return res.msg, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("handshake: %w", ctx.Err())
	}
}

// buildArgs constructs the Codex CLI app-server arguments.
func buildArgs(_ *agent.Options) []string {
	return []string{"codex", "app-server"}
}
