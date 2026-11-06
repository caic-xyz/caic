// Directional ACP metadata correlates native requests and normalizes session configuration and accounting.

package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/maruel/genai/acp"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
)

type pendingRequest struct {
	method    acp.Method
	sessionID string
}

// metadataState is the shared response owner for transport and metadata-only replay.
type metadataState struct {
	config *Config

	mu       sync.Mutex
	session  agent.SessionMetadata
	requests map[int64]pendingRequest
}

func (w *metadataState) ParseInput(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, err
	}
	if probe.ID == nil || string(probe.ID) == "null" || probe.Method == "" {
		return nil, nil
	}
	var id int64
	if err := json.Unmarshal(probe.ID, &id); err != nil {
		return nil, fmt.Errorf("unmarshal request ID: %w", err)
	}
	method := acp.Method(probe.Method)
	w.mu.Lock()
	defer w.mu.Unlock()
	if method == acp.MethodInitialize {
		clear(w.requests)
	}
	// A reused request ID replaces the old request, including with an unrelated method.
	delete(w.requests, id)
	switch method {
	case acp.MethodSessionPrompt, acp.MethodInitialize, acp.MethodSessionNew, acp.MethodSessionLoad, acp.MethodSessionResume, acp.MethodSessionSetConfigOption:
		sid, err := agent.JSONString(probe.Params, "sessionId")
		if err != nil {
			return nil, err
		}
		if w.requests == nil {
			w.requests = make(map[int64]pendingRequest)
		}
		if len(w.requests) == 1024 {
			return nil, errors.New("too many pending ACP metadata requests")
		}
		w.requests[id] = pendingRequest{method: method, sessionID: sid}
	default:
		// Other ACP requests do not carry session metadata or turn accounting.
	}
	return nil, nil
}

func (w *metadataState) ParseOutput(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, err
	}
	msgs, _, err := w.parseOutput(line, &probe)
	return msgs, err
}

func (w *metadataState) parseOutput(line []byte, probe *agent.JSONRPCEnvelope) ([]agent.Message, bool, error) {
	if probe.ID != nil && string(probe.ID) != "null" && probe.Method == "" {
		var id int64
		if err := json.Unmarshal(probe.ID, &id); err != nil {
			return nil, true, fmt.Errorf("unmarshal response ID: %w", err)
		}
		w.mu.Lock()
		request, matched := w.requests[id]
		delete(w.requests, id)
		foreign := request.sessionID != "" && w.session.SessionID != "" && request.sessionID != w.session.SessionID &&
			(request.method == acp.MethodSessionPrompt || request.method == acp.MethodSessionSetConfigOption)
		w.mu.Unlock()
		if foreign {
			return nil, true, nil
		}
		if matched && request.method == acp.MethodSessionPrompt {
			result, err := promptResult(line)
			if err != nil {
				return nil, true, err
			}
			return []agent.Message{result}, true, nil
		}
		if !matched {
			return nil, false, nil
		}
		failure, err := agent.JSONField(line, "error")
		if err != nil {
			return nil, true, err
		}
		if len(failure) > 0 && string(failure) != "null" {
			if err := requireMetadataObject(failure); err != nil {
				return nil, true, err
			}
			return nil, false, nil
		}
		result, err := agent.JSONField(line, "result")
		if err != nil {
			return nil, true, err
		}
		metadata, err := sessionResultMetadata(request.method, request.sessionID, result, w.config.EffortID)
		if err == nil {
			err = w.normalizeMetadata(&metadata)
		}
		if err != nil {
			return nil, true, err
		}
		w.mu.Lock()
		w.session.Update(metadata)
		known := w.session
		w.mu.Unlock()
		if request.method == acp.MethodSessionNew || request.method == acp.MethodSessionLoad || request.method == acp.MethodSessionResume {
			return []agent.Message{&agent.InitMessage{SessionID: known.SessionID, ReportedModel: known.ReportedModel, ReportedEffort: known.ReportedEffort, Version: known.AgentVersion}}, true, nil
		}
		report := &agent.SessionMetadataMessage{SessionMetadata: metadata}
		report.Source.ThreadID = request.sessionID
		return []agent.Message{report}, true, nil
	}
	if acp.Method(probe.Method) != acp.MethodSessionUpdate {
		return nil, false, nil
	}
	sid, err := agent.JSONString(probe.Params, "sessionId")
	if err != nil {
		return nil, true, err
	}
	w.mu.Lock()
	foreign := sid != "" && w.session.SessionID != "" && sid != w.session.SessionID
	w.mu.Unlock()
	if foreign {
		return nil, true, nil
	}
	update, err := agent.JSONField(probe.Params, "update")
	if err != nil {
		return nil, true, err
	}
	typ, err := agent.JSONString(update, "sessionUpdate")
	if err != nil {
		return nil, true, err
	}
	switch acp.UpdateType(typ) {
	case acp.UpdateUsageUpdate:
		msgs, err := usageUpdate(update)
		return msgs, true, err
	case acp.UpdateConfigOptionUpdate:
		metadata := agent.SessionMetadata{SessionID: sid}
		if err := parseConfigOptions(update, &metadata, w.config.EffortID); err != nil {
			return nil, true, err
		}
		if err := w.normalizeMetadata(&metadata); err != nil {
			return nil, true, err
		}
		w.mu.Lock()
		w.session.Update(metadata)
		w.mu.Unlock()
		return []agent.Message{&agent.SessionMetadataMessage{SessionMetadata: metadata}}, true, nil
	default:
		// Conversation and tool updates belong to the full projection.
	}
	return nil, false, nil
}

func sessionResultMetadata(method acp.Method, requestedSession string, result []byte, effortID acp.ConfigOptionID) (agent.SessionMetadata, error) {
	if err := requireMetadataObject(result); err != nil {
		return agent.SessionMetadata{}, err
	}
	metadata := agent.SessionMetadata{}
	switch method {
	case acp.MethodInitialize:
		info, err := agent.JSONField(result, "agentInfo")
		if err != nil {
			return agent.SessionMetadata{}, err
		}
		if err := requireMetadataObject(info); err != nil {
			return agent.SessionMetadata{}, err
		}
		metadata.AgentVersion, err = agent.JSONString(info, "version")
		return metadata, err
	case acp.MethodSessionNew, acp.MethodSessionLoad, acp.MethodSessionResume:
		var err error
		metadata.SessionID, err = agent.JSONString(result, "sessionId")
		if err != nil {
			return agent.SessionMetadata{}, err
		}
		if metadata.SessionID == "" && (method == acp.MethodSessionLoad || method == acp.MethodSessionResume) {
			metadata.SessionID = requestedSession
		}
		if metadata.SessionID == "" {
			return agent.SessionMetadata{}, errors.New("session response missing sessionId")
		}
		models, err := agent.JSONField(result, "models")
		if err != nil {
			return agent.SessionMetadata{}, err
		}
		if err := requireMetadataObject(models); err != nil {
			return agent.SessionMetadata{}, err
		}
		metadata.ReportedModel, err = agent.JSONString(models, "currentModelId")
		if err != nil {
			return agent.SessionMetadata{}, err
		}
		err = parseConfigOptions(result, &metadata, effortID)
		return metadata, err
	case acp.MethodSessionSetConfigOption:
		metadata.SessionID = requestedSession
		err := parseConfigOptions(result, &metadata, effortID)
		return metadata, err
	default:
		// The request owner calls this only for initialization and configuration.
		return metadata, nil
	}
}

func parseConfigOptions(raw []byte, metadata *agent.SessionMetadata, effortID acp.ConfigOptionID) error {
	options, err := agent.JSONField(raw, "configOptions")
	if err != nil || options == nil || string(options) == "null" {
		return err
	}
	var parseErr error
	_, err = jsonparser.ArrayEach(options, func(value []byte, _ jsonparser.ValueType, _ int, err error) {
		if parseErr != nil {
			return
		}
		if err != nil {
			parseErr = err
			return
		}
		if err := requireMetadataObject(value); err != nil {
			parseErr = err
			return
		}
		id, err := agent.JSONString(value, "id")
		if err != nil {
			parseErr = err
			return
		}
		current, err := agent.JSONField(value, "currentValue")
		if err != nil {
			parseErr = err
			return
		}
		parseErr = applyConfigValue(metadata, acp.ConfigOptionID(id), current, effortID)
	})
	if err != nil {
		return err
	}
	return parseErr
}

func applyConfigValue(metadata *agent.SessionMetadata, id acp.ConfigOptionID, value []byte, effortID acp.ConfigOptionID) error {
	switch id {
	case acp.ConfigOptionModel:
		if err := json.Unmarshal(value, &metadata.ReportedModel); err != nil {
			return fmt.Errorf("decode current model configuration: %w", err)
		}
	case effortID:
		if err := json.Unmarshal(value, &metadata.ReportedEffort); err != nil {
			return fmt.Errorf("decode current effort configuration: %w", err)
		}
	default:
		// Other controls do not report model or reasoning effort.
	}
	return nil
}

func usageUpdate(update []byte) ([]agent.Message, error) {
	var size int
	rawSize, err := agent.JSONField(update, "size")
	if err != nil {
		return nil, err
	}
	if rawSize != nil {
		if err := json.Unmarshal(rawSize, &size); err != nil {
			return nil, fmt.Errorf("usage_update size: %w", err)
		}
	}
	usage := &agent.UsageMessage{ContextWindow: size}
	currency, err := agent.JSONString(update, "cost", "currency")
	if err != nil {
		return nil, err
	}
	if currency == "USD" {
		var amount float64
		rawAmount, err := agent.JSONField(update, "cost", "amount")
		if err != nil {
			return nil, err
		}
		if rawAmount != nil {
			if err := json.Unmarshal(rawAmount, &amount); err != nil {
				return nil, fmt.Errorf("usage_update cost: %w", err)
			}
		}
		usage.CumulativeCostUSD = &amount
	}
	return []agent.Message{usage}, nil
}

func promptResult(line []byte) (*agent.ResultMessage, error) {
	rm := &agent.ResultMessage{MessageType: "result", Subtype: "result"}
	failure, err := agent.JSONField(line, "error")
	if err != nil {
		return nil, err
	}
	if failure != nil && string(failure) != "null" {
		if err := requireMetadataObject(failure); err != nil {
			return nil, err
		}
		rm.IsError = true
		rm.Result, err = agent.JSONString(failure, "message")
		return rm, err
	}
	result, err := agent.JSONField(line, "result")
	if err != nil {
		return nil, err
	}
	if result == nil {
		return rm, nil
	}
	if err := requireMetadataObject(result); err != nil {
		return nil, err
	}
	reason, err := agent.JSONString(result, "stopReason")
	if err != nil {
		return nil, err
	}
	if acp.StopReason(reason) == acp.StopReasonCancelled || acp.StopReason(reason) == acp.StopReasonRefusal {
		rm.IsError, rm.Result = true, reason
	}
	rawUsage, err := agent.JSONField(result, "usage")
	if err != nil {
		return nil, err
	}
	var usage acp.PromptUsage
	if rawUsage != nil {
		if err := json.Unmarshal(rawUsage, &usage); err != nil {
			return nil, fmt.Errorf("unmarshal prompt usage: %w", err)
		}
	}
	// ACP reports no applied cache duration, regardless of the selected provider.
	rm.Usage = agent.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadInputTokens: usage.CachedReadTokens, CacheCreationInputTokens: usage.CachedWriteTokens, ReasoningOutputTokens: usage.ThoughtTokens}
	return rm, nil
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

func (w *metadataState) normalizeMetadata(m *agent.SessionMetadata) error {
	if w.config.Models == nil || m.ReportedModel == "" {
		return nil
	}
	var err error
	m.ReportedModel, err = w.config.Models.Decode(m.ReportedModel)
	return err
}
