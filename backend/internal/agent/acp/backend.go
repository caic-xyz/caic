// Shared ACP transport, directional replay, metadata, and task-scoped MCP.

// Package acp implements Agent Client Protocol over JSON-RPC 2.0 on stdin/stdout.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/maruel/genai/acp"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/gomode/mcp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// Backend owns shared ACP transport and parsing for a configured harness.
type Backend struct {
	agent.Base

	// Immutable
	Config *Config
}

// New creates an ACP backend with harness-owned protocol settings.
func New(h harness.Name, config *Config) *Backend {
	return &Backend{HarnessID: h, Config: config}
}

// SupportsImages implements agent.Backend.
func (b *Backend) SupportsImages() bool { return b.Config.Images }

// SupportsCompact implements agent.Backend.
func (b *Backend) SupportsCompact() bool { return b.Config.CompactPrompt != "" }

// RecordHandshake performs ACP setup using the recording's continuous decoder.
func (b *Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.ReadCloser, opts *agent.Options) (io.Reader, error) {
	_, continuation, err := b.handshake(ctx, stdin, bufio.NewReaderSize(stdout, 1<<16), stdout, opts)
	return continuation, err
}

// SetModelInventory implements agent.Backend, normalizing model names before
// storing them via the embedded Base (which owns the concurrency-safe storage).
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	b.Base.SetModelInventory(agent.ModelInventory{Models: NormalizeModels(inventory.Models)})
}

// Start launches the selected ACP process via the relay daemon in the given
// container. It performs the JSON-RPC handshake (initialize → session/new)
// before returning a Session.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (session *agent.Session, err error) {
	agentArgs := b.AgentArgs(agent.HarnessArgs{Model: opts.Model})
	relayArgs := []string{"--harness", string(b.Harness())}
	if opts.MCP != nil {
		relayArgs = append(relayArgs, "--caic-mcp")
	}
	rp, err := agent.PrepareRelay(ctx, opts, relayArgs, agentArgs)
	if err != nil {
		return nil, err
	}
	bootstrap := true
	defer func() {
		if bootstrap {
			closeErr := errors.Join(rp.Stdin.Close(), rp.Stdout.Close())
			_ = rp.Cmd.Process.Kill()
			err = errors.Join(err, closeErr, rp.Cmd.Wait())
			// Killing the SSH attachment leaves its daemon alive by design.
			// A failed handshake has no Session owner to stop that daemon.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			alive, statusErr := agent.IsRelayRunning(cleanup, opts.Target.SSHHost)
			err = errors.Join(err, statusErr)
			if alive {
				err = errors.Join(err, agent.StopRelay(cleanup, opts.Target))
			}
		}
	}()

	// Wrap stdout in a bufio.Reader so the handshake can read line-by-line
	// without losing buffered bytes for the session's readMessages goroutine.
	br := bufio.NewReaderSize(rp.Stdout, 1<<16)

	_, continuation, err := b.handshake(ctx, rp.Stdin, br, rp.Stdout, opts)
	if err != nil {
		return nil, fmt.Errorf("%s handshake: %w", b.Harness(), err)
	}

	log := opts.Logger.With("target", opts.Target.SSHHost)
	c := agent.NewConn(ctx, log, rp.Stdin, opts.Records, opts.MCP)
	if b.Config.Permissions != nil {
		c = &permissionConn{Conn: c, MessageInterceptor: b.Config.Permissions}
	}
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
func (b *Backend) AgentArgs(_ agent.HarnessArgs) []string {
	return b.Config.Args
}

// AttachRelay connects after the owning record processor has replayed history.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if b.Config.Permissions != nil {
		return agent.AttachRelaySession(ctx, opts, func(c agent.Conn) (agent.Conn, error) {
			return &permissionConn{Conn: c, MessageInterceptor: b.Config.Permissions}, nil
		})
	}
	return agent.AttachRelaySession(ctx, opts, nil)
}

// NewWire implements agent.Backend.
func (b *Backend) NewWire() agent.WireFormat {
	// Schema drift is checked offline by check-agent-logs.
	w := &wireFormat{config: b.Config}
	if b.Config.NewTools != nil {
		w.tools = b.Config.NewTools()
	}
	return w
}

// NewMetadataParser implements agent.Backend with an independent metadata projection.
func (b *Backend) NewMetadataParser() agent.MessageParser {
	return &metadataState{config: b.Config}
}

// HandleMessage delegates native controls to the configured permission policy.
func (b *Backend) HandleMessage(m agent.Message) (response []byte, handled bool, err error) {
	if b.Config.Permissions == nil {
		return nil, false, nil
	}
	return b.Config.Permissions.HandleMessage(m)
}

// handshake performs the ACP initialize → session/new sequence and returns
// the configuration needed to select requested settings.
func (b *Backend) handshake(ctx context.Context, stdin io.Writer, stdout *bufio.Reader, closer io.Closer, opts *agent.Options) (*handshakeResult, *bufio.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stopRead := agent.InterruptReadOnCancel(ctx, closer)
	defer stopRead()

	records := agent.NewRelayRecordReader(ctx, stdout, opts.Records)
	res := &handshakeResult{effortID: b.Config.EffortID}

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

	var handleAgent func(agent.Message) error
	if b.Config.Permissions != nil {
		handleAgent = func(m agent.Message) error {
			data, handled, err := b.Config.Permissions.HandleMessage(m)
			if err == nil && !handled {
				return errors.New("unhandled ACP startup request")
			}
			if err != nil {
				return err
			}
			return opts.Records.WriteInput(ctx, stdin, data)
		}
	}
	initResp, err := readJSONRPCResponse(ctx, records, initReq.ID, handleMCP, handleAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("read initialize response: %w", err)
	}

	// Extract capabilities and agent info.
	var initResult acp.InitializeResult
	if initResp.Result != nil {
		if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
			return nil, nil, fmt.Errorf("parse initialize result: %w", err)
		}
		metadata, err := sessionResultMetadata(acp.MethodInitialize, "", initResp.Result, b.Config.EffortID)
		if err != nil {
			return nil, nil, err
		}
		res.agentVersion = metadata.AgentVersion
	}

	// 2. Create or resume session. Use ACP's native MCP registration so user
	// configuration, including inline JSONC, stays untouched.
	servers := []acp.MCPServer{}
	if opts.MCP != nil {
		// Use an absolute MCP executable path. env keeps interpreter
		// lookup consistent with the relay's normal python3 launch.
		servers = append(servers, acp.MCPServer{
			Name:    "caic",
			Command: "/usr/bin/env",
			Args:    []string{"python3", agent.RelayScriptPath, "caic-mcp"},
			Env:     []acp.EnvVariable{{Name: "CAIC_RELAY_DIR", Value: agent.RelayDir}},
		})
	}
	var sessionReq acp.JSONRPCRequest
	if opts.ResumeSessionID != "" {
		// Resume requires persisted native state and the original workspace.
		params, err := marshalParams(acp.SessionLoadParams{SessionID: opts.ResumeSessionID, Cwd: opts.Dir, McpServers: servers})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session/load params: %w", err)
		}
		sessionReq = acp.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      res.allocID(),
			Method:  b.Config.ResumeMethod,
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
	resp, err := readJSONRPCResponse(ctx, records, sessionReq.ID, handleMCP, handleAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("read session response: %w", err)
	}

	// Extract session ID and models from result.
	var snResult acp.SessionNewResult
	if err := json.Unmarshal(resp.Result, &snResult); err != nil {
		return nil, nil, fmt.Errorf("parse session result: %w", err)
	}
	metadata, err := sessionResultMetadata(sessionReq.Method, opts.ResumeSessionID, resp.Result, b.Config.EffortID)
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
	if model != "" && b.Config.Models != nil {
		option := res.configOption(acp.ConfigOptionModel)
		var choices []acp.ConfigOptionValue
		if option != nil {
			choices = option.Options
		}
		model, err = b.Config.Models.Select(model, res.currentModel, choices)
		if err != nil {
			return nil, nil, err
		}
	}
	if model == "" {
		model = res.currentModel
	}
	if model != "" && model != res.currentModel {
		if err := res.setSessionConfigOption(ctx, stdin, records, acp.ConfigOptionModel, model, opts.Records, handleMCP, handleAgent); err != nil {
			return nil, nil, err
		}
	}
	if opts.Effort != "" {
		if err := res.setSessionConfigOption(ctx, stdin, records, b.Config.EffortID, opts.Effort, opts.Records, handleMCP, handleAgent); err != nil {
			return nil, nil, err
		}
	}

	return res, records.Reader(), nil
}

var (
	_ agent.Backend          = (*Backend)(nil)
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

	tools         ToolParser
	supportsImage bool // Set during handshake; read-only after.

	skillReadCalls map[string]struct{}
	nextID         int64
	textAccum      strings.Builder // Accumulated text from agent_message_chunk.
	thinkAccum     strings.Builder // Accumulated text from agent_thought_chunk.
	textOverflow   bool
	thinkOverflow  bool
}

// EncodePrompt encodes a session/prompt JSON-RPC request to begin a new turn.
func (w *wireFormat) EncodePrompt(p agent.Prompt) ([]byte, error) {
	w.mu.Lock()
	if w.session.SessionID == "" {
		w.mu.Unlock()
		return nil, errors.New("ACP: no session ID (handshake not completed)")
	}
	if len(p.Images) > 0 && (!w.config.Images || !w.supportsImage) {
		w.mu.Unlock()
		return nil, errors.New("ACP harness does not support image prompts")
	}
	id := w.allocIDLocked()
	sessionID := w.session.SessionID
	w.mu.Unlock()
	content := make([]acp.PromptContent, 0, 1+len(p.Images))
	content = append(content, acp.PromptContent{Type: acp.ContentText, Text: p.Text})
	for _, img := range p.Images {
		content = append(content, acp.PromptContent{
			Type:     acp.ContentImage,
			Data:     img.Data,
			MimeType: img.MediaType,
		})
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
	return MarshalInput(req)
}

// EncodeCompact sends the harness-configured native command through session/prompt.
func (w *wireFormat) EncodeCompact(instructions string) ([]byte, error) {
	if w.config.CompactPrompt == "" {
		return nil, errors.New("ACP harness has no verified compaction command")
	}
	if instructions != "" {
		return nil, agent.ErrCompactInstructionsUnsupported
	}
	return w.EncodePrompt(agent.Prompt{Text: w.config.CompactPrompt})
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
//   - native permission requests → preserves controls for the connection interceptor.
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
	if w.tools != nil {
		msgs, err = w.tools.Parse(toolCall, msgs)
		if err != nil {
			return nil, err
		}
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
	effortID      acp.ConfigOptionID
	sessionID     string
	nextID        int64
	currentModel  string // Model ID the session is using.
	currentEffort string // Reasoning effort the session is using.
	agentVersion  string // Agent version string from initialize.
	configOptions []acp.SessionConfigOption
}

func (res *handshakeResult) setConfigOptions(options []acp.SessionConfigOption) error {
	res.configOptions = options
	m := &agent.SessionMetadata{}
	for i := range options {
		option := &options[i]
		if err := applyConfigValue(m, option.ID, option.CurrentValue, res.effortID); err != nil {
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

func (res *handshakeResult) setSessionConfigOption(ctx context.Context, stdin io.Writer, records *agent.RelayRecordReader, id acp.ConfigOptionID, value string, owner *agent.Records, handleMCP func(agent.MCPRequest) error, handleAgent func(agent.Message) error) error {
	option := res.configOption(id)
	if option == nil {
		return fmt.Errorf("ACP does not expose %q for the selected model", id)
	}
	if option.Type != acp.ConfigOptionTypeSelect {
		return fmt.Errorf("ACP %q option has unsupported type %q", id, option.Type)
	}
	if id != acp.ConfigOptionModel && !hasConfigValue(option.Options, value) {
		return fmt.Errorf("ACP %q value %q is unavailable", id, value)
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
	resp, err := readJSONRPCResponse(ctx, records, requestID, handleMCP, handleAgent)
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

// allocID allocates the next request in the synchronous startup exchange.
func (res *handshakeResult) allocID() int64 {
	res.nextID++
	return res.nextID
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
	data, err := MarshalInput(v)
	if err != nil {
		return err
	}
	return owner.WriteInput(ctx, w, data)
}

// readJSONRPCResponse waits for the expected request reply, servicing task-scoped
// MCP controls so an eager client cannot block the ACP handshake.
func readJSONRPCResponse(ctx context.Context, r *agent.RelayRecordReader, expectedID int64, handleMCP func(agent.MCPRequest) error, handleAgent func(agent.Message) error) (*acp.JSONRPCMessage, error) {
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
		if msg.IsAgentRequest() && handleAgent != nil {
			if err := handleAgent(&agent.RawMessage{MessageType: string(msg.Method), Raw: line}); err != nil {
				return nil, fmt.Errorf("respond ACP startup request: %w", err)
			}
			continue
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
		slog.DebugContext(ctx, "ACP handshake: skipping notification", "method", msg.Method)
	}
}

// MarshalInput encodes one complete native command without interpreting or delivering it.
func MarshalInput(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
