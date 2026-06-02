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
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	cx "github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// TODO: re-enable once widget plugin is fixed for codex
// widgetMCPServerPath is the container path for the widget MCP server script.
// var widgetMCPServerPath = agent.WidgetPluginDir + "/mcp_server.py"

// Backend implements agent.Backend for Codex CLI using the app-server
// JSON-RPC 2.0 protocol.
type Backend struct {
	agent.Base

	mu      sync.Mutex
	cache   *agent.HarnessCache
	EnvVars []string // KEY=VALUE pairs used to scope cached model lists.
}

var (
	_ agent.Backend          = (*Backend)(nil)
	_ agent.ModelFetcher     = (*Backend)(nil)
	_ agent.RecordHandshaker = (*Backend)(nil)
)

// New creates a Codex CLI backend with parser configured. If cacheDir is
// non-empty, the model list is loaded from the on-disk harness cache.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{EnvVars: envVars}
	b.Base = agent.Base{
		HarnessID:     agent.Codex,
		Images:        true,
		Compact:       true,
		ContextWindow: 200_000,
	}
	if cacheDir != "" {
		b.cache = agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
		if models, _ := b.cache.Models(agent.Codex, agent.APIKeyHash(envVars)); len(models) > 0 {
			b.setModels(models)
		}
	}
	return b
}

// ExportDiscussion reads a JSONL log and returns the conversation as markdown.
func (b *Backend) ExportDiscussion(path string) (string, error) {
	return agent.ExportDiscussion(path, b.NewWire().ParseMessage)
}

// Models returns the current model list, updated dynamically after each handshake.
func (b *Backend) Models() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ModelList
}

// SetModels replaces the model list with sorted models. Thread-safe.
func (b *Backend) SetModels(models []string) {
	b.setModels(models)
}

// RecordHandshake performs the codex app-server JSON-RPC handshake
// (initialize → initialized → thread/start) for golden-file trace recording.
func (b *Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.Reader, model string) (agent.WireFormat, io.Reader, error) {
	br := bufio.NewReaderSize(stdout, 1<<16)
	wire, _, err := handshake(ctx, stdin, br, &agent.Options{Dir: "/workspace", Model: model})
	if err != nil {
		return nil, nil, err
	}
	return wire, br, nil
}

// Start launches a Codex CLI app-server process via the relay daemon in the
// given container. It performs the JSON-RPC handshake (initialize →
// initialized → thread/start) before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}
	// TODO: re-enable once widget plugin is fixed for codex
	// if err := deployWidgetMCP(ctx, opts.Container); err != nil {
	// 	return nil, err
	// }

	codexArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})

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
		b.setDiscoveredModels(models)
	}
	wire.suppressUserInput = true
	initMsg := &agent.InitMessage{SessionID: wire.threadID, Model: opts.Model}
	opts.MsgCh <- initMsg
	if err := agent.WriteMetaSession(opts.LogW, initMsg); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("write session metadata: %w", err)
	}

	log := slog.With("container", opts.Container)
	s := agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, wire), br, opts.MsgCh, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.SendPrompt(opts.InitialPrompt); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(_ agent.HarnessArgs) []string {
	// TODO: re-enable widget MCP plugin once it's fixed for codex
	// return []string{
	// 	"codex", "app-server",
	// 	"-c", `approval_policy="never"`,
	// 	"-c", `sandbox_mode="danger-full-access"`,
	// 	"-c", `mcp_servers.widget.command="python3"`,
	// 	"-c", `mcp_servers.widget.args=["` + widgetMCPServerPath + `"]`,
	// }
	return codexAppServerArgs()
}

// FetchModels implements agent.ModelFetcher.
func (*Backend) FetchModels(ctx context.Context, instance string, extraEnv []string) ([]string, error) {
	return FetchModels(ctx, instance, extraEnv)
}

// FetchModels SSHes into the given container, runs codex app-server, fetches
// model/list, and returns the model ID list.
// extraEnv holds KEY=VALUE pairs injected via the env command.
func FetchModels(ctx context.Context, container string, extraEnv []string) ([]string, error) {
	args := []string{container}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, codexAppServerArgs()...)

	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // container is not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Prefix: "codex model-list", Container: container}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	nextID := atomic.Int64{}
	models, err := fetchModelsFromAppServer(ctx, stdin, bufio.NewReaderSize(stdout, 1<<16), &nextID)
	if err != nil {
		return nil, fmt.Errorf("codex model/list: %w", err)
	}
	return models, nil
}

// AttachRelay connects to an already-running relay in the container.
// opts.ResumeSessionID is used to pre-populate the thread ID so that
// WritePrompt works immediately without waiting for thread/started replay.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if opts.ResumeSessionID == "" {
		return nil, errors.New("codex: missing thread ID for relay attach")
	}
	// Pre-populate thread ID from the known session so WritePrompt works
	// immediately. wireFormat.process() will update it again if thread/started
	// appears in the replayed output.
	wire := &wireFormat{threadID: opts.ResumeSessionID, effort: opts.Effort, suppressUserInput: true, fw: &jsonutil.FieldWarner{}}
	return agent.AttachRelaySession(ctx, opts, wire)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	return &wireFormat{fw: &jsonutil.FieldWarner{}}
}

func codexAppServerArgs() []string {
	return []string{
		"codex", "app-server",
		"-c", `approval_policy="never"`,
		"-c", `sandbox_mode="danger-full-access"`,
	}
}

func (b *Backend) setDiscoveredModels(models []string) {
	models = b.setModels(models)
	if b.cache != nil {
		b.cache.SetModels(agent.Codex, models, agent.APIKeyHash(b.EnvVars))
	}
}

func (b *Backend) setModels(models []string) []string {
	models = agent.SortModels(models)
	slices.Reverse(models)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.ModelList = models
	return models
}

// wireFormat implements agent.WireFormat for the codex app-server JSON-RPC
// protocol. It holds per-session state: the thread ID, a request ID counter,
// accumulated token usage from thread/tokenUsage/updated, and the reasoning
// effort level.
type wireFormat struct {
	threadID          string
	effort            string // Reasoning effort (e.g. "none", "low", "medium", "high").
	suppressUserInput bool
	nextID            atomic.Int64
	mu                sync.Mutex
	totalUsage        agent.Usage // accumulated per-turn from thread/tokenUsage/updated
	fw                *jsonutil.FieldWarner
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
	input := make([]cx.TurnInput, 0, 1+len(p.Images))
	input = append(input, cx.TurnInput{Type: cx.TurnInputTypeText, Text: p.Text})
	for _, img := range p.Images {
		input = append(input, cx.TurnInput{
			Type: cx.TurnInputTypeImage,
			URL:  "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	req := cx.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "turn/start",
		Params: cx.TurnStartParams{
			ThreadID: w.threadID,
			Input:    input,
			Summary:  cx.ReasoningSummaryAuto,
			Effort:   cx.ReasoningEffort(w.effort),
		},
	}
	// Don't log to logW — stdin is not logged with --no-log-stdin.
	return writeJSON(wr, req)
}

// WriteCompact implements agent.CompactCommand by sending a thread/compact/start
// JSON-RPC request. Codex compacts the context window for the current thread.
func (w *wireFormat) WriteCompact(wr io.Writer, _ string, _ io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threadID == "" {
		return errors.New("codex: no thread ID (handshake not completed)")
	}
	req := cx.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      w.nextID.Add(1),
		Method:  "thread/compact/start",
		Params:  cx.ThreadCompactStartParams{ThreadID: w.threadID},
	}
	return writeJSON(wr, req)
}

// ParseMessage wraps the package-level parseMessage with two interceptions:
//
//   - thread/tokenUsage/updated → emits UsageMessage (incremental Last
//     breakdown); values are also accumulated into totalUsage. Not forwarded
//     to the package-level parseMessage.
//   - ResultMessage (from turn/completed) has Usage populated from totalUsage,
//     then totalUsage is reset for the next turn.
//
// It also captures the thread ID from InitMessage (thread/started).
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	// Intercept thread/tokenUsage/updated: emit a UsageMessage with the
	// incremental (Last) usage and accumulate into totalUsage.
	var probe cx.MethodProbe
	_ = json.Unmarshal(line, &probe)
	if probe.Method == cx.MethodTokenUsageUpdated {
		var msg cx.JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("tokenUsage/updated: %w", err)
		}
		var p cx.ThreadTokenUsageUpdatedNotification
		if err := unmarshalNotification(msg.Params, &p, "ThreadTokenUsageUpdatedNotification", w.fw); err != nil {
			return nil, fmt.Errorf("tokenUsage/updated params: %w", err)
		}
		// Codex reports cached token counts but not cache TTL. OpenAI's
		// default in-memory prompt cache lasts 5–10 min of inactivity,
		// up to 1 hour. If Codex starts using
		// prompt_cache_retention:"24h", set CacheTTLSeconds = 86400.
		// See https://developers.openai.com/api/docs/guides/prompt-caching
		incremental := agent.Usage{
			InputTokens:           int(p.TokenUsage.Last.InputTokens),
			CacheReadInputTokens:  int(p.TokenUsage.Last.CachedInputTokens),
			OutputTokens:          int(p.TokenUsage.Last.OutputTokens),
			ReasoningOutputTokens: int(p.TokenUsage.Last.ReasoningOutputTokens),
			CacheTTLSeconds:       300,
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

	msgs, err := parseMessage(line, w.fw)
	if err != nil {
		return nil, err
	}
	out := msgs[:0]
	for _, msg := range msgs {
		if _, ok := msg.(*agent.UserInputMessage); ok && w.suppressUserInput {
			continue
		}
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
		out = append(out, msg)
	}
	return out, nil
}

// handshake performs the JSON-RPC initialize → initialized → model/list →
// thread/start (or thread/resume) sequence and returns a wireFormat with the
// thread ID set, plus the model IDs from model/list.
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*wireFormat, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	w := &wireFormat{effort: opts.Effort, fw: &jsonutil.FieldWarner{}}

	models, err := fetchModelsFromAppServer(ctx, stdin, stdout, &w.nextID)
	if err != nil {
		return nil, nil, err
	}

	// 4. Send thread/start or thread/resume.
	var threadReq cx.JSONRPCRequest
	if opts.ResumeSessionID != "" {
		threadReq = cx.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/resume",
			Params:  cx.ThreadResumeParams{ThreadID: opts.ResumeSessionID},
		}
	} else {
		threadReq = cx.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/start",
			Params:  cx.ThreadStartParams{Model: opts.Model},
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
	var result cx.ThreadStartResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, nil, fmt.Errorf("parse thread/start result: %w", err)
	}
	if result.Thread.ID == "" {
		return nil, nil, errors.New("thread/start response missing thread.id")
	}
	w.threadID = result.Thread.ID
	return w, models, nil
}

func fetchModelsFromAppServer(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, nextID *atomic.Int64) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	initReq := cx.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      nextID.Add(1),
		Method:  "initialize",
		Params: cx.InitializeParams{
			ClientInfo: cx.ClientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
			Capabilities: cx.Capabilities{
				OptOutNotificationMethods: []cx.Method{
					// Interactive terminal prompts (e.g. sudo password, interactive stdin);
					// caic does not forward interactive terminal I/O to the agent.
					cx.MethodCommandTerminalInteract,
					// Incremental diff of a file being written; we surface the completed
					// diff via item/completed fileChange instead.
					cx.MethodFileChangeOutputDelta,
					// Streaming pre-summary reasoning part markers; we prefer the
					// incremental text via item/reasoning/summaryTextDelta.
					cx.MethodReasoningSummaryPartAdded,
					// Raw token-by-token reasoning text; we prefer the summarised form via
					// item/reasoning/summaryTextDelta which is more readable.
					cx.MethodReasoningTextDelta,
					// Incremental plan text delta; we surface the final plan text via
					// item/completed plan instead.
					cx.MethodPlanDelta,
					// Coarse git diff snapshot repeated on every file change; we use the
					// caic-injected caic_diff_stat from the relay watcher instead.
					cx.MethodTurnDiffUpdated,
					// High-level plan snapshot updated on each tool call; redundant with
					// item/plan which gives us the final plan text.
					cx.MethodTurnPlanUpdated,
					// Thread name set by the agent (cosmetic label); caic uses the user's
					// initial prompt as the task title instead.
					cx.MethodThreadNameUpdated,
				},
			},
		},
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	if _, err := readJSONRPCResponse(ctx, stdout); err != nil {
		return nil, fmt.Errorf("read initialize response: %w", err)
	}

	// 2. Send initialized notification.
	if err := writeJSON(stdin, cx.JSONRPCNotification{JSONRPC: "2.0", Method: "initialized"}); err != nil {
		return nil, fmt.Errorf("write initialized: %w", err)
	}

	// 3. Fetch model list so the UI offers only valid model IDs.
	var models []string
	if err := writeJSON(stdin, cx.JSONRPCRequest{JSONRPC: "2.0", ID: nextID.Add(1), Method: "model/list", Params: struct{}{}}); err != nil {
		return nil, fmt.Errorf("write model/list: %w", err)
	}
	mlResp, err := readJSONRPCResponse(ctx, stdout)
	if err != nil {
		return nil, fmt.Errorf("read model/list response: %w", err)
	}
	if mlResp.Result == nil {
		return nil, errors.New("model/list response missing result")
	}
	var mlResult cx.ModelListResult
	if err := json.Unmarshal(mlResp.Result, &mlResult); err != nil {
		return nil, fmt.Errorf("parse model/list result: %w", err)
	}
	for i := range mlResult.Data {
		if mlResult.Data[i].ID != "" {
			models = append(models, mlResult.Data[i].ID)
		}
	}
	return models, nil
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
func readJSONRPCResponse(ctx context.Context, r *bufio.Reader) (*cx.JSONRPCMessage, error) {
	type result struct {
		msg *cx.JSONRPCMessage
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
			var msg cx.JSONRPCMessage
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

// TODO: re-enable once widget plugin is fixed for codex
// deployWidgetMCP writes the widget MCP server script to the container so
// that codex can launch it as a stdio MCP server.
// func deployWidgetMCP(ctx context.Context, container string) error {
// 	cmd := exec.CommandContext(ctx, "ssh", container, //nolint:gosec // container is not user-controlled
// 		"mkdir -p "+agent.WidgetPluginDir+" && cat > "+widgetMCPServerPath)
// 	cmd.Stdin = bytes.NewReader(agent.WidgetMCPServerScript)
// 	if out, err := cmd.CombinedOutput(); err != nil {
// 		return fmt.Errorf("deploy widget MCP: %w: %s", err, out)
// 	}
// 	return nil
// }
