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
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// Backend implements agent.Backend for OpenCode using the ACP JSON-RPC 2.0
// protocol.
type Backend struct {
	agent.Base
	mu sync.Mutex
}

var _ agent.Backend = (*Backend)(nil)

// NewParser implements agent.Backend.
func (*Backend) NewParser() func([]byte) ([]agent.Message, error) {
	fw := &jsonutil.FieldWarner{}
	return func(line []byte) ([]agent.Message, error) { return parseMessage(line, fw) }
}

// New creates an OpenCode backend with parser configured.
// ModelList starts with common defaults; it is replaced with the live list
// returned by session/new on the first successful handshake.
func New() *Backend {
	return &Backend{Base: agent.Base{
		HarnessID:     agent.OpenCode,
		ModelList:     []string{"anthropic/claude-sonnet-4"},
		Images:        true,
		ContextWindow: 200_000,
	}}
}

// Models returns the current model list, updated dynamically after each handshake.
func (b *Backend) Models() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ModelList
}

// Start launches an OpenCode ACP process via the relay daemon in the given
// container. It performs the JSON-RPC handshake (initialize → session/new)
// before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	if err := agent.DeployRelay(ctx, opts.Container); err != nil {
		return nil, err
	}

	ocArgs := []string{"opencode", "acp"}

	sshArgs := make([]string, 0, 8+len(ocArgs))
	sshArgs = append(sshArgs, opts.Container, "python3", agent.RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin", "--")
	sshArgs = append(sshArgs, ocArgs...)

	slog.Debug("relay", "msg", "launch", "ctr", opts.Container, "args", ocArgs)
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

	hs, err := handshake(ctx, stdin, br, opts)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("opencode handshake: %w", err)
	}
	if len(hs.models) > 0 {
		b.mu.Lock()
		b.ModelList = hs.models
		b.mu.Unlock()
	}

	log := slog.With("ctr", opts.Container)
	s := agent.NewSession(cmd, stdin, br, msgCh, logW, hs.wire, log)

	// Emit InitMessage so the task captures session ID, model, and version.
	initMsg := &agent.InitMessage{
		SessionID: hs.wire.sessionID,
		Model:     hs.currentModel,
		Version:   hs.agentVersion,
	}
	msgCh <- initMsg
	// Persist a synthetic caic_init line to output.jsonl so replay
	// reconstructs the InitMessage (handshake responses aren't logged).
	if logW != nil {
		if data, err := json.Marshal(caicInit{
			Type:      "caic_init",
			SessionID: initMsg.SessionID,
			Model:     initMsg.Model,
			Version:   initMsg.Version,
		}); err == nil {
			_, _ = logW.Write(append(data, '\n'))
		}
	}

	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

// ReadRelayOutput reads relay output using a fresh wireFormat.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	wire := &wireFormat{fw: &jsonutil.FieldWarner{}}
	return agent.ReadRelayOutput(ctx, container, wire.ParseMessage)
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	wire := &wireFormat{sessionID: opts.ResumeSessionID, fw: &jsonutil.FieldWarner{}}
	return agent.AttachRelaySession(ctx, opts.Container, opts.RelayOffset, msgCh, logW, wire)
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

// allocID returns the next JSON-RPC request ID. Not thread-safe; callers
// must hold mu or be in the single-threaded handshake phase.
func (w *wireFormat) allocIDLocked() int64 {
	w.nextID++
	return w.nextID
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
	content := make([]promptContent, 0, 1+len(p.Images))
	content = append(content, promptContent{Type: ContentText, Text: p.Text})
	if w.supportsImage {
		for _, img := range p.Images {
			content = append(content, promptContent{
				Type:     ContentImage,
				Data:     img.Data,
				MimeType: img.MediaType,
			})
		}
	}
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  MethodSessionPrompt,
		Params:  sessionPromptParams{SessionID: w.sessionID, Prompt: content},
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
	var probe messageProbe
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
	if probe.Method == MethodSessionUpdate {
		params, err := extractParams(line)
		if err != nil {
			return nil, fmt.Errorf("extract params: %w", err)
		}
		var sup SessionUpdateParams
		if err := json.Unmarshal(params, &sup); err == nil {
			var uprobe updateProbe
			if err := json.Unmarshal(sup.Update, &uprobe); err == nil && uprobe.SessionUpdate == UpdateUsageUpdate {
				var u UsageUpdateUpdate
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

// handlePromptResponseLocked converts a session/prompt JSON-RPC response into
// a ResultMessage with accumulated usage. Emits synthetic TextMessage and
// ThinkingMessage from accumulated deltas before the ResultMessage.
// Must not be called under mu.
func (w *wireFormat) handlePromptResponseLocked(line []byte) ([]agent.Message, error) {
	var resp JSONRPCMessage
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
		var pr promptResult
		if err := json.Unmarshal(resp.Result, &pr); err == nil {
			if pr.StopReason == "cancelled" || pr.StopReason == "refusal" {
				rm.IsError = true
				rm.Result = pr.StopReason
			}
			rm.Usage = agent.Usage{
				InputTokens:              pr.Usage.InputTokens,
				OutputTokens:             pr.Usage.OutputTokens,
				CacheReadInputTokens:     pr.Usage.CachedReadTokens,
				CacheCreationInputTokens: pr.Usage.CachedWriteTokens,
				ReasoningOutputTokens:    pr.Usage.ThoughtTokens,
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
	initReq := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      w.allocIDLocked(),
		Method:  MethodInitialize,
		Params: initializeParams{
			ProtocolVersion: 1,
			ClientCapabilities: clientCapabilities{
				Terminal: false,
			},
			ClientInfo: clientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
		},
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
	var initResult initializeResult
	if initResp.Result != nil {
		if json.Unmarshal(initResp.Result, &initResult) == nil {
			w.supportsImage = initResult.AgentCapabilities.PromptCapabilities.Image
			res.agentVersion = initResult.AgentInfo.Version
		}
	}

	// 2. Create or resume session.
	var sessionReq jsonrpcRequest
	if opts.ResumeSessionID != "" {
		sessionReq = jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  MethodSessionLoad,
			Params:  sessionLoadParams{SessionID: opts.ResumeSessionID, Cwd: opts.Dir, McpServers: []mcpServer{}},
		}
	} else {
		sessionReq = jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  MethodSessionNew,
			Params:  sessionNewParams{Cwd: opts.Dir, McpServers: []mcpServer{}},
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
	var snResult sessionNewResult
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
		setModelReq := jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      w.allocIDLocked(),
			Method:  MethodUnstableSetSessionModel,
			Params:  setSessionModelParams{SessionID: w.sessionID, ModelID: opts.Model},
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

// extractParams extracts the raw "params" field from a JSON-RPC message.
func extractParams(line []byte) (json.RawMessage, error) {
	var p paramsProbe
	if err := json.Unmarshal(line, &p); err != nil {
		return nil, err
	}
	return p.Params, nil
}
