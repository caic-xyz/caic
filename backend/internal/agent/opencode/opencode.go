// Package opencode implements ACP sessions with shared directional metadata and task-scoped MCP.
// ACP (Agent Client Protocol) uses JSON-RPC 2.0 over stdin/stdout.
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
	"time"

	"github.com/maruel/genai/acp"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai/providers/opencode"
	"github.com/maruel/gomode/mcp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Backend implements agent.Backend for OpenCode using the ACP JSON-RPC 2.0
// protocol.
type Backend struct {
	agent.Base
}

// New creates an OpenCode backend with parser configured.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID: harness.OpenCode,
		Images:    true,
		Compact:   true,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.OpenCode, envVars))
	return b
}

// RecordHandshake performs ACP setup using the recording's continuous decoder.
func (*Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.ReadCloser, opts *agent.Options) (io.Reader, error) {
	_, continuation, err := handshake(ctx, stdin, bufio.NewReaderSize(stdout, 1<<16), stdout, opts)
	return continuation, err
}

// SetModelInventory implements agent.Backend, normalizing model names before
// storing them via the embedded Base (which owns the concurrency-safe storage).
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	b.Base.SetModelInventory(agent.ModelInventory{Models: normalizeModels(inventory.Models)})
}

// Start launches an OpenCode ACP process via the relay daemon in the given
// container. It performs the JSON-RPC handshake (initialize → session/new)
// before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (session *agent.Session, err error) {
	ocArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})
	relayArgs := []string{"--harness", "opencode"}
	if opts.MCP != nil {
		relayArgs = append(relayArgs, "--caic-mcp")
	}
	rp, err := agent.PrepareRelay(ctx, opts, relayArgs, ocArgs)
	if err != nil {
		return nil, err
	}
	bootstrap := true
	defer func() {
		if bootstrap {
			closeErr := errors.Join(rp.Stdin.Close(), rp.Stdout.Close())
			_ = rp.Cmd.Process.Kill()
			err = errors.Join(err, closeErr, rp.Cmd.Wait())
		}
	}()

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(rp.Stdout, 1<<16)

	_, continuation, err := handshake(ctx, rp.Stdin, br, rp.Stdout, opts)
	if err != nil {
		return nil, fmt.Errorf("opencode handshake: %w", err)
	}

	log := opts.Logger.With("target", opts.Target.SSHHost)
	c := agent.NewConn(ctx, log, rp.Stdin, opts.Records, opts.MCP)
	bootstrap = false
	s := agent.NewSession(ctx, rp.Cmd, c, agent.ContinuedReader{Reader: continuation, Closer: rp.Stdout}, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.SendPrompt(opts.InitialPrompt); err != nil {
			s.Cancel()
			return nil, fmt.Errorf("write prompt: %w", errors.Join(err, s.Wait()))
		}
	}
	return s, nil
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(_ agent.HarnessArgs) []string {
	return []string{"opencode", "acp"}
}

// AttachRelay connects after the owning record processor has replayed history.
func (*Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, nil)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Schema drift is checked offline by check-agent-logs.
	return &wireFormat{nativeSubagents: nativeSubagents{tasks: make(map[string]agent.NativeSubagent), taskTools: make(map[string]struct{})}}
}

// NewMetadataParser implements agent.Backend with an independent metadata projection.
func (*Backend) NewMetadataParser() agent.MessageParser { return &metadataState{} }

// FetchModelInventory implements agent.ModelFetcher.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModels(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return agent.ModelInventory{Models: models}, nil
}

var (
	_ agent.Backend          = (*Backend)(nil)
	_ agent.ModelFetcher     = (*Backend)(nil)
	_ agent.RecordHandshaker = (*Backend)(nil)
)

// maxAccumulatedOutputBytes bounds synthetic final-message state. Streaming
// deltas are already persisted independently, so an overflow disables the
// synthetic final rather than retaining an incomplete duplicate.
const maxAccumulatedOutputBytes = 1 << 20

// wireFormat implements agent.WireFormat for the ACP JSON-RPC protocol.
// It holds per-session state: the session ID, a request ID counter,
// prompt state and image support flag.
type wireFormat struct {
	metadataState

	nativeSubagents nativeSubagents
	supportsImage   bool // Set during handshake; read-only after.

	nextID         int64
	skillReadCalls map[string]struct{} // active tool calls whose skill read was emitted
	textAccum      strings.Builder     // Accumulated text from agent_message_chunk.
	thinkAccum     strings.Builder     // Accumulated text from agent_thought_chunk.
	textOverflow   bool
	thinkOverflow  bool
}

// EncodePrompt encodes a session/prompt JSON-RPC request to begin a new turn.
func (w *wireFormat) EncodePrompt(p agent.Prompt) ([]byte, error) {
	w.mu.Lock()
	if w.session.SessionID == "" {
		w.mu.Unlock()
		return nil, errors.New("opencode: no session ID (handshake not completed)")
	}
	id := w.allocIDLocked()
	sessionID, supportsImage := w.session.SessionID, w.supportsImage
	w.mu.Unlock()
	content := make([]acp.PromptContent, 0, 1+len(p.Images))
	content = append(content, acp.PromptContent{Type: acp.ContentText, Text: p.Text})
	if supportsImage {
		for _, img := range p.Images {
			content = append(content, acp.PromptContent{
				Type:     acp.ContentImage,
				Data:     img.Data,
				MimeType: img.MediaType,
			})
		}
	}
	params, err := marshalParams(acp.SessionPromptParams{SessionID: sessionID, Prompt: content})
	if err != nil {
		return nil, fmt.Errorf("marshal session/prompt params: %w", err)
	}
	req := acp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  acp.MethodSessionPrompt,
		Params:  params,
	}
	return marshalInput(req)
}

// EncodeCompact implements agent.CompactCommand by sending /compact as a prompt.
// OpenCode recognizes this as a slash command via session/prompt.
func (w *wireFormat) EncodeCompact(_ string) ([]byte, error) {
	return w.EncodePrompt(agent.Prompt{Text: "/compact"})
}

// ParseInput restores prompt response correlation and decodes recorded client prompts.
func (w *wireFormat) ParseInput(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, fmt.Errorf("unmarshal input: %w", err)
	}
	if _, err := w.metadataState.ParseInput(line); err != nil {
		return nil, err
	}
	if probe.ID != nil && string(probe.ID) != "null" && probe.Method != "" {
		var id int64
		if err := json.Unmarshal(probe.ID, &id); err != nil {
			return nil, fmt.Errorf("unmarshal request ID: %w", err)
		}
		w.mu.Lock()
		w.nextID = max(w.nextID, id)
		w.mu.Unlock()
		if acp.Method(probe.Method) == acp.MethodSessionPrompt {
			w.mu.Lock()
			w.resetAccumulatedOutputLocked()
			w.mu.Unlock()
			var prompt acp.SessionPromptParams
			if err := json.Unmarshal(probe.Params, &prompt); err != nil {
				return nil, fmt.Errorf("unmarshal session/prompt params: %w", err)
			}
			input := &agent.UserInputMessage{}
			for i := range prompt.Prompt {
				content := &prompt.Prompt[i]
				switch content.Type {
				case acp.ContentText:
					input.Text += content.Text
				case acp.ContentImage:
					input.Images = append(input.Images, v3.ImageData{MediaType: content.MimeType, Data: content.Data})
				case acp.ContentAudio, acp.ContentResource, acp.ContentResourceLink:
					// UserInputMessage currently has no representation for these ACP content types.
				}
			}
			return []agent.Message{
				input,
				&agent.RawMessage{MessageType: "jsonrpc_request", Raw: append([]byte(nil), line...)},
			}, nil
		}
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_request", Raw: append([]byte(nil), line...)}}, nil
	}
	return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil
}

// ParseOutput wraps the package-level parseMessage with interceptions:
//
//   - session/request_permission → auto-approves with "allow_once".
//   - prompt responses → emits final Text/Thinking messages and ResultMessage.
//
// It also captures the session ID from InitMessage if present.
func (w *wireFormat) ParseOutput(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	if probe.Method == "" && probe.ID != nil {
		var id int64
		if err := json.Unmarshal(probe.ID, &id); err != nil {
			return nil, err
		}
		w.mu.Lock()
		request, matched := w.requests[id]
		w.mu.Unlock()
		if matched && request.method == acp.MethodInitialize {
			result, err := agent.JSONField(line, "result")
			if err != nil {
				return nil, err
			}
			if len(result) > 0 && string(result) != "null" {
				var initialized acp.InitializeResult
				if err := json.Unmarshal(result, &initialized); err != nil {
					return nil, err
				}
				w.mu.Lock()
				w.supportsImage = initialized.AgentCapabilities.PromptCapabilities.Image
				w.mu.Unlock()
			}
		}
	}
	metadata, handled, err := w.parseOutput(line, &probe)
	if err != nil {
		return nil, err
	}
	if handled {
		if len(metadata) > 0 {
			if result, ok := metadata[0].(*agent.ResultMessage); ok {
				return w.finishPrompt(result), nil
			}
		}
		return metadata, nil
	}
	if probe.ID != nil && string(probe.ID) != "null" && probe.Method == "" {
		return []agent.Message{&agent.RawMessage{MessageType: "jsonrpc_response", Raw: append([]byte(nil), line...)}}, nil
	}

	msgs, toolCall, err := parseMessage(line, &probe)
	if err != nil {
		return nil, err
	}
	if toolCall != nil && toolCall.ToolCallID != "" {
		w.mu.Lock()
		switch toolCall.Status {
		case acp.StatusInProgress:
			if _, seen := w.skillReadCalls[toolCall.ToolCallID]; seen {
				msgs = slices.DeleteFunc(msgs, func(m agent.Message) bool {
					switch m.(type) {
					case *agent.SkillReadMessage, *agent.SkillLoadMessage:
						return true
					}
					return false
				})
			} else {
				for _, m := range msgs {
					if _, ok := m.(*agent.SkillReadMessage); ok {
						if w.skillReadCalls == nil {
							w.skillReadCalls = make(map[string]struct{})
						}
						w.skillReadCalls[toolCall.ToolCallID] = struct{}{}
						break
					}
				}
			}
		case acp.StatusCompleted, acp.StatusFailed:
			delete(w.skillReadCalls, toolCall.ToolCallID)
		case acp.StatusPending:
			// The initial announcement has no input to classify.
		}
		w.mu.Unlock()
	}
	native, err := w.nativeSubagents.parse(toolCall)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, native...)
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

// allocIDLocked returns the next JSON-RPC request ID. Not thread-safe; callers
// must hold mu or be in the single-threaded handshake phase.
func (w *wireFormat) allocIDLocked() int64 {
	w.nextID++
	return w.nextID
}

// finishPrompt emits consolidated output before the normalized terminal report.
func (w *wireFormat) finishPrompt(rm *agent.ResultMessage) []agent.Message {
	// Emit synthetic final messages from accumulated deltas, then reset.
	w.mu.Lock()
	var msgs []agent.Message
	if !w.thinkOverflow && w.thinkAccum.Len() > 0 {
		msgs = append(msgs, &agent.ThinkingMessage{Text: w.thinkAccum.String()})
	}
	if !w.textOverflow && w.textAccum.Len() > 0 {
		msgs = append(msgs, &agent.TextMessage{Text: w.textAccum.String()})
	}
	w.resetAccumulatedOutputLocked()
	clear(w.skillReadCalls)
	w.mu.Unlock()
	msgs = append(msgs, rm)
	return msgs
}

// handshakeResult bundles everything returned by a successful handshake.
type handshakeResult struct {
	sessionID     string
	nextID        int64
	currentModel  string // Model ID the session is using.
	currentEffort string // Reasoning effort the session is using.
	agentVersion  string // Agent version string from initialize.
	configOptions []acp.SessionConfigOption
}

// handshake performs the ACP initialize → session/new sequence and returns
// the configuration needed to select requested settings.
func handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, closer io.Closer, opts *agent.Options) (*handshakeResult, *bufio.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stopRead := agent.InterruptReadOnCancel(ctx, closer)
	defer stopRead()

	records := agent.NewRelayRecordReader(ctx, stdout, opts.Records)
	res := &handshakeResult{}

	// 1. Send initialize request.
	initParams, err := marshalParams(acp.InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: acp.ClientCapabilities{
			Terminal: false,
		},
		ClientInfo: acp.ClientInfo{Name: "caic", Title: "caic", Version: "1.0.0"},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal initialize params: %w", err)
	}
	initReq := acp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      res.allocID(),
		Method:  acp.MethodInitialize,
		Params:  initParams,
	}
	if err := writeJSONInput(ctx, stdin, initReq, opts.Records); err != nil {
		return nil, nil, fmt.Errorf("write initialize: %w", err)
	}

	// Read initialize response.
	handleMCP := func(req agent.MCPRequest) error {
		result, resultErr := agent.MCPRequestResult(ctx, opts.MCP, req)
		response, err := agent.MCPResponseBytes(req.ID, result, resultErr)
		if err != nil {
			return err
		}
		_, err = stdin.Write(response)
		return err
	}

	initResp, err := readJSONRPCResponse(ctx, records, initReq.ID, handleMCP)
	if err != nil {
		return nil, nil, fmt.Errorf("read initialize response: %w", err)
	}

	// Extract capabilities and agent info.
	var initResult acp.InitializeResult
	if initResp.Result != nil {
		if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
			return nil, nil, fmt.Errorf("parse initialize result: %w", err)
		}
		metadata, err := sessionResultMetadata(acp.MethodInitialize, "", initResp.Result)
		if err != nil {
			return nil, nil, err
		}
		res.agentVersion = metadata.AgentVersion
	}

	// 2. Create or resume session. Use ACP's native MCP registration so user
	// configuration, including inline JSONC, stays untouched.
	servers := []acp.MCPServer{}
	if opts.MCP != nil {
		servers = append(servers, acp.MCPServer{
			Name:    "caic",
			Command: "python3",
			Args:    []string{agent.RelayScriptPath, "caic-mcp"},
			Env:     []acp.EnvVariable{{Name: "CAIC_RELAY_DIR", Value: agent.RelayDir}},
		})
	}
	var sessionReq acp.JSONRPCRequest
	if opts.ResumeSessionID != "" {
		// session/load reads local session data under ~/.local/share/opencode/
		// by default. The ID alone cannot recover missing history. md mounts
		// OpenCode's data directory into new containers; custom storage must
		// also be accessible. The load request supplies this task's workspace.
		// https://opencode.ai/docs/troubleshooting/#storage
		params, err := marshalParams(acp.SessionLoadParams{SessionID: opts.ResumeSessionID, Cwd: opts.Dir, McpServers: servers})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session/load params: %w", err)
		}
		sessionReq = acp.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      res.allocID(),
			Method:  acp.MethodSessionLoad,
			Params:  params,
		}
	} else {
		params, err := marshalParams(acp.SessionNewParams{Cwd: opts.Dir, McpServers: servers})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session/new params: %w", err)
		}
		sessionReq = acp.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      res.allocID(),
			Method:  acp.MethodSessionNew,
			Params:  params,
		}
	}
	if err := writeJSONInput(ctx, stdin, sessionReq, opts.Records); err != nil {
		return nil, nil, fmt.Errorf("write session/new: %w", err)
	}

	// Read session response.
	resp, err := readJSONRPCResponse(ctx, records, sessionReq.ID, handleMCP)
	if err != nil {
		return nil, nil, fmt.Errorf("read session response: %w", err)
	}

	// Extract session ID and models from result.
	var snResult acp.SessionNewResult
	if err := json.Unmarshal(resp.Result, &snResult); err != nil {
		return nil, nil, fmt.Errorf("parse session result: %w", err)
	}
	metadata, err := sessionResultMetadata(sessionReq.Method, opts.ResumeSessionID, resp.Result)
	if err != nil {
		return nil, nil, fmt.Errorf("session metadata: %w", err)
	}
	res.sessionID = metadata.SessionID
	res.currentModel, res.currentEffort = metadata.ReportedModel, metadata.ReportedEffort
	if err := res.setConfigOptions(snResult.ConfigOptions); err != nil {
		return nil, nil, err
	}

	// 3. Select the requested model and effort using ACP configuration options.
	model := opts.Model
	if model == "" {
		model = res.currentModel
	}
	if model != "" && model != res.currentModel {
		if err := res.setSessionConfigOption(ctx, stdin, records, acp.ConfigOptionModel, model, opts.Records, handleMCP); err != nil {
			return nil, nil, err
		}
	}
	if opts.Effort != "" {
		if err := res.setSessionConfigOption(ctx, stdin, records, opencode.ConfigOptionEffort, opts.Effort, opts.Records, handleMCP); err != nil {
			return nil, nil, err
		}
	}

	return res, records.Reader(), nil
}

func (res *handshakeResult) setConfigOptions(options []acp.SessionConfigOption) error {
	res.configOptions = options
	m := &agent.SessionMetadata{}
	for i := range options {
		option := &options[i]
		if err := applyConfigValue(m, option.ID, option.CurrentValue); err != nil {
			return err
		}
	}
	if m.ReportedModel != "" {
		res.currentModel = m.ReportedModel
	}
	if m.ReportedEffort != "" {
		res.currentEffort = m.ReportedEffort
	}
	return nil
}

func (res *handshakeResult) configOption(id acp.ConfigOptionID) *acp.SessionConfigOption {
	for i := range res.configOptions {
		if res.configOptions[i].ID == id {
			return &res.configOptions[i]
		}
	}
	return nil
}

func (res *handshakeResult) setSessionConfigOption(ctx context.Context, stdin io.Writer, records *agent.RelayRecordReader, id acp.ConfigOptionID, value string, owner *agent.Records, handleMCP func(agent.MCPRequest) error) error {
	option := res.configOption(id)
	if option == nil {
		return fmt.Errorf("opencode ACP does not expose %q for the selected model", id)
	}
	if option.Type != acp.ConfigOptionTypeSelect {
		return fmt.Errorf("opencode ACP %q option has unsupported type %q", id, option.Type)
	}
	if id != acp.ConfigOptionModel && !hasConfigValue(option.Options, value) {
		return fmt.Errorf("opencode ACP %q value %q is unavailable", id, value)
	}
	params, err := marshalParams(acp.SetSessionConfigOptionParams{SessionID: res.sessionID, ConfigID: id, Value: value})
	if err != nil {
		return fmt.Errorf("marshal session/set_config_option params: %w", err)
	}
	requestID := res.allocID()
	if err := writeJSONInput(ctx, stdin, acp.JSONRPCRequest{
		JSONRPC: "2.0", ID: requestID, Method: acp.MethodSessionSetConfigOption, Params: params,
	}, owner); err != nil {
		return fmt.Errorf("write session/set_config_option: %w", err)
	}
	resp, err := readJSONRPCResponse(ctx, records, requestID, handleMCP)
	if err != nil {
		return fmt.Errorf("read session/set_config_option response: %w", err)
	}
	var result acp.SetSessionConfigOptionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse session/set_config_option response: %w", err)
	}
	if len(result.ConfigOptions) == 0 {
		return errors.New("session/set_config_option response missing configOptions")
	}
	return res.setConfigOptions(result.ConfigOptions)
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

func hasConfigValue(values []acp.ConfigOptionValue, want string) bool {
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

// writeJSONInput commits one complete command before writing it to the transport.
func writeJSONInput(ctx context.Context, w io.Writer, v any, owner *agent.Records) error {
	data, err := marshalInput(v)
	if err != nil {
		return err
	}
	return owner.WriteInput(ctx, w, data)
}

// readJSONRPCResponse waits for the expected request reply, servicing task-scoped
// MCP controls so an eager client cannot block the ACP handshake.
func readJSONRPCResponse(ctx context.Context, r *agent.RelayRecordReader, expectedID int64, handleMCP func(agent.MCPRequest) error) (*acp.JSONRPCMessage, error) {
	for {
		line, controls, err := r.ReadRecord()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("handshake: %w", ctx.Err())
			}
			return nil, fmt.Errorf("read response: %w", err)
		}
		for _, control := range controls {
			if exit, ok := control.Message.(*agent.ExitMessage); ok && exit.ExitCode != 0 {
				return nil, errors.New(exit.ExitError())
			}
			if request, ok := control.Message.(*agent.MCPRequestMessage); ok {
				if handleMCP == nil {
					continue
				}
				if err := handleMCP(agent.MCPRequest{ID: request.ID, Method: mcp.Method(request.Method), Name: request.Name, Arguments: request.Arguments}); err != nil {
					return nil, fmt.Errorf("respond task-scoped MCP: %w", err)
				}
			}
		}
		if len(line) == 0 {
			continue
		}
		var msg acp.JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		if msg.IsResponse() {
			var id int64
			if err := json.Unmarshal(msg.ID, &id); err != nil {
				return nil, fmt.Errorf("decode response ID: %w", err)
			}
			if id != expectedID {
				continue
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("handshake: %w", ctx.Err())
			}
			return &msg, nil
		}
		// Skip notifications during handshake.
		slog.DebugContext(ctx, "opencode handshake: skipping notification", "method", msg.Method)
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

// modelMetadata is the metadata "opencode models --verbose" prints beneath a
// model ID.
type modelMetadata struct {
	Variants map[string]json.RawMessage `json:"variants"`
	Limit    modelLimit                 `json:"limit"`
}

// modelLimit holds the token limits OpenCode publishes for a model.
type modelLimit struct {
	Context int `json:"context"`
}

// parseModels reads the model ID and pretty-printed metadata pairs
// emitted by "opencode models --verbose". OpenCode exposes model variants as
// its ACP effort options.
func parseModels(out []byte) ([]agent.Model, error) {
	lines := strings.Split(string(out), "\n")
	models := make([]agent.Model, 0)
	for line := 0; line < len(lines); line++ {
		model := strings.TrimSpace(lines[line])
		provider, id, ok := strings.Cut(model, "/")
		if !ok || provider == "" || id == "" {
			continue // E.g. the colored "Models cache refreshed" status line.
		}

		var data []byte
		var info modelMetadata
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
		models = append(models, agent.Model{ID: model, EffortOptions: efforts, ContextWindow: info.Limit.Context})
	}
	if len(models) == 0 {
		return nil, errors.New("no model metadata")
	}
	return normalizeModels(models), nil
}

// marshalInput encodes one complete native command without interpreting or delivering it.
func marshalInput(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// allocID allocates the next request in the synchronous startup exchange.
func (res *handshakeResult) allocID() int64 {
	res.nextID++
	return res.nextID
}
