// Package pi implements agent.Backend for Pi coding agent CLI in RPC mode.
//
// Pi uses a custom type-dispatched JSONL protocol over stdin/stdout (not
// JSON-RPC 2.0). There is no handshake — the subprocess is immediately ready
// to accept commands after launch.
//
// Per-session state (accumulated usage, text/thinking buffers) is managed by
// piWireFormat, which wraps the stateless parseMessage function. A fresh
// piWireFormat is created for every Start, AttachRelay, and ReadRelayOutput
// call so that accumulators reset between sessions and replays.
//
// Extension UI auto-response is handled by piConn, which wraps the default
// Conn to intercept extension_ui_request messages during ReadMessages and
// write confirm/select responses back to Pi's stdin.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	pi "github.com/maruel/genai/providers/pi"
)

// Backend implements agent.Backend for the Pi coding agent.
type Backend struct {
	agent.Base

	mu      sync.Mutex
	cache   *agent.HarnessCache
	EnvVars []string // KEY=VALUE pairs for FetchModels SSH commands
}

var _ agent.Backend = (*Backend)(nil)

// New creates a Pi backend. If cacheDir is non-empty, the model list is loaded
// from the on-disk harness cache; otherwise a hardcoded default is used.
// envVars are KEY=VALUE pairs passed to FetchModels SSH commands so Pi sees
// API keys without requiring a login shell.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{EnvVars: envVars}
	b.Base = agent.Base{
		HarnessID:     agent.Pi,
		Images:        true,
		ContextWindow: 200_000,
	}
	if cacheDir != "" {
		b.cache = agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
		if models, _ := b.cache.Models(agent.Pi); len(models) > 0 {
			b.ModelList = agent.SortModels(models)
		}
	}
	return b
}

// Models returns the current model list, updated dynamically after each Start.
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

// NewParser implements agent.Backend.
func (*Backend) NewParser() func([]byte) ([]agent.Message, error) {
	fw := &jsonutil.FieldWarner{}
	return func(line []byte) ([]agent.Message, error) { return parseMessage(line, fw) }
}

// Start launches a Pi RPC process via the relay daemon. It sends optional
// set_model commands before the initial prompt.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	wire := &piWireFormat{fw: &jsonutil.FieldWarner{}}
	b.Wire = wire

	rp, err := agent.PrepareRelay(ctx, opts, buildArgs())
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
			return nil, fmt.Errorf("pi: set_model %s: %w", opts.Model, err)
		}
		if cw := parseModelContextWindow(&resp); cw > 0 {
			wire.modelCtxWindow = cw
			// Persist to relay output so replay/adoption restores the
			// context window (otherwise it falls back to the harness
			// hardcoded default of 200k).
			info, _ := json.Marshal(caicModelInfo{
				Type:          "caic_model_info",
				ContextWindow: cw,
			})
			_, _ = opts.LogW.Write(append(info, '\n'))
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
			return nil, fmt.Errorf("pi: set_thinking_level %s: %w", opts.Effort, err)
		}
		rp.Stdout = br
	}

	c := newPiConn(rp.Stdin, opts.LogW, wire)
	sess, err := agent.StartSession(rp, c, opts)
	if err != nil {
		return nil, err
	}

	// Opportunistically refresh models in background using the task's container.
	if b.cache != nil {
		if _, fresh := b.cache.Models(agent.Pi); !fresh {
			container := opts.Container
			go func() { //nolint:gosec,contextcheck // intentionally detached from request context; must outlive Start()
				fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if models, err := FetchModels(fetchCtx, container, b.EnvVars); err != nil {
					slog.Warn("pi: background model fetch failed", "err", err)
				} else {
					sorted := agent.SortModels(models)
					b.mu.Lock()
					b.ModelList = sorted
					b.mu.Unlock()
					// Store raw models so the cache survives blacklist changes.
					b.cache.SetModels(agent.Pi, models)
				}
			}()
		}
	}
	return sess, nil
}

// AttachRelay connects to an already-running relay in the container.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	wire := &piWireFormat{fw: &jsonutil.FieldWarner{}}
	return agent.AttachRelaySession(ctx, opts, wire)
}

// ReadRelayOutput reads relay output using a fresh piWireFormat.
func (b *Backend) ReadRelayOutput(ctx context.Context, container string) ([]agent.Message, int64, error) {
	wire := &piWireFormat{fw: &jsonutil.FieldWarner{}}
	return agent.ReadRelayOutput(ctx, container, wire.ParseMessage)
}

// buildArgs constructs the Pi CLI arguments for RPC mode.
func buildArgs() []string {
	return []string{"pi", "--mode", "rpc", "--no-session"}
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
// type-dispatched JSONL protocol. It holds per-session state: text/thinking
// buffers for synthetic final messages, a start time for duration tracking,
// and a turn counter incremented by handleTurnEnd.
type piWireFormat struct {
	mu         sync.Mutex
	initSent   bool
	textAccum  strings.Builder
	thinkAccum strings.Builder
	startTime  time.Time // When the prompt was written.
	numTurns   int       // Incremented by handleTurnEnd; consumed by handleAgentEnd.

	modelCtxWindow int64 // Model's context window from set_model response; 0 if unknown.

	fw *jsonutil.FieldWarner
}

// WritePrompt sends a prompt command to Pi's stdin and records the start time
// for duration tracking.
func (w *piWireFormat) WritePrompt(wr io.Writer, p agent.Prompt, logW io.Writer) error {
	w.mu.Lock()
	w.textAccum.Reset()
	w.thinkAccum.Reset()
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
//   - text_delta/thinking_delta: accumulated for the final ResultMessage.
//   - done: emits accumulated text/thinking + ResultMessage (future Pi versions).
//   - agent_end: emits ResultMessage with accumulated text + usage + duration.
//   - turn_end: emits UsageMessage from turn's assistant message.
func (w *piWireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	var probe pi.LineProbe
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}

	// Restore model context window from synthetic caic_model_info line
	// written during Start. This ensures replay/adoption correctly
	// reports the model's real context window instead of falling back
	// to the harness hardcoded default of 200k.
	if probe.Type == "caic_model_info" {
		var info caicModelInfo
		if err := json.Unmarshal(line, &info); err == nil && info.ContextWindow > 0 {
			w.modelCtxWindow = info.ContextWindow
		}
		return nil, nil
	}

	// Intercept agent_end for final usage.
	if probe.Type == pi.EventAgentEnd {
		return w.handleAgentEnd(line)
	}

	// Intercept turn_end for per-turn usage.
	if probe.Type == pi.EventTurnEnd {
		return w.handleTurnEnd(line)
	}

	// Intercept message_start to report the model on the first turn.
	if probe.Type == pi.EventMessageStart {
		return w.handleMessageStart(line)
	}

	// Intercept message_end with stopReason=error for error ResultMessage.
	if probe.Type == pi.EventMessageEnd {
		return w.handleMessageEnd(line)
	}

	// Intercept message_update with done/error delta for ResultMessage.
	if probe.Type == pi.EventMessageUpdate {
		var ev pi.MessageUpdateEvent
		if err := json.Unmarshal(line, &ev); err == nil {
			switch ev.AssistantMessageEvent.Type {
			case pi.DeltaDone:
				return w.handleDone(&ev)
			case pi.DeltaError:
				return w.handleError(&ev)
			default:
				// Other delta types (text_delta, thinking_delta, etc.)
				// are handled by the stateless parseMessage below.
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

// handleDone converts a done delta into synthetic final messages + a
// ResultMessage. Pi currently does not emit done deltas, so this path is
// not exercised in normal operation, but is kept for protocol evolution.
func (w *piWireFormat) handleDone(ev *pi.MessageUpdateEvent) ([]agent.Message, error) {
	w.mu.Lock()
	var msgs []agent.Message
	if w.thinkAccum.Len() > 0 {
		msgs = append(msgs, &agent.ThinkingMessage{Text: w.thinkAccum.String()})
		w.thinkAccum.Reset()
	}
	turnText := ""
	if w.textAccum.Len() > 0 {
		turnText = w.textAccum.String()
		w.textAccum.Reset()
	}
	if turnText == "" && ev.AssistantMessageEvent.Message != nil {
		turnText = extractTextContent(ev.AssistantMessageEvent.Message.Content)
	}
	if turnText != "" {
		msgs = append(msgs, &agent.TextMessage{Text: turnText})
	}
	rm := &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "result",
		Result:      turnText,
	}
	if ev.AssistantMessageEvent.Reason == pi.StopReasonError {
		rm.IsError = true
	}
	w.mu.Unlock()
	msgs = append(msgs, rm)
	return msgs, nil
}

// extractTextContent joins text from content blocks into a single string.
func extractTextContent(blocks pi.ContentBlocks) string {
	var b strings.Builder
	for i := range blocks {
		if blocks[i].Text != "" {
			b.WriteString(blocks[i].Text)
		}
	}
	return b.String()
}

// handleMessageEnd handles a message_end event, emitting an error ResultMessage
// when stopReason is "error".
func (w *piWireFormat) handleMessageEnd(line []byte) ([]agent.Message, error) {
	var ev pi.MessageEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal message_end: %w", err)
	}
	if ev.Message.StopReason != pi.StopReasonError {
		return nil, nil
	}
	w.mu.Lock()
	w.textAccum.Reset()
	w.thinkAccum.Reset()
	w.mu.Unlock()
	return []agent.Message{&agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      ev.Message.ErrorMessage,
	}}, nil
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
	return []agent.Message{&agent.InitMessage{Model: model}}, nil
}

// handleError converts an error delta into a ResultMessage.
func (w *piWireFormat) handleError(ev *pi.MessageUpdateEvent) ([]agent.Message, error) {
	w.mu.Lock()
	w.textAccum.Reset()
	w.thinkAccum.Reset()
	w.mu.Unlock()

	result := ""
	if ev.AssistantMessageEvent.Error != nil && ev.AssistantMessageEvent.Error.ErrorMessage != "" {
		result = ev.AssistantMessageEvent.Error.ErrorMessage
	}
	return []agent.Message{&agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      result,
	}}, nil
}

// handleAgentEnd extracts final usage from the last assistant message and
// emits a ResultMessage with the accumulated session text, usage, and duration.
// Pi does not emit done deltas, so the text from text_delta events is consumed
// here to populate the frontend "Done" card.
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
	// Consume accumulated streaming text for the final "Done" card.
	// If text_delta events were not emitted (non-streaming), fall back
	// to extracting text from the last assistant message's content blocks.
	resultText := ""
	if w.textAccum.Len() > 0 {
		resultText = w.textAccum.String()
		w.textAccum.Reset()
	}
	if resultText == "" {
		for i := range slices.Backward(ev.Messages) {
			msg := &ev.Messages[i]
			if msg.Role != pi.RoleAssistant {
				continue
			}
			resultText = extractTextContent(msg.Content)
			break
		}
	}
	w.thinkAccum.Reset()

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
		Result:      resultText,
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

// FetchModels SSHes into the given container, runs pi --mode rpc --no-session,
// sends get_available_models, and returns the model ID list.
// extraEnv holds KEY=VALUE pairs injected via the env command so Pi sees them
// without requiring a login shell that sources ~/.env.
func FetchModels(ctx context.Context, container string, extraEnv []string) ([]string, error) {
	args := []string{container}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, "pi", "--mode", "rpc", "--no-session")
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // container is not user-controlled
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
		models := make([]string, 0, len(payload.Models))
		for i := range payload.Models {
			models = append(models, payload.Models[i].GetID())
		}
		return models, nil
	}
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
