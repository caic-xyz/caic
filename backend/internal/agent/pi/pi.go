// Package pi implements agent.Backend for Pi coding agent CLI in RPC mode.
//
// Pi uses a custom type-dispatched JSONL protocol over stdin/stdout (not
// JSON-RPC 2.0). There is no handshake — the subprocess is immediately ready
// to accept commands after launch.
//
// Per-session state is managed by piWireFormat, which wraps the stateless
// parseMessage function. A fresh piWireFormat is created for every Start,
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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Backend implements agent.Backend for the Pi coding agent.
type Backend struct {
	agent.Base

	mu        sync.Mutex
	inventory agent.ModelInventory
}

var (
	_ agent.Backend      = (*Backend)(nil)
	_ agent.ModelFetcher = (*Backend)(nil)
)

// New creates a Pi backend.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{}
	b.Base = agent.Base{
		HarnessID:     harness.Pi,
		Images:        true,
		Compact:       true,
		ContextWindow: 200_000,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Pi, envVars))
	return b
}

// ModelInventory implements agent.Backend.
func (b *Backend) ModelInventory() agent.ModelInventory {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inventory
}

// SetModelInventory implements agent.Backend.
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inventory = agent.ModelInventory{Models: sorted}
}

// Start launches a Pi RPC process via the relay daemon. If Pi exits while
// starting, it updates Pi once and retries the launch.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	sess, err := b.start(ctx, opts)
	if err == nil {
		return sess, nil
	}
	if _, ok := errors.AsType[*piProcessExitError](err); !ok {
		return nil, err
	}

	slog.WarnContext(ctx, "pi: startup failed; updating and retrying", "err", err)
	if logErr := writeStartupLog(opts.LogW, "Pi startup failed; running pi update --all ..."); logErr != nil {
		return nil, errors.Join(err, logErr)
	}
	out, updateErr := updatePi(ctx, opts.Target)
	if logErr := writeStartupLogOutput(opts.LogW, "pi update", out); logErr != nil {
		return nil, errors.Join(err, updateErr, logErr)
	}
	if updateErr != nil {
		return nil, errors.Join(err, updateErr)
	}
	if logErr := writeStartupLog(opts.LogW, "Pi update completed; retrying startup ..."); logErr != nil {
		return nil, errors.Join(err, logErr)
	}
	return b.start(ctx, opts)
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(_ agent.HarnessArgs) []string {
	return []string{"pi", "--mode", "rpc", "--approve"}
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	wire := &piWireFormat{fw: &jsonutil.FieldWarner{}}
	return agent.AttachRelaySession(ctx, opts, wire, nil)
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	// Log replay can parse large histories; skip development-only unknown-field scans.
	return &piWireFormat{}
}

// WritePrePrompt implements agent.PrePromptWriter. It sends a set_model command
// when model is non-empty.
func (*Backend) WritePrePrompt(w io.Writer, model string, logW io.Writer) error {
	if model != "" {
		return writeSetModel(w, model, logW)
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
	wire := &piWireFormat{fw: &jsonutil.FieldWarner{}}

	rp, err := agent.PrepareRelay(ctx, opts, b.AgentArgs(agent.HarnessArgs{Model: opts.Model}))
	if err != nil {
		return nil, err
	}

	// Pre-prompt commands: set_model and set_thinking_level. We must wait
	// for each response before sending the next.
	var br *bufio.Reader
	if opts.Model != "" {
		if err := writeSetModel(rp.Stdin, opts.Model, opts.LogW); err != nil {
			return nil, fmt.Errorf("pi: write set_model: %w", err)
		}
		br = bufio.NewReaderSize(rp.Stdout, 1<<20)
		resp, err := waitForResponse(br, pi.CmdSetModel, opts.LogW)
		if err != nil {
			err = reapPiProcessExit(rp, err)
			return nil, fmt.Errorf("pi: set_model %s: %w", opts.Model, err)
		}
		if cw := parseModelContextWindow(&resp); cw > 0 {
			wire.modelCtxWindow = cw
			// Persist to relay output so replay/adoption restores the
			// context window (otherwise it falls back to the harness
			// hardcoded default of 200k).
			info, err := json.Marshal(caicModelInfo{
				Type:          "caic_model_info",
				ContextWindow: cw,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal caic_model_info: %w", err)
			}
			if _, err := opts.LogW.Write(append(info, '\n')); err != nil {
				return nil, fmt.Errorf("write caic_model_info: %w", err)
			}
		}
		rp.Stdout = br // hand the buffered reader to the next command
	}
	if opts.Effort != "" {
		if err := writeSetThinking(rp.Stdin, opts.Effort, opts.LogW); err != nil {
			return nil, fmt.Errorf("pi: write set_thinking_level: %w", err)
		}
		if br == nil {
			br = bufio.NewReaderSize(rp.Stdout, 1<<20)
		}
		if _, err := waitForResponse(br, pi.CmdSetThinking, opts.LogW); err != nil {
			err = reapPiProcessExit(rp, err)
			return nil, fmt.Errorf("pi: set_thinking_level %s: %w", opts.Effort, err)
		}
		rp.Stdout = br
	}

	sessionID := ""
	if err := writeGetState(rp.Stdin, opts.LogW); err != nil {
		slog.WarnContext(ctx, "pi: write get_state failed", "err", err)
	} else {
		if br == nil {
			br = bufio.NewReaderSize(rp.Stdout, 1<<20)
		}
		stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := waitForResponseContext(stateCtx, br, pi.CmdGetState, opts.LogW, func() {
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
			slog.WarnContext(ctx, "pi: get_state failed", "err", err)
		} else {
			var state pi.StateData
			if err := json.Unmarshal(resp.Data, &state); err != nil {
				slog.WarnContext(ctx, "pi: parse get_state failed", "err", err)
			} else {
				sessionID = state.SessionID
			}
		}
		rp.Stdout = br
	}
	wire.sessionID = sessionID

	if sessionID != "" {
		opts.MsgCh <- &agent.MetaSessionMessage{MessageType: "caic_session", SessionID: sessionID}
		if err := agent.WriteMetaSession(opts.LogW, &agent.InitMessage{SessionID: sessionID}); err != nil {
			_ = rp.Cmd.Process.Kill()
			_ = rp.Cmd.Wait()
			return nil, fmt.Errorf("write session metadata: %w", err)
		}
	}

	c := newPiConn(rp.Stdin, opts.LogW, wire)
	sess, err := agent.StartSession(rp, c, opts)
	if err != nil {
		return nil, err
	}

	return sess, nil
}

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
func writeStartupLog(w io.Writer, line string) error {
	data, err := agent.MarshalMessage(&agent.LogMessage{MessageType: "caic_log", Line: line})
	if err != nil {
		return fmt.Errorf("marshal Pi startup log: %w", err)
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err != nil {
		return fmt.Errorf("write Pi startup log: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write Pi startup log: %w", io.ErrShortWrite)
	}
	return nil
}

// writeStartupLogOutput writes each non-empty command-output line to the task log.
func writeStartupLogOutput(w io.Writer, command, output string) error {
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := writeStartupLog(w, command+": "+line); err != nil {
			return err
		}
	}
	return nil
}

// caicModelInfo is written to output.jsonl during Start so replay/adoption
// can restore the model's context window. Without it, the context window
// defaults to the harness's hardcoded fallback (200k) after server restart.
type caicModelInfo struct {
	Type          string `json:"type"` // always "caic_model_info"
	ContextWindow int64  `json:"context_window"`
}

// piConn wraps a default Conn to intercept extension_ui_request messages
// during ReadMessages and auto-respond on stdin.
type piConn struct {
	agent.Conn

	logW io.Writer
	wire *piWireFormat
}

// newPiConn creates a piConn wrapping a standard Conn.
func newPiConn(stdin io.WriteCloser, logW io.Writer, wire *piWireFormat) *piConn {
	return &piConn{
		Conn: agent.NewConn(stdin, logW, wire),
		logW: logW,
		wire: wire,
	}
}

// ReadMessages overrides the default read loop to intercept extension_ui_request
// messages and auto-respond before forwarding them to the message channel.
func (c *piConn) ReadMessages(r io.Reader, msgCh chan<- agent.Message) error {
	return agent.DefaultReadMessages(r, func(m agent.Message) {
		// Intercept extension UI requests.
		if raw, ok := m.(*agent.RawMessage); ok && raw.MessageType == string(pi.EventExtensionUI) {
			if err := handleExtensionUI(c.Conn, raw.Raw); err != nil {
				slog.Warn("pi: extension_ui_request auto-response failed", "err", err)
			}
			// Don't forward to msgCh — these are internal.
			return
		}
		msgCh <- m
	}, c.logW, c.wire.ParseMessage)
}

// handleExtensionUI auto-responds to an extension UI request. For confirm
// requests it sends confirmed=true. For select requests it picks the first
// option. For all others it sends an empty value.
func handleExtensionUI(conn agent.Conn, raw []byte) error {
	var req pi.ExtensionUIRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("unmarshal extension_ui_request: %w", err)
	}
	switch req.Method {
	case pi.UIMethodConfirm:
		resp := pi.ExtensionUIResponseConfirm{Type: pi.ExtensionUIResponseType, ID: req.ID, Confirmed: true}
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		return conn.SendRaw(append(data, '\n'))
	case pi.UIMethodSelect:
		val := ""
		if len(req.Options) > 0 {
			val = req.Options[0]
		}
		resp := pi.ExtensionUIResponseValue{Type: pi.ExtensionUIResponseType, ID: req.ID, Value: val}
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		return conn.SendRaw(append(data, '\n'))
	case pi.UIMethodNotify, pi.UIMethodSetStatus, pi.UIMethodSetWidget, pi.UIMethodSetTitle:
		// Fire-and-forget; no response needed.
		return nil
	default:
		// input, editor, or unknown: send empty value.
		resp := pi.ExtensionUIResponseValue{Type: pi.ExtensionUIResponseType, ID: req.ID, Value: ""}
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		return conn.SendRaw(append(data, '\n'))
	}
}

// piWireFormat implements agent.WireFormat and agent.CompactCommand for Pi's
// type-dispatched JSONL protocol. It holds per-session state: a start time for
// duration tracking and a turn counter incremented by handleTurnEnd.
type piWireFormat struct {
	mu        sync.Mutex
	initSent  bool
	sessionID string
	startTime time.Time // When the prompt was written.
	numTurns  int       // Incremented by handleTurnEnd; consumed by handleAgentEnd.

	modelCtxWindow int64 // Model's context window from set_model response; 0 if unknown.

	// Per-tool accumulated output length for computing incremental deltas.
	// Pi's tool_execution_update events carry the full accumulated output;
	// we track the previous length to emit only the new portion.
	toolOutputLen map[string]int

	fw *jsonutil.FieldWarner
}

// WritePrompt sends a prompt command to Pi's stdin and records the start time
// for duration tracking.
func (w *piWireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	w.mu.Lock()
	w.startTime = time.Now()
	w.numTurns = 0
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
	return writeJSONLine(wr, cmd, logW)
}

// WriteCompact implements agent.CompactCommand.
func (w *piWireFormat) WriteCompact(wr io.Writer, instructions string, logW io.Writer) error {
	cmd := pi.CompactCmd{
		Type:               pi.CmdCompact,
		CustomInstructions: instructions,
	}
	return writeJSONLine(wr, cmd, logW)
}

// ParseMessage wraps the stateless parseMessage with stateful interceptions:
//
//   - done: emits ResultMessage (future Pi versions).
//   - agent_end: emits ResultMessage with usage + duration.
//   - turn_end: emits UsageMessage from turn's assistant message.
func (w *piWireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	typ, err := decodeEventType(line)
	if err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// Restore model context window from synthetic caic_model_info line
	// written during Start. This ensures replay/adoption correctly
	// reports the model's real context window instead of falling back
	// to the harness hardcoded default of 200k.
	if typ == "caic_model_info" {
		var info caicModelInfo
		if err := json.Unmarshal(line, &info); err == nil && info.ContextWindow > 0 {
			w.modelCtxWindow = info.ContextWindow
		}
		return nil, nil
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
			return messagesFromMessageUpdateDelta(&ev.AssistantMessageEvent, line)
		}
	}

	msgs, err := parseMessage(line, w.fw)
	if err != nil {
		return nil, err
	}

	// For tool output deltas, compute incremental deltas since Pi's
	// tool_execution_update events carry the full accumulated output.
	for _, msg := range msgs {
		m, ok := msg.(*agent.ToolOutputDeltaMessage)
		if !ok {
			continue
		}
		w.mu.Lock()
		if w.toolOutputLen == nil {
			w.toolOutputLen = make(map[string]int)
		}
		prev := w.toolOutputLen[m.ToolUseID]
		if prev < len(m.Delta) {
			m.Delta = m.Delta[prev:]
			w.toolOutputLen[m.ToolUseID] = prev + len(m.Delta)
		} else {
			m.Delta = ""
		}
		w.mu.Unlock()
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
func (w *piWireFormat) handleDone(ev *pi.MessageUpdateDeltaEvent) ([]agent.Message, error) {
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
		return errorResultMessages(ev.Message.ErrorMessage), nil
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

// handleMessageStart emits a one-shot InitMessage carrying the model name on
// the first message_start event that contains a non-empty model field.
func (w *piWireFormat) handleMessageStart(line []byte) ([]agent.Message, error) {
	var ev pi.MessageStartEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal message_start: %w", err)
	}
	if ev.Message.Model == "" {
		return nil, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.initSent {
		return nil, nil
	}
	w.initSent = true
	model := ev.Message.Model
	if ev.Message.Provider != "" {
		model = ev.Message.Provider + "/" + model
	}
	return []agent.Message{&agent.InitMessage{SessionID: w.sessionID, Model: model}}, nil
}

// handleError converts an error delta into a ResultMessage.
func (w *piWireFormat) handleError(ev *pi.MessageUpdateDeltaEvent) ([]agent.Message, error) {
	result := ""
	if ev.AssistantMessageEvent.Error != nil && ev.AssistantMessageEvent.Error.ErrorMessage != "" {
		result = ev.AssistantMessageEvent.Error.ErrorMessage
	}
	return errorResultMessages(result), nil
}

// errorResultMessages builds the terminal messages for a Pi error stop. When
// the error text names a provider quota or rate limit, it prepends a
// RateLimitMessage so the UI surfaces the quota banner instead of a bare error;
// Pi relays only free-text errors, so status is "rejected" with no reset time.
func errorResultMessages(errMsg string) []agent.Message {
	var msgs []agent.Message
	if isQuotaError(errMsg) {
		msgs = append(msgs, &agent.RateLimitMessage{Status: "rejected"})
	}
	return append(msgs, &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      errMsg,
	})
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

// handleAgentEnd extracts final usage from the last assistant message and emits
// a ResultMessage with usage and duration.
func (w *piWireFormat) handleAgentEnd(line []byte) ([]agent.Message, error) {
	var ev pi.AgentEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal agent_end: %w", err)
	}

	// Find the last assistant message for usage.
	var usage agent.Usage
	for i := range slices.Backward(ev.Messages) {
		msg := &ev.Messages[i]
		if msg.Role != pi.RoleAssistant {
			continue
		}
		usage = agent.Usage{
			InputTokens:              int(msg.Usage.Input),
			OutputTokens:             int(msg.Usage.Output),
			CacheReadInputTokens:     int(msg.Usage.CacheRead),
			CacheCreationInputTokens: int(msg.Usage.CacheWrite),
		}
		break
	}

	w.mu.Lock()
	var durationMs int64
	if !w.startTime.IsZero() {
		durationMs = time.Since(w.startTime).Milliseconds()
		w.startTime = time.Time{}
	}
	numTurns := w.numTurns
	w.numTurns = 0
	w.mu.Unlock()

	return []agent.Message{&agent.ResultMessage{
		MessageType: "result",
		Subtype:     "result",
		DurationMs:  durationMs,
		NumTurns:    numTurns,
		Usage:       usage,
	}}, nil
}

// handleTurnEnd extracts per-turn usage from the turn's assistant message and
// increments the turn counter consumed by handleAgentEnd.
func (w *piWireFormat) handleTurnEnd(line []byte) ([]agent.Message, error) {
	var ev pi.TurnEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal turn_end: %w", err)
	}
	w.mu.Lock()
	w.numTurns++
	w.mu.Unlock()
	if ev.Message.Role == pi.RoleAssistant && ev.Message.Usage.TotalTokens > 0 {
		return []agent.Message{&agent.UsageMessage{
			Usage: agent.Usage{
				InputTokens:              int(ev.Message.Usage.Input),
				OutputTokens:             int(ev.Message.Usage.Output),
				CacheReadInputTokens:     int(ev.Message.Usage.CacheRead),
				CacheCreationInputTokens: int(ev.Message.Usage.CacheWrite),
			},
			ContextWindow: int(w.modelCtxWindow),
		}}, nil
	}
	return nil, nil
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

// waitForResponse reads JSONL lines from r until a response for the given
// command is found. Returns the full response envelope. Lines are logged to
// logW. Non-response events are discarded (Pi should not emit any before the
// first prompt).
func waitForResponse(r *bufio.Reader, cmd pi.CommandType, logW io.Writer) (pi.Response, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return pi.Response{}, fmt.Errorf("read response for %s: %w", cmd, err)
		}
		if logW != nil {
			_, _ = logW.Write(line)
		}
		var probe pi.LineProbe
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Type == "caic_exit" {
			var exit agent.ExitMessage
			if json.Unmarshal(line, &exit) == nil && exit.ExitCode != 0 {
				return pi.Response{}, &piProcessExitError{exit: exit}
			}
		}
		if probe.Type != pi.EventResponse {
			continue
		}
		var resp pi.Response
		if json.Unmarshal(line, &resp) != nil {
			continue
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

func waitForResponseContext(ctx context.Context, r *bufio.Reader, cmd pi.CommandType, logW io.Writer, onTimeout func()) (pi.Response, error) {
	type result struct {
		resp pi.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := waitForResponse(r, cmd, logW)
		ch <- result{resp: resp, err: err}
	}()
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

// parseModelContextWindow extracts the context window from a set_model response.
func parseModelContextWindow(resp *pi.Response) int64 {
	if resp.Command != pi.CmdSetModel {
		return 0
	}
	var m pi.Model
	if err := json.Unmarshal(resp.Data, &m); err != nil {
		return 0
	}
	return m.ContextWindow
}

// writeSetModel sends a set_model command to Pi. The model string is split on
// "/" into provider + modelId (e.g. "cerebras/gpt-oss-120b").
func writeSetModel(w io.Writer, model string, logW io.Writer) error {
	provider, modelID := "", model
	if p, m, ok := strings.Cut(model, "/"); ok {
		provider, modelID = p, m
	}
	cmd := pi.SetModelCmd{
		Type:     pi.CmdSetModel,
		Provider: provider,
		ModelID:  modelID,
	}
	return writeJSONLine(w, cmd, logW)
}

// writeSetThinking sends a set_thinking_level command to Pi.
func writeSetThinking(w io.Writer, level string, logW io.Writer) error {
	cmd := pi.SetThinkingCmd{
		Type:  pi.CmdSetThinking,
		Level: pi.ThinkingLevel(level),
	}
	return writeJSONLine(w, cmd, logW)
}

func writeGetState(w, logW io.Writer) error {
	return writeJSONLine(w, pi.GetStateCmd{Type: pi.CmdGetState}, logW)
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
	req := struct {
		Type pi.CommandType `json:"type"`
	}{Type: pi.CmdGetModels}
	data, err := json.Marshal(req)
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
	ids := make([]string, 0, len(models))
	for i := range models {
		id := models[i].GetID()
		if id == "" {
			continue
		}
		ids = append(ids, id)
		efforts[id] = effortOptions(&models[i])
	}

	ids = agent.SortModels(ids)
	result := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		result = append(result, agent.Model{ID: id, EffortOptions: efforts[id]})
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
func writeJSONLine(w io.Writer, v any, logW io.Writer) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
}
