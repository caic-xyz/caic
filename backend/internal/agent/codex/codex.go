// Package codex implements agent.Backend for Codex CLI.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// TODO: re-enable once widget plugin is fixed for codex
// widgetMCPServerPath is the container path for the widget MCP server script.
// var widgetMCPServerPath = agent.WidgetPluginDir + "/mcp_server.py"

// Backend implements agent.Backend for Codex CLI using the app-server
// JSON-RPC 2.0 protocol.
type Backend struct {
	agent.Base
}

var (
	_ agent.Backend          = (*Backend)(nil)
	_ agent.ModelFetcher     = (*Backend)(nil)
	_ agent.RecordHandshaker = (*Backend)(nil)
)

// New creates a Codex CLI backend with parser configured.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     harness.Codex,
		Images:        true,
		Compact:       true,
		ContextWindow: 200_000,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Codex, envVars))
	return b
}

// SetModelInventory implements agent.Backend, sorting models before storing
// them via the embedded Base (which owns the concurrency-safe storage).
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	byID := make(map[string]agent.Model, len(inventory.Models))
	for _, model := range inventory.Models {
		byID[model.ID] = model
	}
	ids := sortedModels(inventory.IDs())
	sorted := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		sorted = append(sorted, byID[id])
	}
	b.Base.SetModelInventory(agent.ModelInventory{Models: sorted})
}

// RecordHandshake performs the codex app-server JSON-RPC handshake
// (initialize → initialized → thread/start) for golden-file trace recording.
func (b *Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.Reader, model string) (agent.WireFormat, io.Reader, error) {
	br := bufio.NewReaderSize(stdout, 1<<16)
	log := agent.DiscardLogSink{Version: agent.LogVersionV1}
	wire, _, continuation, err := handshake(ctx, stdin, br, &agent.Options{Dir: "/workspace", Model: model, Log: log})
	if err != nil {
		return nil, nil, err
	}
	return wire, continuation, nil
}

// Start launches a Codex CLI app-server process via the relay daemon in the
// given container. It performs the JSON-RPC handshake (initialize →
// initialized → thread/start) before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if err := agent.DeployRelay(ctx, opts.Target, opts.Log.LogVersion()); err != nil {
		return nil, err
	}
	// TODO: re-enable once widget plugin is fixed for codex
	// if err := deployWidgetMCP(ctx, opts.Target); err != nil {
	// 	return nil, err
	// }

	codexArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})

	sshArgs := make([]string, 0, 8+len(codexArgs))
	sshArgs = append(sshArgs, sshHost, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin", "--")
	sshArgs = append(sshArgs, codexArgs...)

	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	opts.Logger.DebugContext(ctx, "relay", "msg", "launch", "target", sshHost, "args", codexArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Context: ctx, Logger: opts.Logger, Prefix: "relay serve-attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(stdout, 1<<16)

	wire, models, continuation, err := handshake(ctx, stdin, br, opts)
	if err != nil {
		// Kill the process on handshake failure.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("codex handshake: %w", err)
	}
	if len(models) > 0 {
		b.SetModelInventory(newModelInventory(models))
	}
	wire.suppressUserInput = true
	initMsg := &agent.InitMessage{SessionID: wire.threadID, Model: opts.Model, Version: wire.agentVersion}
	opts.MsgCh <- agent.ParsedMessage{Message: initMsg}
	if err := agent.WriteMetaSession(opts.Log, initMsg); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("write session metadata: %w", err)
	}

	log := opts.Logger.With("target", sshHost)
	s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, log, stdin, opts.Log, wire), continuation, opts.MsgCh, log)
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

// FetchModelInventory implements agent.ModelFetcher.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModelInfo(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return newModelInventory(models), nil
}

func fetchModelInfo(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) ([]codex.ModelInfo, error) {
	if target.SSHHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	args := []string{target.SSHHost}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, codexAppServerArgs()...)

	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // target is not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Prefix: "codex model-list", Container: target.SSHHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	nextID := atomic.Int64{}
	records, err := agent.NewRelayRecordReader(stdout, agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
	if err != nil {
		return nil, fmt.Errorf("construct model-list reader: %w", err)
	}
	models, err := fetchModelsFromAppServer(ctx, stdin, records, &nextID)
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
	wire := &wireFormat{threadID: opts.ResumeSessionID, effort: opts.Effort, suppressUserInput: true}
	return agent.AttachRelaySession(ctx, opts, wire, nil)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Schema drift is checked offline by check-agent-logs.
	return &wireFormat{}
}

func codexAppServerArgs() []string {
	return []string{
		"codex", "app-server",
		"-c", `approval_policy="never"`,
		"-c", `sandbox_mode="danger-full-access"`,
	}
}

func newModelInventory(modelInfo []codex.ModelInfo) agent.ModelInventory {
	models := sortedModels(modelIDs(modelInfo))
	return agent.ModelInventory{Models: modelsForModelInfo(models, modelInfo)}
}

func sortedModels(models []string) []string {
	models = agent.SortModels(models)
	slices.Reverse(models)
	return models
}

func modelIDs(modelInfo []codex.ModelInfo) []string {
	models := make([]string, 0, len(modelInfo))
	for i := range modelInfo {
		if modelInfo[i].ID != "" {
			models = append(models, modelInfo[i].ID)
		}
	}
	return models
}

func modelsForModelInfo(models []string, modelInfo []codex.ModelInfo) []agent.Model {
	efforts := make(map[string][]string, len(modelInfo))
	for i := range modelInfo {
		for _, effort := range modelInfo[i].SupportedReasoningEfforts {
			if effort.ReasoningEffort != "" {
				efforts[modelInfo[i].ID] = append(efforts[modelInfo[i].ID], string(effort.ReasoningEffort))
			}
		}
	}

	result := make([]agent.Model, 0, len(models))
	for _, model := range models {
		result = append(result, agent.Model{
			ID:            model,
			EffortOptions: efforts[model],
		})
	}
	return result
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
	agentVersion      string
	totalUsage        agent.Usage // accumulated per-turn from thread/tokenUsage/updated
}

// WritePrompt sends a turn/start JSON-RPC request to begin a new turn with
// the given user message. Images are sent as data URL items after the text item.
func (w *wireFormat) WritePrompt(wr io.Writer, p agent.Prompt, log agent.LogSink) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threadID == "" {
		return errors.New("codex: no thread ID (handshake not completed)")
	}
	id := w.nextID.Add(1)
	input := make([]codex.TurnInput, 0, 1+len(p.Images))
	input = append(input, codex.TurnInput{Type: codex.TurnInputTypeText, Text: p.Text})
	for _, img := range p.Images {
		input = append(input, codex.TurnInput{
			Type: codex.TurnInputTypeImage,
			URL:  "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	params, err := marshalParams(codex.TurnStartParams{
		ThreadID: w.threadID,
		Input:    input,
		Summary:  codex.ReasoningSummaryAuto,
		Effort:   codex.ReasoningEffort(w.effort),
	})
	if err != nil {
		return fmt.Errorf("marshal turn/start params: %w", err)
	}
	req := codex.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "turn/start",
		Params:  params,
	}
	// Don't log to logW — stdin is not logged with --no-log-stdin.
	return writeJSON(wr, req)
}

// WriteCompact implements agent.CompactCommand by sending a thread/compact/start
// JSON-RPC request. Codex compacts the context window for the current thread.
func (w *wireFormat) WriteCompact(wr io.Writer, _ string, _ agent.LogSink) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threadID == "" {
		return errors.New("codex: no thread ID (handshake not completed)")
	}
	params, err := marshalParams(codex.ThreadCompactStartParams{ThreadID: w.threadID})
	if err != nil {
		return fmt.Errorf("marshal thread/compact/start params: %w", err)
	}
	req := codex.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      w.nextID.Add(1),
		Method:  "thread/compact/start",
		Params:  params,
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
	var probe codex.MethodProbe
	_ = json.Unmarshal(line, &probe)
	if probe.Method == codex.MethodTokenUsageUpdated {
		var msg codex.JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("tokenUsage/updated: %w", err)
		}
		var p codex.ThreadTokenUsageUpdatedNotification
		if err := json.Unmarshal(msg.Params, &p); err != nil {
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

	msgs, err := parseMessage(line)
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
// thread ID set, plus model metadata from model/list.
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*wireFormat, []codex.ModelInfo, *bufio.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	w := &wireFormat{effort: opts.Effort}
	records, err := agent.NewRelayRecordReader(stdout, opts.Log.LogVersion(), agent.DiscardLogSink{Version: opts.Log.LogVersion()})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct relay reader: %w", err)
	}

	models, err := fetchModelsFromAppServer(ctx, stdin, records, &w.nextID)
	if err != nil {
		return nil, nil, nil, err
	}

	// 4. Send thread/start or thread/resume.
	var threadReq codex.JSONRPCRequest
	if opts.ResumeSessionID != "" {
		params, err := marshalParams(codex.ThreadResumeParams{ThreadID: opts.ResumeSessionID})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("marshal thread/resume params: %w", err)
		}
		threadReq = codex.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/resume",
			Params:  params,
		}
	} else {
		params, err := marshalParams(codex.ThreadStartParams{Model: opts.Model})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("marshal thread/start params: %w", err)
		}
		threadReq = codex.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.nextID.Add(1),
			Method:  "thread/start",
			Params:  params,
		}
	}
	if err := writeJSON(stdin, threadReq); err != nil {
		return nil, nil, nil, fmt.Errorf("write thread/start: %w", err)
	}

	// Read thread/start response — contains the thread info.
	resp, err := readJSONRPCResponse(ctx, records)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read thread/start response: %w", err)
	}

	// Extract thread ID from the response result.
	var result codex.ThreadStartResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, nil, nil, fmt.Errorf("parse thread/start result: %w", err)
	}
	if result.Thread.ID == "" {
		return nil, nil, nil, errors.New("thread/start response missing thread.id")
	}
	w.threadID = result.Thread.ID
	w.agentVersion = result.Thread.CLIVersion
	return w, models, records.Reader(), nil
}

func fetchModelsFromAppServer(ctx context.Context, stdin io.Writer, records *agent.RelayRecordReader, nextID *atomic.Int64) ([]codex.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	initParams, err := marshalParams(codex.InitializeParams{
		ClientInfo: codex.ClientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
		Capabilities: codex.Capabilities{
			OptOutNotificationMethods: []codex.Method{
				// Interactive terminal prompts (e.g. sudo password, interactive stdin);
				// caic does not forward interactive terminal I/O to the agent.
				codex.MethodCommandTerminalInteract,
				// Streaming pre-summary reasoning part markers; we prefer the
				// incremental text via item/reasoning/summaryTextDelta.
				codex.MethodReasoningSummaryPartAdded,
				// Raw token-by-token reasoning text; we prefer the summarised form via
				// item/reasoning/summaryTextDelta which is more readable.
				codex.MethodReasoningTextDelta,
				// Incremental plan text delta; we surface the final plan text via
				// item/completed plan instead.
				codex.MethodPlanDelta,
				// Coarse git diff snapshot repeated on every file change; we use the
				// caic-injected caic_diff_stat from the relay watcher instead.
				codex.MethodTurnDiffUpdated,
				// High-level plan snapshot updated on each tool call; redundant with
				// item/plan which gives us the final plan text.
				codex.MethodTurnPlanUpdated,
				// Thread name set by the agent (cosmetic label); caic uses the user's
				// initial prompt as the task title instead.
				codex.MethodThreadNameUpdated,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal initialize params: %w", err)
	}
	initReq := codex.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      nextID.Add(1),
		Method:  "initialize",
		Params:  initParams,
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	if _, err := readJSONRPCResponse(ctx, records); err != nil {
		return nil, fmt.Errorf("read initialize response: %w", err)
	}

	// 2. Send initialized notification.
	if err := writeJSON(stdin, codex.JSONRPCNotification{JSONRPC: "2.0", Method: "initialized"}); err != nil {
		return nil, fmt.Errorf("write initialized: %w", err)
	}

	// 3. Fetch model list so the UI offers only valid model IDs.
	var models []codex.ModelInfo
	mlParams, err := marshalParams(struct{}{})
	if err != nil {
		return nil, fmt.Errorf("marshal model/list params: %w", err)
	}
	if err := writeJSON(stdin, codex.JSONRPCRequest{JSONRPC: "2.0", ID: nextID.Add(1), Method: "model/list", Params: mlParams}); err != nil {
		return nil, fmt.Errorf("write model/list: %w", err)
	}
	mlResp, err := readJSONRPCResponse(ctx, records)
	if err != nil {
		return nil, fmt.Errorf("read model/list response: %w", err)
	}
	if mlResp.Result == nil {
		return nil, errors.New("model/list response missing result")
	}
	var mlResult codex.ModelListResult
	if err := json.Unmarshal(mlResp.Result, &mlResult); err != nil {
		return nil, fmt.Errorf("parse model/list result: %w", err)
	}
	for i := range mlResult.Data {
		if mlResult.Data[i].ID != "" {
			models = append(models, mlResult.Data[i])
		}
	}
	return models, nil
}

// marshalParams marshals v into a json.RawMessage for use as JSONRPCRequest.Params.
func marshalParams(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
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
func readJSONRPCResponse(ctx context.Context, r *agent.RelayRecordReader) (*codex.JSONRPCMessage, error) {
	type result struct {
		msg *codex.JSONRPCMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, controls, err := r.ReadRecord()
			if err != nil {
				ch <- result{nil, fmt.Errorf("read response: %w", err)}
				return
			}
			for _, control := range controls {
				if exit, ok := control.Message.(*agent.ExitMessage); ok && exit.ExitCode != 0 {
					ch <- result{nil, errors.New(exit.ExitError())}
					return
				}
			}
			if len(line) == 0 {
				continue
			}
			var msg codex.JSONRPCMessage
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
			slog.DebugContext(ctx, "codex handshake: skipping notification", "method", msg.Method)
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
// deployWidgetMCP writes the widget MCP server script to the target so
// that codex can launch it as a stdio MCP server.
// func deployWidgetMCP(ctx context.Context, target runtime.ConnectionTarget) error {
// 	if target.SSHHost == "" {
// 		return errors.New("agent connection target missing SSH host")
// 	}
// 	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, //nolint:gosec // target is not user-controlled
// 		"mkdir -p "+agent.WidgetPluginDir+" && cat > "+widgetMCPServerPath)
// 	cmd.Stdin = bytes.NewReader(agent.WidgetMCPServerScript)
// 	if out, err := cmd.CombinedOutput(); err != nil {
// 		return fmt.Errorf("deploy widget MCP: %w: %s", err, out)
// 	}
// 	return nil
// }
