// Package opencode implements agent.Backend for OpenCode via ACP
// (Agent Client Protocol): JSON-RPC 2.0 over stdin/stdout.
package opencode

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
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Backend implements agent.Backend for OpenCode using the ACP JSON-RPC 2.0
// protocol.
type Backend struct {
	agent.Base

	mu      sync.Mutex
	cache   *agent.HarnessCache
	EnvVars []string // KEY=VALUE pairs for FetchModels SSH commands
}

var _ agent.Backend = (*Backend)(nil)
var _ agent.ModelFetcher = (*Backend)(nil)
var _ agent.RecordHandshaker = (*Backend)(nil)

// New creates an OpenCode backend with parser configured. If cacheDir is
// non-empty, the model list is loaded from the on-disk harness cache.
// envVars are KEY=VALUE pairs passed to FetchModels SSH commands.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{EnvVars: envVars}
	b.Base = agent.Base{
		HarnessID:     harness.OpenCode,
		ModelList:     []string{"anthropic/claude-sonnet-4"},
		Images:        true,
		Compact:       true,
		ContextWindow: 200_000,
	}
	if cacheDir != "" {
		b.cache = agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
		if models, _ := b.cache.Models(harness.OpenCode, agent.APIKeyHash(envVars)); len(models) > 0 {
			b.ModelList = agent.SortModels(models)
		}
	}
	return b
}

// RecordHandshake performs the ACP handshake (initialize → session/new →
// optional set_model) over stdin/stdout for record-trace golden-file
// generation. It returns a populated wireFormat and a buffered reader that
// replaces the original stdout for subsequent reads.
func (b *Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.Reader, model string) (agent.WireFormat, io.Reader, error) {
	br := bufio.NewReaderSize(stdout, 1<<16)
	hs, err := handshake(ctx, stdin, br, &agent.Options{Dir: "/workspace", Model: model})
	if err != nil {
		return nil, nil, err
	}
	return hs.wire, br, nil
}

// Models returns the current model list, updated dynamically after each handshake.
func (b *Backend) Models() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ModelList
}

// SetModels replaces the model list with sorted models. Thread-safe.
func (b *Backend) SetModels(models []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ModelList = agent.SortModels(models)
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
	if err := agent.DeployRelay(ctx, opts.Target); err != nil {
		return nil, err
	}

	ocArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})

	sshArgs := make([]string, 0, 8+len(ocArgs))
	sshArgs = append(sshArgs, sshHost, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin", "--")
	sshArgs = append(sshArgs, ocArgs...)

	slog.Debug("relay", "msg", "launch", "target", sshHost, "args", ocArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &agent.SlogWriter{Prefix: "relay serve-attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(stdout, 1<<16)

	hs, err := handshake(ctx, stdin, br, opts)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("opencode handshake: %w", err)
	}
	if len(hs.models) > 0 {
		b.mu.Lock()
		b.ModelList = agent.SortModels(hs.models)
		b.mu.Unlock()
		if b.cache != nil {
			b.cache.SetModels(harness.OpenCode, hs.models, agent.APIKeyHash(b.EnvVars))
		}
	}

	log := slog.With("target", sshHost)
	c := agent.NewConn(stdin, opts.LogW, hs.wire)
	s := agent.NewSession(cmd, c, br, opts.MsgCh, log)

	// Emit InitMessage so the task captures session ID, model, and version.
	initMsg := &agent.InitMessage{
		SessionID: hs.wire.sessionID,
		Model:     hs.currentModel,
		Version:   hs.agentVersion,
	}
	opts.MsgCh <- initMsg
	if err := agent.WriteMetaSession(opts.LogW, initMsg); err != nil {
		return nil, fmt.Errorf("write session metadata: %w", err)
	}

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
	wire := &wireFormat{sessionID: opts.ResumeSessionID, fw: &jsonutil.FieldWarner{}}
	return agent.AttachRelaySession(ctx, opts, wire)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	return &wireFormat{fw: &jsonutil.FieldWarner{}}
}

// TODO: Trim caicInit after 2026-08 once legacy caic_init logs are old enough to ignore.
// caicInit is the legacy pre-caic_session metadata record.
type caicInit struct {
	Type      string `json:"type"` // always "caic_init"
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitzero"`
	Version   string `json:"version,omitzero"`
}

// wireFormat implements agent.WireFormat for the ACP JSON-RPC protocol.
// It holds per-session state: the session ID, a request ID counter,
// accumulated token usage, and image support flag.
type wireFormat struct {
	sessionID     string // Set during handshake; read-only after.
	supportsImage bool   // Set during handshake; read-only after.

	mu          sync.Mutex
	nextID      int64
	promptReqID int64 // JSON-RPC ID of the current session/prompt request.
	totalUsage  agent.Usage
	textAccum   strings.Builder // Accumulated text from agent_message_chunk.
	thinkAccum  strings.Builder // Accumulated text from agent_thought_chunk.
	fw          *jsonutil.FieldWarner
}

// WritePrompt sends a session/prompt JSON-RPC request to begin a new turn.
func (w *wireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sessionID == "" {
		return errors.New("opencode: no session ID (handshake not completed)")
	}
	id := w.allocIDLocked()
	w.promptReqID = id
	w.textAccum.Reset()
	w.thinkAccum.Reset()
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
func (w *wireFormat) WriteCompact(wr io.Writer, _ string, logW io.Writer) error {
	return w.WritePrompt(wr, agent.Prompt{Text: "/compact"}, logW)
}

// ParseMessage wraps the package-level parseMessage with interceptions:
//
//   - usage_update → emits UsageMessage and accumulates into totalUsage.
//   - session/request_permission → auto-approves with "allow_once".
//   - ResultMessage has Usage populated from totalUsage, then totalUsage resets.
//
// It also captures the session ID from InitMessage if present.
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	var probe opencode.MessageProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// Intercept session/prompt response → ResultMessage.
	if probe.ID != nil {
		var id int64
		if json.Unmarshal(probe.ID, &id) == nil {
			w.mu.Lock()
			isPromptResp := id == w.promptReqID
			w.mu.Unlock()
			if isPromptResp {
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

	msgs, err := parseMessage(line, w.fw)
	if err != nil {
		return nil, err
	}
	// Accumulate text/thinking deltas for synthetic final messages.
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.TextDeltaMessage:
			w.mu.Lock()
			w.textAccum.WriteString(m.Text)
			w.mu.Unlock()
		case *agent.ThinkingDeltaMessage:
			w.mu.Lock()
			w.thinkAccum.WriteString(m.Text)
			w.mu.Unlock()
		}
	}
	return msgs, nil
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
			// OpenCode doesn't report cache TTL; default to 5 minutes.
			if pr.Usage.InputTokens > 0 || pr.Usage.OutputTokens > 0 {
				rm.Usage = agent.Usage{
					InputTokens:              pr.Usage.InputTokens,
					OutputTokens:             pr.Usage.OutputTokens,
					CacheReadInputTokens:     pr.Usage.CachedReadTokens,
					CacheCreationInputTokens: pr.Usage.CachedWriteTokens,
					ReasoningOutputTokens:    pr.Usage.ThoughtTokens,
					CacheTTLSeconds:          300,
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
	if w.thinkAccum.Len() > 0 {
		msgs = append(msgs, &agent.ThinkingMessage{Text: w.thinkAccum.String()})
		w.thinkAccum.Reset()
	}
	if w.textAccum.Len() > 0 {
		msgs = append(msgs, &agent.TextMessage{Text: w.textAccum.String()})
		w.textAccum.Reset()
	}
	w.mu.Unlock()
	msgs = append(msgs, rm)
	return msgs, nil
}

// handshakeResult bundles everything returned by a successful handshake.
type handshakeResult struct {
	wire         *wireFormat
	models       []string // All available model IDs (current first).
	currentModel string   // Model ID the session is using.
	agentVersion string   // Agent version string from initialize.
}

// handshake performs the ACP initialize → session/new sequence and returns
// a handshakeResult with the wireFormat, model list, and agent metadata.
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, opts *agent.Options) (*handshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	w := &wireFormat{fw: &jsonutil.FieldWarner{}}
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
		return nil, fmt.Errorf("marshal initialize params: %w", err)
	}
	initReq := opencode.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      w.allocIDLocked(),
		Method:  opencode.MethodInitialize,
		Params:  initParams,
	}
	if err := writeJSON(stdin, initReq); err != nil {
		return nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	initResp, err := readJSONRPCResponse(ctx, stdout)
	if err != nil {
		return nil, fmt.Errorf("read initialize response: %w", err)
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
			return nil, fmt.Errorf("marshal session/load params: %w", err)
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
			return nil, fmt.Errorf("marshal session/new params: %w", err)
		}
		sessionReq = opencode.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  opencode.MethodSessionNew,
			Params:  params,
		}
	}
	if err := writeJSON(stdin, sessionReq); err != nil {
		return nil, fmt.Errorf("write session/new: %w", err)
	}

	// Read session response.
	resp, err := readJSONRPCResponse(ctx, stdout)
	if err != nil {
		return nil, fmt.Errorf("read session response: %w", err)
	}

	// Extract session ID and models from result.
	var snResult opencode.SessionNewResult
	if err := json.Unmarshal(resp.Result, &snResult); err != nil {
		return nil, fmt.Errorf("parse session result: %w", err)
	}
	if snResult.SessionID != "" {
		w.sessionID = snResult.SessionID
	} else if opts.ResumeSessionID != "" {
		// session/load doesn't return sessionId in the result.
		w.sessionID = opts.ResumeSessionID
	}
	if w.sessionID == "" {
		return nil, errors.New("session response missing sessionId")
	}
	// Put the current model first so the frontend shows it as default.
	res.currentModel = snResult.Models.CurrentModelID
	if res.currentModel != "" {
		res.models = append(res.models, res.currentModel)
	}
	for _, m := range snResult.Models.AvailableModels {
		if m.ModelID != "" && m.ModelID != res.currentModel {
			res.models = append(res.models, m.ModelID)
		}
	}

	// 3. Switch model if the caller requested a specific one.
	if opts.Model != "" {
		params, err := marshalParams(opencode.SetSessionModelParams{SessionID: w.sessionID, ModelID: opts.Model})
		if err != nil {
			return nil, fmt.Errorf("marshal unstable_setSessionModel params: %w", err)
		}
		setModelReq := opencode.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  opencode.MethodUnstableSetSessionModel,
			Params:  params,
		}
		if err := writeJSON(stdin, setModelReq); err != nil {
			return nil, fmt.Errorf("write unstable_setSessionModel: %w", err)
		}
		resp, err := readJSONRPCResponse(ctx, stdout)
		if err != nil {
			// Log and continue — model switch is best-effort. The agent
			// may not support the unstable method yet.
			slog.Warn("opencode: unstable_setSessionModel failed, using default model", "err", err, "model", opts.Model)
		} else {
			_ = resp // success; model has been switched
			res.currentModel = opts.Model
		}
	}

	return res, nil
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
func readJSONRPCResponse(ctx context.Context, r *bufio.Reader) (*opencode.JSONRPCMessage, error) {
	type result struct {
		msg *opencode.JSONRPCMessage
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
			slog.Debug("opencode handshake: skipping notification", "method", msg.Method)
		}
	}()
	select {
	case res := <-ch:
		return res.msg, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("handshake: %w", ctx.Err())
	}
}

// FetchModels implements agent.ModelFetcher.
func (*Backend) FetchModels(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) ([]string, error) {
	return FetchModels(ctx, target, extraEnv)
}

// FetchModels runs "opencode models" in the target and
// returns the model ID list (one per line).
// extraEnv holds KEY=VALUE pairs injected via the env command.
func FetchModels(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) ([]string, error) {
	if target.SSHHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	args := []string{target.SSHHost}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, "opencode", "models")
	out, err := exec.CommandContext(ctx, "ssh", args...).Output() //nolint:gosec // target is not user-controlled
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	var models []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if m := strings.TrimSpace(line); m != "" {
			models = append(models, m)
		}
	}
	return models, nil
}

// extractParams extracts the raw "params" field from a JSON-RPC message.
func extractParams(line []byte) (json.RawMessage, error) {
	var p opencode.ParamsProbe
	if err := json.Unmarshal(line, &p); err != nil {
		return nil, err
	}
	return p.Params, nil
}
