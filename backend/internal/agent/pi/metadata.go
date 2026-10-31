// Pi metadata projection shares native session normalization and usage accounting without decoding transcript content.

package pi

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/pi"
)

// piMetadata owns the native metadata and accounting lifetime for either projection.
type piMetadata struct {
	usageSinceResult bool

	mu             sync.Mutex
	session        agent.SessionMetadata
	startTime      time.Time
	numTurns       int
	modelCtxWindow int64
	modelProvider  string
}

func (w *piMetadata) ParseInput(line []byte) ([]agent.Message, error) {
	typ, err := decodeEventType(line)
	if err == nil && typ == pi.CmdPrompt {
		w.beginPrompt()
	}
	return nil, err
}

func (w *piMetadata) ParseOutput(line []byte) ([]agent.Message, error) {
	typ, err := decodeEventType(line)
	if err != nil {
		return nil, err
	}
	if typ == pi.EventAgentStart {
		w.usageSinceResult = false
	}
	var msgs []agent.Message
	switch typ {
	case pi.EventMessageStart:
		msgs, err = w.handleMessageStart(line)
	case pi.EventTurnEnd:
		msgs, err = w.handleTurnEnd(line)
	case pi.EventAgentEnd:
		msgs, err = w.handleAgentEnd(line)
	case pi.EventResponse:
		resp, decodeErr := decodeResponse(line)
		if decodeErr != nil {
			return nil, decodeErr
		}
		msgs, err = w.responseMessages(&resp)
	case pi.EventCompactionStart, pi.EventCompactionEnd:
		msgs, _, err = parseMessageTyped(typ, line)
	case pi.EventMessageEnd:
		msg, decodeErr := decodeUsageMessage(line, "message")
		if decodeErr != nil {
			return nil, decodeErr
		}
		if msg.StopReason == pi.StopReasonError {
			msgs = w.errorResultMessages(msg.ErrorMessage)
		}
	case pi.EventMessageUpdate:
		// Delta content is irrelevant; only a terminal error/done changes coverage.
		kind, decodeErr := agent.JSONString(line, "assistantMessageEvent", "type")
		if decodeErr != nil {
			return nil, decodeErr
		}
		if pi.DeltaType(kind) == pi.DeltaError || pi.DeltaType(kind) == pi.DeltaDone {
			if pi.DeltaType(kind) == pi.DeltaError {
				detail, err := agent.JSONString(line, "assistantMessageEvent", "error", "errorMessage")
				if err != nil {
					return nil, err
				}
				msgs = w.errorResultMessages(detail)
			} else {
				reason, err := agent.JSONString(line, "assistantMessageEvent", "reason")
				if err != nil {
					return nil, err
				}
				msgs = []agent.Message{&agent.ResultMessage{MessageType: "result", Subtype: "result", IsError: pi.StopReason(reason) == pi.StopReasonError}}
			}
		}
	default:
		// Conversation deltas and extension tool lifecycles have no root metadata.
	}
	if err != nil {
		return nil, err
	}
	w.applyMetadata(msgs)
	w.markAccounting(typ, msgs)
	return msgs, nil
}

// beginPrompt applies the committed input boundary in both native projections.
func (w *piMetadata) beginPrompt() {
	w.mu.Lock()
	w.startTime = time.Now()
	w.numTurns = 0
	w.mu.Unlock()
}

func (w *piMetadata) applyMetadata(msgs []agent.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range msgs {
		w.session.Apply(m)
		if model, ok := m.(*agent.ModelInfoMessage); ok {
			w.modelCtxWindow = model.ContextWindow
		}
	}
}

// observeResponse preserves only a provider the native command actually reports.
func (w *piMetadata) observeResponse(resp *pi.Response) error {
	if !resp.Success {
		return nil
	}
	var provider string
	var err error
	switch resp.Command {
	case pi.CmdGetState:
		provider, err = agent.JSONString(resp.Data, "model", "provider")
	case pi.CmdSetModel:
		provider, err = agent.JSONString(resp.Data, "provider")
	default:
		// Other command acknowledgments do not report an active provider.
	}
	if err != nil {
		return err
	}
	if provider != "" {
		w.mu.Lock()
		w.modelProvider = provider
		w.mu.Unlock()
	}
	return nil
}

func (w *piMetadata) markAccounting(typ pi.EventType, msgs []agent.Message) {
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *agent.UsageMessage:
			m.Additive = true
			if !m.Source.IsChild() {
				w.usageSinceResult = true
			}
		case *agent.ResultMessage:
			if !m.Source.IsChild() {
				m.UsageIsSnapshot = w.usageSinceResult
				if typ == pi.EventAgentEnd {
					w.usageSinceResult = false
				}
			}
		}
	}
}

// handleMessageStart reports the configured or served model for each native
// assistant message without inventing a session boundary or identity.
func (w *piMetadata) handleMessageStart(line []byte) ([]agent.Message, error) {
	msg, err := decodeUsageMessage(line, "message")
	if err != nil {
		return nil, fmt.Errorf("unmarshal message_start: %w", err)
	}
	if msg.Model == "" && msg.ResponseModel == "" {
		return nil, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if msg.Provider != "" {
		w.modelProvider = msg.Provider
	}
	return []agent.Message{&agent.SessionMetadataMessage{ReportedModel: resolvedModel(msg.Provider, msg.ResponseModel, msg.Model)}}, nil
}

// errorResultMessages builds the terminal messages for a Pi error stop. When
// the error text names a provider quota or rate limit, it prepends a
// RateLimitMessage attributed to the active model's quota provider so the UI
// surfaces the quota banner instead of a bare error; Pi relays only free-text
// errors, so status is "rejected" with no reset time or window.
func (w *piMetadata) errorResultMessages(errMsg string) []agent.Message {
	var msgs []agent.Message
	if isQuotaError(errMsg) {
		w.mu.Lock()
		provider := agent.QuotaProviderForModel(w.modelProvider + "/x")
		w.mu.Unlock()
		if provider.Valid() {
			msgs = append(msgs, &agent.RateLimitMessage{
				Status:        "rejected",
				QuotaProvider: provider,
				QuotaLabel:    provider.String(),
				QuotaWindow:   agent.QuotaWindowRequest,
			})
		} else {
			msgs = append(msgs, &agent.RateLimitMessage{Status: "rejected"})
		}
	}
	return append(msgs, &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      errMsg,
	})
}

// handleAgentEnd sums the invocation's assistant costs and emits a ResultMessage
// with the last call's usage and the invocation duration.
func (w *piMetadata) handleAgentEnd(line []byte) ([]agent.Message, error) {
	var envelope agentEndEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("unmarshal agent_end envelope: %w", err)
	}
	if envelope.WillRetry {
		return nil, nil
	}

	// The terminal array includes full transcript messages; borrow each entry and
	// decode only the assistant role and usage needed by accounting.
	var usage agent.Usage
	var totalCostUSD float64
	var decodeErr error
	_, err := jsonparser.ArrayEach(line, func(raw []byte, _ jsonparser.ValueType, _ int, err error) {
		if decodeErr != nil {
			return
		}
		if err != nil {
			decodeErr = err
			return
		}
		msg, err := decodeUsageMessage(raw)
		if err != nil {
			decodeErr = err
			return
		}
		if msg.Role == pi.RoleAssistant {
			usage = toAgentUsage(&msg.Usage)
			totalCostUSD += msg.Usage.Cost.Total
		}
	}, "messages")
	if err != nil {
		return nil, fmt.Errorf("unmarshal agent_end messages: %w", err)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("unmarshal agent_end usage: %w", decodeErr)
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
		MessageType:  "result",
		Subtype:      "result",
		DurationMs:   durationMs,
		NumTurns:     numTurns,
		TotalCostUSD: totalCostUSD,
		Usage:        usage,
	}}, nil
}

// handleTurnEnd extracts per-turn usage from the turn's assistant message and
// increments the turn counter consumed by handleAgentEnd.
func (w *piMetadata) handleTurnEnd(line []byte) ([]agent.Message, error) {
	msg, err := decodeUsageMessage(line, "message")
	if err != nil {
		return nil, fmt.Errorf("unmarshal turn_end: %w", err)
	}
	w.mu.Lock()
	if msg.Provider != "" {
		w.modelProvider = msg.Provider
	}
	w.numTurns++
	window := w.modelCtxWindow
	w.mu.Unlock()
	if msg.Role == pi.RoleAssistant && msg.Usage.TotalTokens > 0 {
		return []agent.Message{&agent.UsageMessage{
			PerCall:         true,
			Usage:           toAgentUsage(&msg.Usage),
			ReportedCostUSD: &msg.Usage.Cost.Total,
			ReportedModel:   resolvedModel(msg.Provider, msg.ResponseModel, msg.Model),
			ContextWindow:   int(window),
		}}, nil
	}
	return nil, nil
}

type metadataStringField struct {
	key string
	dst *string
}

// decodeUsageMessage selects metadata fields, avoiding content/tool/image copies.
func decodeUsageMessage(line []byte, keys ...string) (pi.AgentMessage, error) {
	raw := line
	if len(keys) > 0 {
		value, err := agent.JSONField(line, keys...)
		if err != nil {
			return pi.AgentMessage{}, err
		}
		raw = value
	}
	var msg pi.AgentMessage
	if err := requireMetadataObject(raw); err != nil {
		return msg, err
	}
	fields := []metadataStringField{
		{"role", (*string)(&msg.Role)}, {"provider", &msg.Provider}, {"model", &msg.Model},
		{"responseModel", &msg.ResponseModel}, {"stopReason", (*string)(&msg.StopReason)}, {"errorMessage", &msg.ErrorMessage},
	}
	for _, f := range fields {
		value, err := agent.JSONString(raw, f.key)
		if err != nil {
			return msg, err
		}
		*f.dst = value
	}
	usage, err := agent.JSONField(raw, "usage")
	if err != nil {
		return msg, err
	}
	if usage != nil {
		if err := json.Unmarshal(usage, &msg.Usage); err != nil {
			return msg, err
		}
	}
	return msg, nil
}

func stateMetadata(data []byte) (agent.SessionMetadata, error) {
	if err := requireMetadataObject(data); err != nil {
		return agent.SessionMetadata{}, err
	}
	rawModel, err := agent.JSONField(data, "model")
	if err != nil {
		return agent.SessionMetadata{}, err
	}
	if err := requireMetadataObject(rawModel); err != nil {
		return agent.SessionMetadata{}, err
	}
	sid, err := agent.JSONString(data, "sessionId")
	if err != nil {
		return agent.SessionMetadata{}, err
	}
	effort, err := agent.JSONString(data, "thinkingLevel")
	if err != nil {
		return agent.SessionMetadata{}, err
	}
	provider, err := agent.JSONString(data, "model", "provider")
	if err != nil {
		return agent.SessionMetadata{}, err
	}
	model, err := agent.JSONString(data, "model", "id")
	if err != nil {
		return agent.SessionMetadata{}, err
	}
	return agent.SessionMetadata{SessionID: sid, ReportedModel: resolvedModel(provider, "", model), ReportedEffort: effort}, nil
}

// responseMessages shares command normalization and native session ownership
// across live startup, transcript replay, and lightweight metadata projection.
func (w *piMetadata) responseMessages(resp *pi.Response) ([]agent.Message, error) {
	if err := w.observeResponse(resp); err != nil {
		return nil, err
	}
	if !resp.Success && resp.Command == pi.CmdPrompt && resp.Error != "" {
		return []agent.Message{&agent.ResultMessage{MessageType: "result", Subtype: "error", IsError: true, Result: resp.Error}}, nil
	}
	if !resp.Success {
		return nil, nil
	}
	switch resp.Command {
	case pi.CmdGetState:
		metadata, err := stateMetadata(resp.Data)
		if err != nil {
			return nil, fmt.Errorf("get_state metadata: %w", err)
		}
		if metadata == (agent.SessionMetadata{}) {
			return nil, nil
		}
		w.mu.Lock()
		known := w.session
		w.mu.Unlock()
		if metadata.SessionID != "" && metadata.SessionID != known.SessionID {
			known.Update(metadata)
			return []agent.Message{&agent.InitMessage{
				SessionID: known.SessionID, ReportedModel: known.ReportedModel, ReportedEffort: known.ReportedEffort,
				Version: known.AgentVersion, AutoCompactTokenLimit: known.AutoCompactTokenLimit,
			}}, nil
		}
		return []agent.Message{&agent.SessionMetadataMessage{SessionMetadata: metadata}}, nil
	case pi.CmdSetModel:
		if err := requireMetadataObject(resp.Data); err != nil {
			return nil, err
		}
		provider, err := agent.JSONString(resp.Data, "provider")
		if err != nil {
			return nil, err
		}
		model, err := agent.JSONString(resp.Data, "id")
		if err != nil {
			return nil, err
		}
		var msgs []agent.Message
		if reported := resolvedModel(provider, "", model); reported != "" {
			msgs = append(msgs, &agent.SessionMetadataMessage{ReportedModel: reported})
		}
		var window int64
		if err := decodeMetadataField(resp.Data, &window, "contextWindow"); err != nil {
			return nil, err
		}
		if window > 0 {
			msgs = append(msgs, &agent.ModelInfoMessage{MessageType: "caic_model_info", ContextWindow: window})
		}
		return msgs, nil
	default:
		// Other commands report no supported session metadata.
	}
	return nil, nil
}

// decodeResponse borrows the data payload; only a command's semantic consumer decodes it.
func decodeResponse(line []byte) (pi.Response, error) {
	var resp pi.Response
	command, err := agent.JSONString(line, "command")
	if err != nil {
		return resp, err
	}
	resp.Command = pi.EventType(command)
	resp.Error, err = agent.JSONString(line, "error")
	if err != nil {
		return resp, err
	}
	success, err := agent.JSONField(line, "success")
	if err != nil {
		return resp, err
	}
	if success != nil {
		if err := json.Unmarshal(success, &resp.Success); err != nil {
			return resp, err
		}
	}
	resp.Data, err = agent.JSONField(line, "data")
	return resp, err
}

// decodeMetadataField decodes an optional scalar or usage object without its surrounding transcript.
func decodeMetadataField(raw []byte, dst any, keys ...string) error {
	value, err := agent.JSONField(raw, keys...)
	if err != nil || value == nil {
		return err
	}
	return json.Unmarshal(value, dst)
}

func decodeCompactionEnd(line []byte) (pi.CompactionEndEvent, error) {
	var ev pi.CompactionEndEvent
	if err := decodeMetadataField(line, &ev.WillRetry, "willRetry"); err != nil {
		return ev, err
	}
	if err := decodeMetadataField(line, &ev.Aborted, "aborted"); err != nil {
		return ev, err
	}
	var err error
	ev.ErrorMessage, err = agent.JSONString(line, "errorMessage")
	if err != nil {
		return ev, err
	}
	result, err := agent.JSONField(line, "result")
	if err != nil || result == nil || string(result) == "null" {
		return ev, err
	}
	if err := requireMetadataObject(result); err != nil {
		return ev, err
	}
	ev.Result = &pi.CompactionResult{}
	if err := decodeMetadataField(result, &ev.Result.TokensBefore, "tokensBefore"); err != nil {
		return ev, err
	}
	if err := decodeMetadataField(result, &ev.Result.EstimatedTokensAfter, "estimatedTokensAfter"); err != nil {
		return ev, err
	}
	if err := decodeMetadataField(result, &ev.Result.Usage, "usage"); err != nil {
		return ev, err
	}
	return ev, nil
}

// requireMetadataObject preserves native DTO type checks while borrowing ignored fields.
func requireMetadataObject(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	_, kind, _, err := jsonparser.Get(raw)
	if err != nil {
		return err
	}
	if kind != jsonparser.Object && kind != jsonparser.Null {
		return fmt.Errorf("metadata field is %v, want object or null", kind)
	}
	return nil
}
