// Package pi implements Pi RPC sessions with shared native metadata and token accounting.
//
// Pi uses a custom type-dispatched JSONL protocol over stdin/stdout (not
// JSON-RPC 2.0). There is no handshake — the subprocess is immediately ready
// to accept commands after launch.
//
// Per-session state is managed by piWireFormat around the stateless event
// decoders. The owning record processor keeps one configured piWireFormat through
// startup, attach replay, and live input/output.
//
// Extension UI auto-response is handled by piConn, which wraps the default
// Conn to intercept extension_ui_request messages during ReadMessages and
// write confirm/select responses back to Pi's stdin.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/tokens"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Backend implements agent.Backend for the Pi coding agent.
type Backend struct {
	agent.Base
}

// New creates a Pi backend.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID: harness.Pi,
		Images:    true,
		Compact:   true,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Pi, envVars))
	return b
}

// SetModelInventory implements agent.Backend, sorting models before storing
// them via the embedded Base (which owns the concurrency-safe storage).
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	byID := make(map[string]agent.Model, len(inventory.Models))
	for _, model := range inventory.Models {
		byID[model.ID] = model
	}
	ids := agent.SortModels(inventory.IDs())
	sorted := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		sorted = append(sorted, byID[id])
	}
	b.Base.SetModelInventory(agent.ModelInventory{Models: sorted})
}

// Start launches a Pi RPC process via the relay daemon. If Pi exits while
// starting, it updates Pi once and retries the launch.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	sess, err := b.start(ctx, opts)
	if err == nil {
		return sess, nil
	}
	if _, ok := errors.AsType[*piProcessExitError](err); !ok {
		return nil, err
	}

	opts.Logger.WarnContext(ctx, "pi: startup failed; updating and retrying", "err", err)
	if logErr := writeStartupLog(ctx, opts.Records, "Pi startup failed; running pi update --all ..."); logErr != nil {
		return nil, errors.Join(err, logErr)
	}
	out, updateErr := updatePi(ctx, opts.Target)
	if logErr := writeStartupLogOutput(ctx, opts.Records, "pi update", out); logErr != nil {
		return nil, errors.Join(err, updateErr, logErr)
	}
	if updateErr != nil {
		return nil, errors.Join(err, updateErr)
	}
	if logErr := writeStartupLog(ctx, opts.Records, "Pi update completed; retrying startup ..."); logErr != nil {
		return nil, errors.Join(err, logErr)
	}
	return b.start(ctx, opts)
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(a agent.HarnessArgs) []string {
	args := []string{"pi", "--mode", "rpc", "--approve"}
	if a.ResumeSessionID != "" {
		// Resume reads local JSONL history under ~/.pi/agent/sessions/ by default,
		// grouped by working directory. The ID alone cannot recover missing history.
		// md mounts ~/.pi into new containers. A custom session directory must
		// also be accessible; sessions created with --no-session cannot resume.
		// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/sessions.md
		args = append(args, "--session", a.ResumeSessionID)
	}
	return args
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, nil)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Schema drift is checked offline by check-agent-logs.
	return &piWireFormat{nativeSubagents: nativeSubagents{calls: make(map[string]agent.NativeSubagent)}}
}

// NewMetadataParser implements agent.Backend with an independent metadata projection.
func (*Backend) NewMetadataParser() agent.MessageParser {
	return &piMetadata{}
}

// FetchModelInventory implements agent.ModelFetcher.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModels(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return agent.ModelInventory{Models: models}, nil
}

// RecordHandshake configures Pi and validates its observed session using the
// same committed decoder that continues processing the running session.
func (*Backend) RecordHandshake(ctx context.Context, stdin io.Writer, stdout io.ReadCloser, opts *agent.Options) (io.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stopRead := agent.InterruptReadOnCancel(ctx, stdout)
	defer stopRead()
	records := agent.NewRelayRecordReader(ctx, stdout, opts.Records)
	if opts.Model != "" {
		if err := writeSetModel(ctx, stdin, opts.Model, opts.Records); err != nil {
			return nil, err
		}
		if _, err := waitForResponse(records, pi.CmdSetModel); err != nil {
			return nil, err
		}
	}
	if opts.Effort != "" {
		if err := writeSetThinking(ctx, stdin, opts.Effort, opts.Records); err != nil {
			return nil, err
		}
		if _, err := waitForResponse(records, pi.CmdSetThinking); err != nil {
			return nil, err
		}
	}
	if err := writeJSONLine(ctx, stdin, pi.GetStateCmd{Type: pi.CmdGetState}, opts.Records); err != nil {
		return nil, err
	}
	state, err := waitForResponse(records, pi.CmdGetState)
	if err != nil {
		if _, reported := errors.AsType[*piCommandError](err); reported && opts.ResumeSessionID == "" {
			if opts.Logger != nil {
				opts.Logger.WarnContext(ctx, "pi: get_state failed", "err", err)
			}
			return records.Reader(), nil
		}
		return nil, err
	}
	metadata, err := stateMetadata(state.Data)
	if err != nil {
		return nil, err
	}
	if opts.ResumeSessionID != "" && metadata.SessionID != opts.ResumeSessionID {
		return nil, fmt.Errorf("pi: resumed session %q does not match requested session %q", metadata.SessionID, opts.ResumeSessionID)
	}
	return records.Reader(), nil
}

func (b *Backend) start(ctx context.Context, opts *agent.Options) (session *agent.Session, err error) {
	args := b.AgentArgs(agent.HarnessArgs{Model: opts.Model, ResumeSessionID: opts.ResumeSessionID})
	relayArgs := []string{"--harness", "pi"}
	if opts.MCP != nil {
		relayArgs = append(relayArgs, "--caic-mcp")
		args = append(args, "--extension", agent.PiCaicMCPExtensionPath)
	}
	rp, err := agent.PrepareRelay(ctx, opts, relayArgs, args)
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

	continuation, err := b.RecordHandshake(ctx, rp.Stdin, rp.Stdout, opts)
	if err != nil {
		return nil, err
	}
	rp.Stdout = bufferedRelayOutput{Reader: continuation, Closer: rp.Stdout}

	bootstrap = false
	c := newPiConn(ctx, opts.Logger, rp.Stdin, opts)
	sess, err := agent.StartSession(ctx, rp, c, opts)
	if err != nil {
		return nil, err
	}

	return sess, nil
}

type bufferedRelayOutput struct {
	io.Reader
	io.Closer
}

// commandDiscriminator is a Pi command without parameters or a correlation ID.
type commandDiscriminator struct {
	Type pi.EventType `json:"type"`
}

var (
	_ agent.Backend      = (*Backend)(nil)
	_ agent.ModelFetcher = (*Backend)(nil)
)

// updatePi updates Pi and its extensions in the target container after a failed startup.
func updatePi(ctx context.Context, target runtime.ConnectionTarget) (string, error) {
	if target.SSHHost == "" {
		return "", errors.New("agent connection target missing SSH host")
	}
	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(updateCtx, "ssh", target.SSHHost, "pi", "update", "--all") //nolint:gosec // target is not user-controlled.
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err == nil {
		return output, nil
	}
	if output != "" {
		return output, fmt.Errorf("pi update --all: %w: %s", err, output)
	}
	return "", fmt.Errorf("pi update --all: %w", err)
}

// writeStartupLog writes a task log line for Pi startup recovery.
func writeStartupLog(ctx context.Context, records *agent.Records, line string) error {
	if err := records.AppendMessage(ctx, &agent.LogMessage{MessageType: "caic_log", Line: line}); err != nil {
		return fmt.Errorf("write Pi startup log: %w", err)
	}
	return nil
}

// writeStartupLogOutput writes each non-empty command-output line to the task log.
func writeStartupLogOutput(ctx context.Context, records *agent.Records, command, output string) error {
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := writeStartupLog(ctx, records, command+": "+line); err != nil {
			return err
		}
	}
	return nil
}

// piConn wraps a default Conn to intercept extension_ui_request messages
// during ReadMessages and auto-respond on stdin.
type piConn struct{ agent.Conn }

// newPiConn installs the provider interceptor around the shared response owner.
func newPiConn(ctx context.Context, logger *slog.Logger, stdin io.WriteCloser, opts *agent.Options) *piConn {
	return &piConn{Conn: agent.NewConn(ctx, logger, stdin, opts.Records, opts.MCP)}
}

// HandleMessage intercepts extension UI controls without writing stdin.
func (c *piConn) HandleMessage(m agent.Message) (response []byte, handled bool, err error) {
	raw, ok := m.(*agent.RawMessage)
	if !ok {
		return nil, false, nil
	}
	if strings.HasPrefix(raw.MessageType, "response:") {
		return nil, true, nil
	}
	if raw.MessageType == string(pi.EventExtensionUI) {
		data, err := extensionUIResponse(raw.Raw)
		return data, true, err
	}
	return nil, false, nil
}

// extensionUIResponse encodes an automatic extension UI reply. For confirm
// requests it sends confirmed=true. For select requests it picks the first
// option. For all others it sends an empty value.
func extensionUIResponse(raw []byte) ([]byte, error) {
	var req pi.ExtensionUIRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("unmarshal extension_ui_request: %w", err)
	}
	switch req.Method {
	case pi.UIMethodConfirm:
		resp := pi.ExtensionUIResponseConfirm{Type: pi.ExtensionUIResponseType, ID: req.ID, Confirmed: true}
		data, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	case pi.UIMethodSelect:
		val := ""
		if len(req.Options) > 0 {
			val = req.Options[0]
		}
		resp := pi.ExtensionUIResponseValue{Type: pi.ExtensionUIResponseType, ID: req.ID, Value: val}
		data, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	case pi.UIMethodNotify, pi.UIMethodSetStatus, pi.UIMethodSetWidget, pi.UIMethodSetTitle:
		// Fire-and-forget; no response needed.
		return nil, nil
	default:
		// input, editor, or unknown: send empty value.
		resp := pi.ExtensionUIResponseValue{Type: pi.ExtensionUIResponseType, ID: req.ID, Value: ""}
		data, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	}
}

// maxTrackedToolOutputs bounds per-tool incremental output state while parsing
// direct task history or a long-running Pi session.
const maxTrackedToolOutputs = 1024

// piWireFormat implements agent.WireFormat and agent.CompactCommand for Pi's
// type-dispatched JSONL protocol. It holds per-session state: a start time for
// duration tracking and a turn counter incremented by handleTurnEnd.
type piWireFormat struct {
	piMetadata

	nativeSubagents nativeSubagents

	// Per-tool accumulated output length for computing incremental deltas.
	// Pi's tool_execution_update events carry the full accumulated output;
	// we track the previous length to emit only the new portion.
	toolOutputLen map[string]int
}

// EncodePrompt encodes a complete Pi prompt without changing decoder state.
func (*piWireFormat) EncodePrompt(p agent.Prompt) ([]byte, error) {
	cmd := pi.PromptCmd{
		Type:              pi.CmdPrompt,
		Message:           p.Text,
		StreamingBehavior: pi.StreamSteer, // ignored by pi when idle; queues as steer when streaming
	}
	for _, img := range p.Images {
		cmd.Images = append(cmd.Images, pi.ImageContent{
			Type:     pi.ContentImage,
			Data:     img.Data,
			MimeType: img.MediaType,
		})
	}
	return marshalInput(cmd)
}

// EncodeCompact implements agent.CompactCommand.
func (*piWireFormat) EncodeCompact(instructions string) ([]byte, error) {
	cmd := pi.CompactCmd{
		Type:               pi.CmdCompact,
		CustomInstructions: instructions,
	}
	return marshalInput(cmd)
}

// ParseOutput adapts native Pi records and marks unique token contributions.
// turn_end and compaction summaries are additive; a result repeats the last
// call when those reports already account for it. This state is reconstructed
// from native records during replay, including result-only histories.
func (w *piWireFormat) ParseOutput(line []byte) ([]agent.Message, error) {
	typ, err := decodeEventType(line)
	if err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}
	// Each agent_start begins a low-level run with its own agent_end messages.
	// https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/agent/src/agent-loop.ts
	if typ == pi.EventAgentStart {
		w.usageSinceResult = false
	}
	msgs, err := w.parseNativeMessage(typ, line)
	if err != nil {
		return nil, err
	}
	w.applyMetadata(msgs)
	w.markAccounting(typ, msgs)
	return msgs, nil
}

// ParseInput applies prompt lifecycle changes only after the native input commits.
func (w *piWireFormat) ParseInput(line []byte) ([]agent.Message, error) {
	typ, err := decodeEventType(line)
	if err != nil {
		return nil, err
	}
	switch typ {
	case pi.CmdPrompt:
		// Persisted stdin prompt command; convert to UserInputMessage.
		var cmd pi.PromptCmd
		if err := json.Unmarshal(line, &cmd); err != nil {
			return nil, fmt.Errorf("unmarshal prompt cmd: %w", err)
		}
		w.beginPrompt()
		w.toolOutputLen = nil
		ui := &agent.UserInputMessage{Text: cmd.Message}
		for _, img := range cmd.Images {
			ui.Images = append(ui.Images, v3.ImageData{
				MediaType: img.MimeType,
				Data:      img.Data,
			})
		}
		return []agent.Message{ui}, nil

	case pi.CmdCompact:
		// Stdin compact command logged by relay; skip during replay.
		return nil, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(typ), Raw: append([]byte(nil), line...)}}, nil
	}
}

func (w *piWireFormat) parseNativeMessage(typ pi.EventType, line []byte) ([]agent.Message, error) {
	if typ == pi.EventResponse {
		resp, err := decodeResponse(line)
		if err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		msgs, err := w.responseMessages(&resp)
		if err != nil || len(msgs) > 0 {
			return msgs, err
		}
		return []agent.Message{&agent.RawMessage{MessageType: "response:" + string(resp.Command), Raw: append([]byte(nil), line...)}}, nil
	}

	// Intercept agent_end for final usage.
	if typ == pi.EventAgentEnd {
		return w.handleAgentEnd(line)
	}

	// Intercept turn_end for per-turn usage.
	if typ == pi.EventTurnEnd {
		return w.handleTurnEnd(line)
	}

	// Intercept message_start to report the model on the first turn.
	if typ == pi.EventMessageStart {
		return w.handleMessageStart(line)
	}

	// Intercept message_end for consolidated assistant content or errors.
	if typ == pi.EventMessageEnd {
		return w.handleMessageEnd(line)
	}

	if typ == pi.EventMessageUpdate {
		ev, err := decodeMessageUpdateEvent(line)
		if err != nil {
			return nil, fmt.Errorf("unmarshal message_update: %w", err)
		}
		switch ev.AssistantMessageEvent.Type {
		case pi.DeltaDone:
			return w.handleDone(&ev)
		case pi.DeltaError:
			return w.handleError(&ev)
		default:
			return messagesFromAssistantMessageEvent(&ev.AssistantMessageEvent, line)
		}
	}

	msgs, record, err := parseMessageTyped(typ, line)
	if err != nil {
		return nil, err
	}

	native, err := w.nativeSubagents.parse(record)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, native...)

	// For tool output deltas, compute incremental deltas since Pi's
	// tool_execution_update events carry the full accumulated output.
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.ToolOutputDeltaMessage:
			w.mu.Lock()
			if w.toolOutputLen == nil {
				w.toolOutputLen = make(map[string]int)
			}
			prev, found := w.toolOutputLen[m.ToolUseID]
			if !found && len(w.toolOutputLen) == maxTrackedToolOutputs {
				// An interrupted stream can leave arbitrary tool IDs unfinished.
				// Drop old offsets rather than retaining unbounded parser state.
				clear(w.toolOutputLen)
				prev = 0
			}
			if prev < len(m.Delta) {
				m.Delta = m.Delta[prev:]
				w.toolOutputLen[m.ToolUseID] = prev + len(m.Delta)
			} else {
				m.Delta = ""
			}
			w.mu.Unlock()
		case *agent.ToolResultMessage:
			w.mu.Lock()
			delete(w.toolOutputLen, m.ToolUseID)
			w.mu.Unlock()
		}
	}
	// Filter out empty messages after incremental delta computation.
	n := 0
	for _, msg := range msgs {
		if tod, ok := msg.(*agent.ToolOutputDeltaMessage); ok && tod.Delta == "" {
			continue
		}
		msgs[n] = msg
		n++
	}
	return msgs[:n], nil
}

// handleDone converts a done delta into a ResultMessage. Pi currently does not
// emit done deltas, so this path is not exercised in normal operation, but is
// kept for protocol evolution.
func (w *piWireFormat) handleDone(ev *pi.MessageUpdateEvent) ([]agent.Message, error) {
	rm := &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "result",
	}
	if ev.AssistantMessageEvent.Reason == pi.StopReasonError {
		rm.IsError = true
	}
	return []agent.Message{rm}, nil
}

// handleMessageEnd handles a message_end event, emitting the consolidated
// assistant content. That lets replay collapse prior streaming deltas instead
// of sending every token fragment back to clients.
func (w *piWireFormat) handleMessageEnd(line []byte) ([]agent.Message, error) {
	var ev pi.MessageEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal message_end: %w", err)
	}
	if ev.Message.StopReason == pi.StopReasonError {
		return w.errorResultMessages(ev.Message.ErrorMessage), nil
	}
	return messagesFromAgentMessage(&ev.Message), nil
}

func messagesFromAgentMessage(msg *pi.AgentMessage) []agent.Message {
	if msg == nil || msg.Role != pi.RoleAssistant {
		return nil
	}
	out := make([]agent.Message, 0, len(msg.Content))
	for i := range msg.Content {
		block := &msg.Content[i]
		switch block.Type {
		case pi.ContentText:
			if block.Text != "" {
				out = append(out, &agent.TextMessage{Text: block.Text})
			}
		case pi.ContentThinking:
			if block.Thinking != "" {
				out = append(out, &agent.ThinkingMessage{Text: block.Thinking})
			}
		case pi.ContentToolCall, pi.ContentImage:
			// Tool calls and images are emitted by their dedicated event paths.
		}
	}
	return out
}

// resolvedModel returns the provider-served model when available, otherwise
// the configured model. Pi reports responseModel when a router resolves an
// alias to a concrete model.
func resolvedModel(provider, responseModel, model string) string {
	if responseModel != "" {
		model = responseModel
	}
	if provider == "" || model == "" {
		return model
	}
	return provider + "/" + model
}

// handleError converts an error delta into a ResultMessage.
func (w *piWireFormat) handleError(ev *pi.MessageUpdateEvent) ([]agent.Message, error) {
	result := ""
	if ev.AssistantMessageEvent.Error != nil && ev.AssistantMessageEvent.Error.ErrorMessage != "" {
		result = ev.AssistantMessageEvent.Error.ErrorMessage
	}
	return w.errorResultMessages(result), nil
}

// quotaErrorMarkers are lowercase substrings that identify a provider quota or
// rate-limit exhaustion in a Pi error message. Pi bubbles up provider-specific
// wording (e.g. Codex's "The usage limit has been reached"), so match on the
// common phrasings across providers rather than a single exact string.
var quotaErrorMarkers = []string{
	"usage limit",
	"rate limit",
	"quota",
	"too many requests",
	"resource exhausted",
	"resource_exhausted",
	"429",
}

// isQuotaError reports whether errMsg names a provider quota or rate limit.
func isQuotaError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	for _, marker := range quotaErrorMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func toAgentUsage(u *pi.MessageUsage) agent.Usage {
	usage := agent.Usage{
		InputTokens:              int(u.Input),
		OutputTokens:             int(u.Output),
		CacheReadInputTokens:     int(u.CacheRead),
		CacheCreationInputTokens: max(0, int(u.CacheWrite)),
		ReasoningOutputTokens:    int(u.Reasoning),
	}
	if u.CacheWrite1h > 0 {
		usage.CacheWrites = tokens.NewCacheWrites(int64(usage.CacheCreationInputTokens), 5*time.Minute, tokens.CacheWrite{TTL: time.Hour, Tokens: u.CacheWrite1h})
	}

	// Pi defines cacheWrite1h as the one-hour subset of cacheWrite and only
	// Anthropic reports it. A positive remainder is therefore a mixed response
	// whose first expiry is five minutes. Other providers do not report an
	// applied duration, so leave their TTL unknown:
	// https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/types.ts
	// https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/api/anthropic-messages.ts
	switch {
	case u.CacheWrite1h > 0 && u.CacheWrite1h == u.CacheWrite:
		usage.CacheTTLSeconds = 3600
	case u.CacheWrite1h > 0 && u.CacheWrite1h < u.CacheWrite:
		usage.CacheTTLSeconds = 300
	}
	return usage
}

type agentEndEnvelope struct {
	WillRetry bool `json:"willRetry"`
}

type piProcessExitError struct {
	exit agent.ExitMessage
}

func (e *piProcessExitError) Error() string {
	return fmt.Sprintf("agent subprocess exited with code %d: %s", e.exit.ExitCode, e.exit.Error)
}

// piCommandError is a valid native command rejection, distinct from corrupt
// evidence, owner failure, or a transport ending before the reply.
type piCommandError struct {
	command pi.EventType
	detail  string
}

func (e *piCommandError) Error() string { return fmt.Sprintf("pi %s: %s", e.command, e.detail) }

// waitForResponse consumes physical records until it receives the requested
// native Pi response. Relay controls bypass the native response parser.
func waitForResponse(r *agent.RelayRecordReader, cmd pi.EventType) (pi.Response, error) {
	for {
		line, controls, err := r.ReadRecord()
		if err != nil {
			return pi.Response{}, fmt.Errorf("read response for %s: %w", cmd, err)
		}
		for _, control := range controls {
			if exit, ok := control.Message.(*agent.ExitMessage); ok && exit.ExitCode != 0 {
				return pi.Response{}, &piProcessExitError{exit: *exit}
			}
		}
		if len(line) == 0 {
			continue
		}
		var resp pi.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return pi.Response{}, fmt.Errorf("decode response for %s: %w", cmd, err)
		}
		if resp.Command != cmd {
			continue
		}
		if !resp.Success {
			return pi.Response{}, &piCommandError{command: cmd, detail: resp.Error}
		}
		return resp, nil
	}
}

// writeSetModel sends a set_model command to Pi. The model string is split on
// "/" into provider + modelId (e.g. "cerebras/gpt-oss-120b").
func writeSetModel(ctx context.Context, w io.Writer, model string, records *agent.Records) error {
	provider, modelID := "", model
	if p, m, ok := strings.Cut(model, "/"); ok {
		provider, modelID = p, m
	}
	cmd := pi.SetModelCmd{
		Type:     pi.CmdSetModel,
		Provider: provider,
		ModelID:  modelID,
	}
	return writeJSONLine(ctx, w, cmd, records)
}

// writeSetThinking sends a set_thinking_level command to Pi.
func writeSetThinking(ctx context.Context, w io.Writer, level string, records *agent.Records) error {
	cmd := pi.SetThinkingCmd{
		Type:  pi.CmdSetThinking,
		Level: pi.ThinkingLevel(level),
	}
	return writeJSONLine(ctx, w, cmd, records)
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
	args = append(args, "pi", "--mode", "rpc", "--approve", "--no-session")
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // target is not user-controlled
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Send get_available_models.
	data, err := json.Marshal(commandDiscriminator{Type: pi.CmdGetModels})
	if err != nil {
		return nil, err
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write get_available_models: %w", err)
	}

	// Read until we get the response.
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read get_available_models response: %w", err)
		}
		var probe pi.LineProbe
		if json.Unmarshal(line, &probe) != nil || probe.Type != pi.EventResponse {
			continue
		}
		var resp pi.Response
		if json.Unmarshal(line, &resp) != nil || resp.Command != pi.CmdGetModels {
			continue
		}
		if !resp.Success {
			return nil, fmt.Errorf("get_available_models: %s", resp.Error)
		}
		var payload struct {
			Models []pi.Model `json:"models"`
		}
		if err := json.Unmarshal(resp.Data, &payload); err != nil {
			return nil, fmt.Errorf("parse models: %w", err)
		}
		return modelsForPiModels(payload.Models), nil
	}
}

func modelsForPiModels(models []pi.Model) []agent.Model {
	efforts := make(map[string][]string, len(models))
	windows := make(map[string]int, len(models))
	ids := make([]string, 0, len(models))
	for i := range models {
		id := models[i].GetID()
		if id == "" {
			continue
		}
		ids = append(ids, id)
		efforts[id] = effortOptions(&models[i])
		if cw := models[i].ContextWindow; cw > 0 {
			windows[id] = int(cw)
		}
	}

	ids = agent.SortModels(ids)
	result := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		result = append(result, agent.Model{ID: id, EffortOptions: efforts[id], ContextWindow: windows[id]})
	}
	return result
}

func effortOptions(model *pi.Model) []string {
	if !model.Reasoning {
		return []string{string(pi.ThinkingOff)}
	}

	levels := [...]pi.ThinkingLevel{
		pi.ThinkingOff,
		pi.ThinkingMinimal,
		pi.ThinkingLow,
		pi.ThinkingMedium,
		pi.ThinkingHigh,
		pi.ThinkingXHigh,
		pi.ThinkingMax,
	}
	efforts := make([]string, 0, len(levels))
	for _, level := range levels {
		mapped, defined := model.ThinkingLevelMap[level]
		if defined && mapped == "" {
			// genai decodes Pi's JSON null (explicitly unsupported) as empty.
			continue
		}
		if (level == pi.ThinkingXHigh || level == pi.ThinkingMax) && !defined {
			continue
		}
		efforts = append(efforts, string(level))
	}
	return efforts
}

// writeJSONLine commits one complete command before writing it to Pi.
func writeJSONLine(ctx context.Context, w io.Writer, v any, records *agent.Records) error {
	data, err := marshalInput(v)
	if err != nil {
		return err
	}
	return records.WriteInput(ctx, w, data)
}

// marshalInput encodes a complete command without advancing session state.
func marshalInput(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
