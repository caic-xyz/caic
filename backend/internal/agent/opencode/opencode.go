// Package opencode implements agent.Backend for OpenCode via ACP
// (Agent Client Protocol): JSON-RPC 2.0 over stdin/stdout.
package opencode

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
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Backend implements agent.Backend for OpenCode using the ACP JSON-RPC 2.0
// protocol.
type Backend struct {
	agent.Base
}

var (
	_ agent.Backend          = (*Backend)(nil)
	_ agent.ModelFetcher     = (*Backend)(nil)
	_ agent.RecordHandshaker = (*Backend)(nil)
)

// New creates an OpenCode backend with parser configured.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     harness.OpenCode,
		Images:        true,
		Compact:       true,
		ContextWindow: 200_000,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.OpenCode, envVars))
	return b
}

// RecordHandshake performs the ACP handshake (initialize → session/new →
// optional set_model) over stdin/stdout for record-trace golden-file
// generation. It returns a populated wireFormat and a buffered reader that
// replaces the original stdout for subsequent reads.
func (b *Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.Reader, model string) (agent.WireFormat, io.Reader, error) {
	br := bufio.NewReaderSize(stdout, 1<<16)
	log := agent.DiscardLogSink{Version: agent.LogVersionV1}
	hs, continuation, err := handshake(ctx, stdin, br, &agent.Options{Dir: "/workspace", Model: model, Log: log})
	if err != nil {
		return nil, nil, err
	}
	return hs.wire, continuation, nil
}

// SetModelInventory implements agent.Backend, normalizing model names before
// storing them via the embedded Base (which owns the concurrency-safe storage).
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	b.Base.SetModelInventory(agent.ModelInventory{Models: normalizeModels(inventory.Models)})
}

// Start launches an OpenCode ACP process via the relay daemon in the given
// container. It performs the JSON-RPC handshake (initialize → session/new)
// before returning a Session.
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

	ocArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})

	sshArgs := make([]string, 0, 8+len(ocArgs))
	sshArgs = append(sshArgs, sshHost, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin", "--")
	sshArgs = append(sshArgs, ocArgs...)

	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	opts.Logger.DebugContext(ctx, "relay", "msg", "launch", "target", sshHost, "args", ocArgs)
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

	hs, continuation, err := handshake(ctx, stdin, br, opts)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("opencode handshake: %w", err)
	}
	// Emit InitMessage so the task captures session ID, model, and version.
	initMsg := &agent.InitMessage{
		SessionID: hs.wire.sessionID,
		Model:     hs.currentModel,
		Version:   hs.agentVersion,
	}
	opts.MsgCh <- agent.TimedMessage{Message: initMsg}
	if err := agent.WriteMetaSession(opts.Log, initMsg); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		shutdownErr := agent.StopRelay(shutdownCtx, opts.Target)
		closeErr := stdin.Close()
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()
		var waitErr error
		select {
		case waitErr = <-waitCh:
		case <-shutdownCtx.Done():
			_ = cmd.Process.Kill()
			waitErr = <-waitCh
		}
		return nil, fmt.Errorf("write session metadata: %w", errors.Join(err, shutdownErr, closeErr, waitErr))
	}

	log := opts.Logger.With("target", sshHost)
	c := agent.NewConn(ctx, log, stdin, opts.Log, hs.wire)
	s := agent.NewSession(ctx, cmd, c, continuation, opts.MsgCh, log)
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
	return []string{"opencode", "acp"}
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if opts.ResumeSessionID == "" {
		return nil, errors.New("opencode: missing session ID for relay attach")
	}
	wire := &wireFormat{sessionID: opts.ResumeSessionID}
	return agent.AttachRelaySession(ctx, opts, wire, nil)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Schema drift is checked offline by check-agent-logs.
	return &wireFormat{}
}

// FetchModelInventory implements agent.ModelFetcher.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModels(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return agent.ModelInventory{Models: models}, nil
}

// CaicInit is the legacy pre-caic_session metadata record.
//
// TODO: Trim CaicInit after 2026-08 once legacy caic_init logs are old enough to ignore.
type CaicInit struct {
	Type      string `json:"type"` // always "caic_init"
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitzero"`
	Version   string `json:"version,omitzero"`
}

// maxAccumulatedOutputBytes bounds synthetic final-message state. Streaming
// deltas are already persisted independently, so an overflow disables the
// synthetic final rather than retaining an incomplete duplicate.
const maxAccumulatedOutputBytes = 1 << 20

// wireFormat implements agent.WireFormat for the ACP JSON-RPC protocol.
// It holds per-session state: the session ID, a request ID counter,
// accumulated token usage, and image support flag.
type wireFormat struct {
	sessionID     string // Set during handshake; read-only after.
	supportsImage bool   // Set during handshake; read-only after.

	mu            sync.Mutex
	nextID        int64
	promptReqID   int64 // JSON-RPC ID of the current session/prompt request.
	totalUsage    agent.Usage
	textAccum     strings.Builder // Accumulated text from agent_message_chunk.
	thinkAccum    strings.Builder // Accumulated text from agent_thought_chunk.
	textOverflow  bool
	thinkOverflow bool
}

// WritePrompt sends a session/prompt JSON-RPC request to begin a new turn.
func (w *wireFormat) WritePrompt(wr io.Writer, p agent.Prompt, log agent.LogSink) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sessionID == "" {
		return errors.New("opencode: no session ID (handshake not completed)")
	}
	id := w.allocIDLocked()
	w.promptReqID = id
	w.resetAccumulatedOutputLocked()
	content := make([]opencode.PromptContent, 0, 1+len(p.Images))
	content = append(content, opencode.PromptContent{Type: opencode.ContentText, Text: p.Text})
	if w.supportsImage {
		for _, img := range p.Images {
			content = append(content, opencode.PromptContent{
				Type:     opencode.ContentImage,
				Data:     img.Data,
				MimeType: img.MediaType,
			})
		}
	}
	params, err := marshalParams(opencode.SessionPromptParams{SessionID: w.sessionID, Prompt: content})
	if err != nil {
		return fmt.Errorf("marshal session/prompt params: %w", err)
	}
	req := opencode.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  opencode.MethodSessionPrompt,
		Params:  params,
	}
	// Don't log to logW — stdin is not logged with --no-log-stdin.
	return writeJSON(wr, req)
}

// WriteCompact implements agent.CompactCommand by sending /compact as a prompt.
// OpenCode recognizes this as a slash command via session/prompt.
func (w *wireFormat) WriteCompact(wr io.Writer, _ string, log agent.LogSink) error {
	return w.WritePrompt(wr, agent.Prompt{Text: "/compact"}, log)
}

// ParseMessage wraps the package-level parseMessage with interceptions:
//
//   - usage_update → emits UsageMessage and accumulates into totalUsage.
//   - logged session/prompt requests → restores prompt response correlation.
//   - session/request_permission → auto-approves with "allow_once".
//   - prompt responses → emits final Text/Thinking messages and ResultMessage.
//
// It also captures the session ID from InitMessage if present.
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	var probe opencode.MessageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// JSON-RPC requests can appear in replay logs when stdin was logged. Capture
	// session/prompt IDs so the later response is still recognized.
	if probe.ID != nil {
		var id int64
		if json.Unmarshal(probe.ID, &id) == nil {
			if probe.Method != "" {
				if probe.Method == opencode.MethodSessionPrompt {
					w.notePromptRequest(id)
				}
				return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_request", Raw: append([]byte(nil), line...)}}, nil
			}
			w.mu.Lock()
			isPromptResp := id == w.promptReqID && w.promptReqID != 0
			w.mu.Unlock()
			if isPromptResp || isPromptResultResponse(line) {
				return w.handlePromptResponseLocked(line)
			}
		}
		// Other responses pass through as RawMessage.
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil
	}

	// Intercept usage_update to accumulate totals.
	if probe.Method == opencode.MethodSessionUpdate {
		params, err := extractParams(line)
		if err != nil {
			return nil, fmt.Errorf("extract params: %w", err)
		}
		var sup opencode.SessionUpdateParams
		if err := json.Unmarshal(params, &sup); err == nil {
			var uprobe opencode.UpdateProbe
			if err := json.Unmarshal(sup.Update, &uprobe); err == nil && uprobe.SessionUpdate == opencode.UpdateUsageUpdate {
				var u opencode.UsageUpdateUpdate
				if err := json.Unmarshal(sup.Update, &u); err == nil {
					// usage_update provides context window size and cost but not
					// per-step token breakdown. We emit a UsageMessage with the
					// context window; token details come from the prompt result.
					return []agent.Message{&agent.UsageMessage{
						ContextWindow: u.Size,
					}}, nil
				}
			}
		}
	}

	msgs, err := parseMessage(line)
	if err != nil {
		return nil, err
	}
	// Accumulate text/thinking deltas for synthetic final messages.
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.TextDeltaMessage:
			w.mu.Lock()
			appendAccumulatedOutput(&w.textAccum, &w.textOverflow, m.Text)
			w.mu.Unlock()
		case *agent.ThinkingDeltaMessage:
			w.mu.Lock()
			appendAccumulatedOutput(&w.thinkAccum, &w.thinkOverflow, m.Text)
			w.mu.Unlock()
		}
	}
	return msgs, nil
}

func (w *wireFormat) notePromptRequest(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.promptReqID = id
	w.resetAccumulatedOutputLocked()
}

func (w *wireFormat) resetAccumulatedOutputLocked() {
	w.textAccum.Reset()
	w.thinkAccum.Reset()
	w.textOverflow = false
	w.thinkOverflow = false
}

func appendAccumulatedOutput(builder *strings.Builder, overflow *bool, text string) {
	if *overflow || len(text) > maxAccumulatedOutputBytes-builder.Len() {
		builder.Reset()
		*overflow = true
		return
	}
	builder.WriteString(text)
}

func isPromptResultResponse(line []byte) bool {
	var resp opencode.JSONRPCMessage
	if err := json.Unmarshal(line, &resp); err != nil || resp.Result == nil {
		return false
	}
	var pr opencode.PromptResult
	return json.Unmarshal(resp.Result, &pr) == nil && pr.StopReason != ""
}

// allocIDLocked returns the next JSON-RPC request ID. Not thread-safe; callers
// must hold mu or be in the single-threaded handshake phase.
func (w *wireFormat) allocIDLocked() int64 {
	w.nextID++
	return w.nextID
}

// handlePromptResponseLocked converts a session/prompt JSON-RPC response into
// a ResultMessage with accumulated usage. Emits synthetic TextMessage and
// ThinkingMessage from accumulated deltas before the ResultMessage.
// Must not be called under mu.
func (w *wireFormat) handlePromptResponseLocked(line []byte) ([]agent.Message, error) {
	var resp opencode.JSONRPCMessage
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal prompt response: %w", err)
	}
	rm := &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "result",
	}
	if resp.Error != nil {
		rm.IsError = true
		rm.Result = resp.Error.Message
	} else if resp.Result != nil {
		var pr opencode.PromptResult
		if err := json.Unmarshal(resp.Result, &pr); err == nil {
			if pr.StopReason == "cancelled" || pr.StopReason == "refusal" {
				rm.IsError = true
				rm.Result = pr.StopReason
			}
			if pr.Usage != (opencode.PromptUsage{}) {
				// ACP reports cache reads and writes but no duration bucket or
				// applied retention policy. OpenCode supports many providers and
				// gateways, so leave the cache TTL unknown rather than inferring it
				// from the selected model or request adapter:
				// https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/opencode/src/acp/usage.ts
				// https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/opencode/src/provider/transform.ts
				rm.Usage = agent.Usage{
					InputTokens:              pr.Usage.InputTokens,
					OutputTokens:             pr.Usage.OutputTokens,
					CacheReadInputTokens:     pr.Usage.CachedReadTokens,
					CacheCreationInputTokens: pr.Usage.CachedWriteTokens,
					ReasoningOutputTokens:    pr.Usage.ThoughtTokens,
				}
			}
		}
	}
	// Emit synthetic final messages from accumulated deltas, then reset.
	w.mu.Lock()
	if rm.Usage == (agent.Usage{}) {
		rm.Usage = w.totalUsage
	}
	w.totalUsage = agent.Usage{}
	var msgs []agent.Message
	if !w.thinkOverflow && w.thinkAccum.Len() > 0 {
		msgs = append(msgs, &agent.ThinkingMessage{Text: w.thinkAccum.String()})
	}
	if !w.textOverflow && w.textAccum.Len() > 0 {
		msgs = append(msgs, &agent.TextMessage{Text: w.textAccum.String()})
	}
	w.resetAccumulatedOutputLocked()
	w.mu.Unlock()
	msgs = append(msgs, rm)
	return msgs, nil
}

// handshakeResult bundles everything returned by a successful handshake.
type handshakeResult struct {
	wire          *wireFormat
	currentModel  string // Model ID the session is using.
	agentVersion  string // Agent version string from initialize.
	configOptions []opencode.SessionConfigOption
}

// handshake performs the ACP initialize → session/new sequence and returns
// a handshakeResult with the wireFormat, model list, and agent metadata.
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*handshakeResult, *bufio.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	w := &wireFormat{}
	records, err := agent.NewRelayRecordReader(stdout, opts.Log.LogVersion(), agent.DiscardLogSink{Version: opts.Log.LogVersion()})
	if err != nil {
		return nil, nil, fmt.Errorf("construct relay reader: %w", err)
	}
	res := &handshakeResult{wire: w}

	// 1. Send initialize request.
	initParams, err := marshalParams(opencode.InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: opencode.ClientCapabilities{
			Terminal: false,
		},
		ClientInfo: opencode.ClientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal initialize params: %w", err)
	}
	initReq := opencode.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      w.allocIDLocked(),
		Method:  opencode.MethodInitialize,
		Params:  initParams,
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	initResp, err := readJSONRPCResponse(ctx, records)
	if err != nil {
		return nil, nil, fmt.Errorf("read initialize response: %w", err)
	}

	// Extract capabilities and agent info.
	var initResult opencode.InitializeResult
	if initResp.Result != nil {
		if json.Unmarshal(initResp.Result, &initResult) == nil {
			w.supportsImage = initResult.AgentCapabilities.PromptCapabilities.Image
			res.agentVersion = initResult.AgentInfo.Version
		}
	}

	// 2. Create or resume session.
	var sessionReq opencode.JSONRPCRequest
	if opts.ResumeSessionID != "" {
		params, err := marshalParams(opencode.SessionLoadParams{SessionID: opts.ResumeSessionID, Cwd: opts.Dir, McpServers: []opencode.MCPServer{}})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session/load params: %w", err)
		}
		sessionReq = opencode.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  opencode.MethodSessionLoad,
			Params:  params,
		}
	} else {
		params, err := marshalParams(opencode.SessionNewParams{Cwd: opts.Dir, McpServers: []opencode.MCPServer{}})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session/new params: %w", err)
		}
		sessionReq = opencode.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  opencode.MethodSessionNew,
			Params:  params,
		}
	}
	if err := writeJSON(stdin, sessionReq); err != nil {
		return nil, nil, fmt.Errorf("write session/new: %w", err)
	}

	// Read session response.
	resp, err := readJSONRPCResponse(ctx, records)
	if err != nil {
		return nil, nil, fmt.Errorf("read session response: %w", err)
	}

	// Extract session ID and models from result.
	var snResult opencode.SessionNewResult
	if err := json.Unmarshal(resp.Result, &snResult); err != nil {
		return nil, nil, fmt.Errorf("parse session result: %w", err)
	}
	if snResult.SessionID != "" {
		w.sessionID = snResult.SessionID
	} else if opts.ResumeSessionID != "" {
		// session/load doesn't return sessionId in the result.
		w.sessionID = opts.ResumeSessionID
	}
	if w.sessionID == "" {
		return nil, nil, errors.New("session response missing sessionId")
	}
	res.setModels(snResult.Models)
	res.setConfigOptions(snResult.ConfigOptions)

	// 3. Select the requested model and effort using ACP configuration options.
	// Older ACP implementations do not expose configuration options, so model
	// selection falls back to their legacy session/set_model method. Effort has
	// no safe fallback: only ACP tells us which values a model supports.
	model := opts.Model
	if model == "" {
		model = res.currentModel
	}
	if model != "" && model != res.currentModel {
		hasModelConfig := res.configOption(opencode.ConfigOptionModel) != nil
		selected, err := res.setSessionModel(ctx, stdin, records, model)
		if err != nil {
			return nil, nil, err
		}
		if selected && !hasModelConfig {
			res.currentModel = model
		}
	}
	if opts.Effort != "" {
		if err := res.setSessionConfigOption(ctx, stdin, records, opencode.ConfigOptionEffort, opts.Effort); err != nil {
			return nil, nil, err
		}
	}

	return res, records.Reader(), nil
}

func (res *handshakeResult) setModels(models opencode.ModelsInfo) {
	res.currentModel = models.CurrentModelID
}

func (res *handshakeResult) setConfigOptions(options []opencode.SessionConfigOption) {
	res.configOptions = options
	if model := res.configOption(opencode.ConfigOptionModel); model != nil {
		res.currentModel = model.CurrentValue
	}
}

func (res *handshakeResult) configOption(id opencode.ConfigOptionID) *opencode.SessionConfigOption {
	for i := range res.configOptions {
		if res.configOptions[i].ID == id {
			return &res.configOptions[i]
		}
	}
	return nil
}

func (res *handshakeResult) setSessionModel(ctx context.Context, stdin io.Writer, records *agent.RelayRecordReader, model string) (bool, error) {
	if res.configOption(opencode.ConfigOptionModel) != nil {
		if err := res.setSessionConfigOption(ctx, stdin, records, opencode.ConfigOptionModel, model); err != nil {
			return false, err
		}
		return true, nil
	}
	params, err := marshalParams(opencode.SetSessionModelParams{SessionID: res.wire.sessionID, ModelID: model})
	if err != nil {
		return false, fmt.Errorf("marshal session/set_model params: %w", err)
	}
	if err := writeJSON(stdin, opencode.JSONRPCRequest{
		JSONRPC: "2.0", ID: res.wire.allocIDLocked(), Method: opencode.MethodSessionSetModel, Params: params,
	}); err != nil {
		return false, fmt.Errorf("write session/set_model: %w", err)
	}
	if _, err := readJSONRPCResponse(ctx, records); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return false, err
		}
		slog.WarnContext(ctx, "opencode: session/set_model failed, using default model", "err", err, "model", model)
		return false, nil
	}
	return true, nil
}

func (res *handshakeResult) setSessionConfigOption(ctx context.Context, stdin io.Writer, records *agent.RelayRecordReader, id opencode.ConfigOptionID, value string) error {
	option := res.configOption(id)
	if option == nil {
		return fmt.Errorf("opencode ACP does not expose %q for the selected model", id)
	}
	if option.Type != opencode.ConfigOptionTypeSelect {
		return fmt.Errorf("opencode ACP %q option has unsupported type %q", id, option.Type)
	}
	if id != opencode.ConfigOptionModel && !hasConfigValue(option.Options, value) {
		return fmt.Errorf("opencode ACP %q value %q is unavailable", id, value)
	}
	params, err := marshalParams(opencode.SetSessionConfigOptionParams{SessionID: res.wire.sessionID, ConfigID: id, Value: value})
	if err != nil {
		return fmt.Errorf("marshal session/set_config_option params: %w", err)
	}
	if err := writeJSON(stdin, opencode.JSONRPCRequest{
		JSONRPC: "2.0", ID: res.wire.allocIDLocked(), Method: opencode.MethodSessionSetConfigOption, Params: params,
	}); err != nil {
		return fmt.Errorf("write session/set_config_option: %w", err)
	}
	resp, err := readJSONRPCResponse(ctx, records)
	if err != nil {
		return fmt.Errorf("read session/set_config_option response: %w", err)
	}
	var result opencode.SetSessionConfigOptionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse session/set_config_option response: %w", err)
	}
	if len(result.ConfigOptions) == 0 {
		return errors.New("session/set_config_option response missing configOptions")
	}
	res.setConfigOptions(result.ConfigOptions)
	return nil
}

func normalizeModels(models []agent.Model) []agent.Model {
	byID := make(map[string]agent.Model, len(models))
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if _, ok := byID[model.ID]; !ok {
			ids = append(ids, model.ID)
		}
		model.EffortOptions = slices.Clone(model.EffortOptions)
		slices.Sort(model.EffortOptions)
		model.EffortOptions = slices.Compact(model.EffortOptions)
		byID[model.ID] = model
	}

	ids = agent.SortModels(ids)
	normalized := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		normalized = append(normalized, byID[id])
	}
	return normalized
}

func hasConfigValue(values []opencode.ConfigOptionValue, want string) bool {
	for _, value := range values {
		if value.Value == want {
			return true
		}
	}
	return false
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
// and skipped.
func readJSONRPCResponse(ctx context.Context, r *agent.RelayRecordReader) (*opencode.JSONRPCMessage, error) {
	type result struct {
		msg *opencode.JSONRPCMessage
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
			var msg opencode.JSONRPCMessage
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
			slog.DebugContext(ctx, "opencode handshake: skipping notification", "method", msg.Method)
		}
	}()
	select {
	case res := <-ch:
		return res.msg, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("handshake: %w", ctx.Err())
	}
}

func fetchModels(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) ([]agent.Model, error) {
	if target.SSHHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	args := []string{target.SSHHost}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, "opencode", "models", "--refresh", "--verbose")
	out, err := exec.CommandContext(ctx, "ssh", args...).Output() //nolint:gosec // target is not user-controlled
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	models, err := parseModels(out)
	if err != nil {
		return nil, fmt.Errorf("parse opencode models: %w", err)
	}
	return models, nil
}

// parseModels reads the model ID and pretty-printed metadata pairs
// emitted by "opencode models --verbose". OpenCode exposes model variants as
// its ACP effort options.
func parseModels(out []byte) ([]agent.Model, error) {
	type modelInfo struct {
		Variants map[string]json.RawMessage `json:"variants"`
	}

	lines := strings.Split(string(out), "\n")
	models := make([]agent.Model, 0)
	for line := 0; line < len(lines); line++ {
		model := strings.TrimSpace(lines[line])
		provider, id, ok := strings.Cut(model, "/")
		if !ok || provider == "" || id == "" {
			continue // E.g. the colored "Models cache refreshed" status line.
		}

		var data []byte
		var info modelInfo
		for line++; line < len(lines); line++ {
			data = append(data, lines[line]...)
			data = append(data, '\n')
			if err := json.Unmarshal(data, &info); err == nil {
				break
			} else if line == len(lines)-1 {
				return nil, fmt.Errorf("%s metadata: %w", model, err)
			}
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%s missing metadata", model)
		}
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("%s metadata: %w", model, err)
		}

		efforts := make([]string, 0, len(info.Variants))
		for effort := range info.Variants {
			efforts = append(efforts, effort)
		}
		models = append(models, agent.Model{ID: model, EffortOptions: efforts})
	}
	if len(models) == 0 {
		return nil, errors.New("no model metadata")
	}
	return normalizeModels(models), nil
}

// extractParams extracts the raw "params" field from a JSON-RPC message.
func extractParams(line []byte) (json.RawMessage, error) {
	var p opencode.ParamsProbe
	if err := json.Unmarshal(line, &p); err != nil {
		return nil, err
	}
	return p.Params, nil
}
