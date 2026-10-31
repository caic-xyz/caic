// Package pi implements Pi RPC sessions with shared native metadata and token accounting.
//
// Pi uses a custom type-dispatched JSONL protocol over stdin/stdout (not
// JSON-RPC 2.0). There is no handshake — the subprocess is immediately ready
// to accept commands after launch.
//
// Per-session state is managed by piWireFormat around the stateless event
// decoders. A fresh piWireFormat is created for every Start,
// AttachRelay, and NewWire call so that counters reset between sessions and
// replays.
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
	"sync"
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
	if logErr := writeStartupLog(opts.Log, "Pi startup failed; running pi update --all ..."); logErr != nil {
		return nil, errors.Join(err, logErr)
	}
	out, updateErr := updatePi(ctx, opts.Target)
	if logErr := writeStartupLogOutput(opts.Log, "pi update", out); logErr != nil {
		return nil, errors.Join(err, updateErr, logErr)
	}
	if updateErr != nil {
		return nil, errors.Join(err, updateErr)
	}
	if logErr := writeStartupLog(opts.Log, "Pi update completed; retrying startup ..."); logErr != nil {
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
	wire := &piWireFormat{nativeSubagents: nativeSubagents{calls: make(map[string]agent.NativeSubagent)}}
	return agent.AttachRelaySession(ctx, opts, wire, nil)
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

// WritePrePrompt implements agent.PrePromptWriter. It sends a set_model command
// when model is non-empty.
func (*Backend) WritePrePrompt(w io.Writer, model string, log agent.LogSink) error {
	if model != "" {
		return writeSetModel(w, model, log)
	}
	return nil
}

// FetchModelInventory implements agent.ModelFetcher.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModels(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return agent.ModelInventory{Models: models}, nil
}

func (b *Backend) start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	wire := &piWireFormat{nativeSubagents: nativeSubagents{calls: make(map[string]agent.NativeSubagent)}}

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

	records := agent.NewRelayRecordReader(rp.Stdout, opts.Log)

	// Pre-prompt commands: set_model and set_thinking_level. We must wait
	// for each response before sending the next.
	if opts.Model != "" {
		if err := writeSetModel(rp.Stdin, opts.Model, opts.Log); err != nil {
			return nil, fmt.Errorf("pi: write set_model: %w", err)
		}
		resp, err := waitForResponse(records, pi.CmdSetModel)
		if err != nil {
			err = reapPiProcessExit(rp, err)
			return nil, fmt.Errorf("pi: set_model %s: %w", opts.Model, err)
		}
		metadata, err := wire.responseMessages(&resp)
		if err != nil {
			return nil, fmt.Errorf("pi: set_model metadata: %w", err)
		}
		wire.applyMetadata(metadata)
		for _, msg := range metadata {
			switch m := msg.(type) {
			case *agent.SessionMetadataMessage:
				opts.MsgCh <- agent.TimedMessage{Message: m}
			case *agent.ModelInfoMessage:
				if err := opts.Log.AppendMessage(m); err != nil {
					return nil, fmt.Errorf("write caic_model_info: %w", err)
				}
			}
		}
	}
	if opts.Effort != "" {
		if err := writeSetThinking(rp.Stdin, opts.Effort, opts.Log); err != nil {
			return nil, fmt.Errorf("pi: write set_thinking_level: %w", err)
		}
		if _, err := waitForResponse(records, pi.CmdSetThinking); err != nil {
			err = reapPiProcessExit(rp, err)
			return nil, fmt.Errorf("pi: set_thinking_level %s: %w", opts.Effort, err)
		}
	}

	var state []agent.Message
	var metadata agent.SessionMetadata
	if err := writeGetState(rp.Stdin, opts.Log); err != nil {
		opts.Logger.WarnContext(ctx, "pi: write get_state failed", "err", err)
	} else {
		stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := waitForResponseContext(stateCtx, records, pi.CmdGetState, func() {
			_ = rp.Cmd.Process.Kill()
		})
		stateCancel()
		if err != nil {
			if errors.Is(err, errResponseTimeout) {
				_ = rp.Cmd.Wait()
				return nil, fmt.Errorf("pi: get_state: %w", err)
			}
			if _, ok := errors.AsType[*piProcessExitError](err); ok {
				return nil, fmt.Errorf("pi: get_state: %w", reapPiProcessExit(rp, err))
			}
			opts.Logger.WarnContext(ctx, "pi: get_state failed", "err", err)
		} else {
			state, err = wire.responseMessages(&resp)
			if err != nil {
				opts.Logger.WarnContext(ctx, "pi: parse get_state failed", "err", err)
			} else {
				wire.applyMetadata(state)
				for _, msg := range state {
					metadata.Apply(msg)
				}
			}
		}
	}
	if opts.ResumeSessionID != "" && metadata.SessionID != opts.ResumeSessionID {
		_ = rp.Cmd.Process.Kill()
		_ = rp.Cmd.Wait()
		return nil, fmt.Errorf("pi: resumed session %q does not match requested session %q", metadata.SessionID, opts.ResumeSessionID)
	}
	rp.Stdout = bufferedRelayOutput{Reader: records.Reader(), Closer: rp.Stdout}
	for _, msg := range state {
		opts.MsgCh <- agent.TimedMessage{Message: msg}
	}
	if metadata != (agent.SessionMetadata{}) {
		initMsg := &agent.InitMessage{SessionID: metadata.SessionID, ReportedModel: metadata.ReportedModel, ReportedEffort: metadata.ReportedEffort}
		if err := agent.WriteMetaSession(opts.Log, initMsg); err != nil {
			_ = rp.Cmd.Process.Kill()
			_ = rp.Cmd.Wait()
			return nil, fmt.Errorf("write session metadata: %w", err)
		}
	}

	c := newPiConn(ctx, opts.Logger, rp.Stdin, opts.Log, wire, opts)
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
func writeStartupLog(log agent.LogSink, line string) error {
	if err := log.AppendMessage(&agent.LogMessage{MessageType: "caic_log", Line: line}); err != nil {
		return fmt.Errorf("write Pi startup log: %w", err)
	}
	return nil
}

// writeStartupLogOutput writes each non-empty command-output line to the task log.
func writeStartupLogOutput(log agent.LogSink, command, output string) error {
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := writeStartupLog(log, command+": "+line); err != nil {
			return err
		}
	}
	return nil
}

// piConn wraps a default Conn to intercept extension_ui_request messages
// during ReadMessages and auto-respond on stdin.
type piConn struct{ agent.Conn }

// newPiConn installs the provider interceptor around the shared response owner.
func newPiConn(ctx context.Context, logger *slog.Logger, stdin io.WriteCloser, log agent.LogSink, wire *piWireFormat, opts *agent.Options) *piConn {
	return &piConn{Conn: agent.NewMCPConn(ctx, logger, stdin, log, wire, opts.MCP)}
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

// WritePrompt sends a prompt command to Pi's stdin and records the start time
// for duration tracking.
func (w *piWireFormat) WritePrompt(wr io.Writer, p agent.Prompt, log agent.LogSink) error {
	w.mu.Lock()
	w.startTime = time.Now()
	w.numTurns = 0
	w.toolOutputLen = nil
	w.mu.Unlock()

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
	return writeJSONLine(wr, cmd, log)
}

// WriteCompact implements agent.CompactCommand.
func (w *piWireFormat) WriteCompact(wr io.Writer, instructions string, log agent.LogSink) error {
	cmd := pi.CompactCmd{
		Type:               pi.CmdCompact,
		CustomInstructions: instructions,
	}
	return writeJSONLine(wr, cmd, log)
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

// ParseInput decodes recorded Pi commands without changing output lifecycle or usage.
func (*piWireFormat) ParseInput(line []byte) ([]agent.Message, error) {
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

var errResponseTimeout = errors.New("pi response wait timed out")

type piProcessExitError struct {
	exit agent.ExitMessage
}

func (e *piProcessExitError) Error() string {
	return fmt.Sprintf("agent subprocess exited with code %d: %s", e.exit.ExitCode, e.exit.Error)
}

func reapPiProcessExit(rp *agent.RelayProcess, err error) error {
	if _, ok := errors.AsType[*piProcessExitError](err); !ok {
		return err
	}
	// The relay's attach process keeps reading stdin after its daemon reports
	// a failed agent startup. Close the pipe before Wait so it can exit instead
	// of leaving task startup stuck in StateStarting.
	if closeErr := rp.Stdin.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close failed Pi startup stdin: %w", closeErr))
	}
	if waitErr := rp.Cmd.Wait(); waitErr != nil {
		return errors.Join(err, fmt.Errorf("wait for failed Pi startup: %w", waitErr))
	}
	return err
}

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
			return pi.Response{}, fmt.Errorf("pi %s: %s", cmd, resp.Error)
		}
		return resp, nil
	}
}

func waitForResponseContext(ctx context.Context, r *agent.RelayRecordReader, cmd pi.EventType, onTimeout func()) (pi.Response, error) {
	type result struct {
		resp pi.Response
		err  error
	}
	ch := make(chan result, 1)
	var reader sync.WaitGroup
	reader.Go(func() {
		resp, err := waitForResponse(r, cmd)
		ch <- result{resp: resp, err: err}
	})
	defer reader.Wait()
	select {
	case res := <-ch:
		return res.resp, res.err
	case <-ctx.Done():
		if onTimeout != nil {
			onTimeout()
		}
		res := <-ch
		return pi.Response{}, errors.Join(fmt.Errorf("%w for %s: %w", errResponseTimeout, cmd, ctx.Err()), res.err)
	}
}

// writeSetModel sends a set_model command to Pi. The model string is split on
// "/" into provider + modelId (e.g. "cerebras/gpt-oss-120b").
func writeSetModel(w io.Writer, model string, log agent.LogSink) error {
	provider, modelID := "", model
	if p, m, ok := strings.Cut(model, "/"); ok {
		provider, modelID = p, m
	}
	cmd := pi.SetModelCmd{
		Type:     pi.CmdSetModel,
		Provider: provider,
		ModelID:  modelID,
	}
	return writeJSONLine(w, cmd, log)
}

// writeSetThinking sends a set_thinking_level command to Pi.
func writeSetThinking(w io.Writer, level string, log agent.LogSink) error {
	cmd := pi.SetThinkingCmd{
		Type:  pi.CmdSetThinking,
		Level: pi.ThinkingLevel(level),
	}
	return writeJSONLine(w, cmd, log)
}

func writeGetState(w io.Writer, log agent.LogSink) error {
	return writeJSONLine(w, pi.GetStateCmd{Type: pi.CmdGetState}, log)
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

// writeJSONLine marshals v as JSON, writes it followed by LF, and logs it.
func writeJSONLine(w io.Writer, v any, log agent.LogSink) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := agent.AppendInputNativeRecord(log, data); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}
