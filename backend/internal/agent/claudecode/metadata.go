// Projects payload-free Claude input/activity timing, session metadata, and ordered token accounting.

package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/tokens"
	"github.com/maruel/genai/providers/claudecode"
)

// reasoning accumulates root-turn thinking usage until its completed result.
// Live parsing and inventory use the same precedence and turn boundary.
type reasoning struct {
	actual    int
	estimated int
}

// Apply observes one decoded record. Child accounting cannot alter the root
// turn. Explicit result usage wins over preceding actual usage, then estimates.
func (r *reasoning) Apply(source *agent.MessageSource, estimate int, messages []agent.Message) {
	r.observe(source, estimate, 0)
	for _, message := range messages {
		if message.Provenance().IsChild() {
			continue
		}
		switch m := message.(type) {
		case *agent.UsageMessage:
			r.observe(m.Provenance(), 0, m.Usage.ReasoningOutputTokens)
		case *agent.ResultMessage:
			if m.Usage.ReasoningOutputTokens == 0 {
				m.Usage.ReasoningOutputTokens = r.actual
				if m.Usage.ReasoningOutputTokens == 0 {
					m.Usage.ReasoningOutputTokens = r.estimated
				}
			}
			r.actual, r.estimated = 0, 0
		}
	}
}

func (r *reasoning) observe(source *agent.MessageSource, estimate, actual int) {
	if !source.IsChild() {
		r.actual += actual
		r.estimated += max(0, estimate)
	}
}

// metadataParser projects ordered native metadata for inventory. It borrows usage
// fields, retaining only bounded native widget fragments until their block stops.
type metadataParser struct {
	metadataState

	widgets *WidgetTracker // Bounded partial widget input, released by native block stops.
}

// ParseOutput returns session reports, assistant model reports and normalized results.
// Other records only contribute their root-turn reasoning accounting.
func (p *metadataParser) ParseOutput(line []byte) ([]agent.Message, error) {
	typ, err := borrowedString(line, "type")
	if err != nil {
		return nil, err
	}
	var usagePath []string
	var estimate int
	switch claudecode.OutputType(typ) {
	case claudecode.OutputAssistant:
		usagePath = []string{"message", "usage"}
	case claudecode.OutputStreamEvent:
		typ, err := borrowedString(line, "event", "type")
		if err != nil {
			return nil, err
		}
		if string(typ) != "message_delta" {
			if string(typ) == "content_block_start" {
				name, err := agent.JSONString(line, "event", "content_block", "name")
				if err != nil {
					return nil, err
				}
				if _, widget := agent.WidgetToolNames[name]; widget && p.widgets == nil {
					p.widgets = newWidgetTracker()
				}
			}
			if string(typ) != "content_block_delta" {
				if p.widgets != nil {
					_, err := parseStreamEvent(line, p.widgets)
					if len(p.widgets.activeWidgets) == 0 {
						p.widgets = nil
					}
					return nil, err
				}
				return nil, nil
			}
			kind, err := borrowedString(line, "event", "delta", "type")
			if err != nil {
				return nil, err
			}
			var field string
			switch string(kind) {
			case "text_delta":
				field = "text"
			case "thinking_delta":
				field = "thinking"
			case "input_json_delta":
				if p.widgets == nil {
					return nil, nil
				}
				messages, err := parseStreamEvent(line, p.widgets)
				if err != nil || len(messages) == 0 {
					return nil, err
				}
				source, err := parseSource(line)
				if err != nil {
					return nil, err
				}
				if source.ThreadID == "" {
					source.ThreadID = p.sessionID
				}
				return []agent.Message{&agent.TextDeltaMessage{Source: source}}, nil
			default:
				return nil, nil
			}
			content, err := borrowedString(line, "event", "delta", field)
			if err != nil || len(content) == 0 {
				return nil, err
			}
			source, err := parseSource(line)
			if err != nil {
				return nil, err
			}
			if source.ThreadID == "" {
				source.ThreadID = p.sessionID
			}
			return []agent.Message{&agent.TextDeltaMessage{Source: source}}, nil
		}
		usagePath = []string{"event", "usage"}
	case claudecode.OutputSystem:
		subtype, err := borrowedString(line, "subtype")
		if err != nil {
			return nil, err
		}
		if claudecode.SystemSubtype(subtype) == claudecode.SystemInit {
			msg, err := parseInit(line)
			if err != nil {
				return nil, err
			}
			if !msg.Source.IsChild() {
				p.widgets = nil
			}
			messages := []agent.Message{msg}
			p.apply(msg.Provenance(), 0, messages, line)
			return messages, nil
		}
		if claudecode.SystemSubtype(subtype) != claudecode.SystemThinkingTokens {
			return nil, nil
		}
		estimate, err = reasoningEstimate(line)
		if err != nil {
			return nil, err
		}
	case claudecode.OutputResult:
		msg, err := parseResult(line)
		if err != nil {
			return nil, err
		}
		if !msg.Source.IsChild() {
			p.widgets = nil
		}
		messages := []agent.Message{msg}
		p.apply(msg.Provenance(), 0, messages, line)
		return messages, nil
	default:
		return nil, nil
	}
	source, err := parseSource(line)
	if err != nil {
		return nil, err
	}
	if source.ThreadID == "" {
		source.ThreadID = p.sessionID
	}
	var messages []agent.Message
	if claudecode.OutputType(typ) == claudecode.OutputAssistant {
		activity, err := assistantActivity(line)
		if err != nil {
			return nil, err
		}
		if activity {
			messages = append(messages, &agent.TextDeltaMessage{Source: source})
		}
	}
	if len(usagePath) != 0 {
		raw, err := agent.JSONField(line, usagePath...)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return messages, nil
		}
		var usage claudecode.MsgUsage
		if err := json.Unmarshal(raw, &usage); err != nil {
			return nil, err
		}
		model := ""
		if claudecode.OutputType(typ) == claudecode.OutputAssistant {
			model, err = agent.JSONString(line, "message", "model")
			if err != nil {
				return nil, err
			}
		}
		var message *agent.UsageMessage
		if claudecode.OutputType(typ) == claudecode.OutputAssistant {
			message = assistantUsage(&usage, model)
		} else if normalized := normalizeUsage(&usage); hasUsage(normalized) {
			message = &agent.UsageMessage{Usage: normalized}
		}
		if message == nil {
			return messages, nil
		}
		message.Source = source
		messages = append(messages, message)
		p.apply(&source, estimate, messages, line)
		return messages, nil
	}
	p.reasoning.observe(&source, estimate, 0)
	return nil, nil
}

// assistantActivity recognizes only blocks the full parser publishes as accepted
// output. Fields are borrowed; prompt, tool-input, and response payloads are not retained.
func assistantActivity(line []byte) (bool, error) {
	var activity bool
	var parseErr error
	_, err := jsonparser.ArrayEach(line, func(raw []byte, _ jsonparser.ValueType, _ int, err error) {
		if parseErr != nil {
			return
		}
		if err != nil {
			parseErr = err
			return
		}
		typ, err := borrowedString(raw, "type")
		if err != nil {
			parseErr = err
			return
		}
		var field string
		switch string(typ) {
		case "text":
			field = "text"
		case "thinking":
			field = "thinking"
		case "tool_use":
			name, err := borrowedString(raw, "name")
			if err != nil {
				parseErr = err
				return
			}
			if string(name) == "Skill" {
				skill, err := borrowedString(raw, "input", "skill")
				parseErr = err
				activity = activity || len(skill) > 0
			} else {
				activity = true
			}
			return
		default:
			return
		}
		content, err := borrowedString(raw, field)
		parseErr = err
		activity = activity || len(content) > 0
	}, "message", "content")
	if errors.Is(err, jsonparser.MalformedArrayError) {
		_, kind, _, fieldErr := jsonparser.Get(line, "message", "content")
		if fieldErr == nil && kind == jsonparser.Null {
			err = nil
		}
	}
	if errors.Is(err, jsonparser.KeyPathNotFoundError) {
		err = nil
	}
	return activity, errors.Join(err, parseErr)
}

// borrowedString dispatches native discriminators without copying the common
// unescaped values. Escaped values still follow the native string contract.
func borrowedString(line []byte, keys ...string) ([]byte, error) {
	value, kind, _, err := jsonparser.Get(line, keys...)
	if errors.Is(err, jsonparser.KeyPathNotFoundError) || kind == jsonparser.Null {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != jsonparser.String {
		return nil, fmt.Errorf("JSON field is %v, want string", kind)
	}
	return jsonparser.Unescape(value, nil)
}

// reasoningEstimate decodes the incremental system/thinking_tokens estimate.
func reasoningEstimate(line []byte) (int, error) {
	raw, err := agent.JSONField(line, "estimated_tokens_delta")
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return 0, err
	}
	delta, err := jsonparser.ParseInt(raw)
	if err != nil {
		return 0, err
	}
	return int(max(0, delta)), nil
}

// parseInit decodes native session initialization, including its provenance.
func parseInit(line []byte) (*agent.InitMessage, error) {
	var w claudecode.OutputInitMsg
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, err
	}
	source, err := parseSource(line)
	if err != nil {
		return nil, err
	}
	return &agent.InitMessage{
		Source:         source,
		SessionID:      w.SessionID,
		Cwd:            w.Cwd,
		Tools:          w.Tools,
		ReportedModel:  w.Model,
		ReportedEffort: w.Effort,
		Version:        w.Version,
	}, nil
}

// parseResult decodes a native completed result, including its provenance.
// The ordered reasoning fold supplies usage omitted from this record.
func parseResult(line []byte) (*agent.ResultMessage, error) {
	var w claudecode.OutputResultMsg
	reported := false
	if err := jsonparser.ObjectEach(line, func(key, value []byte, kind jsonparser.ValueType, _ int) error {
		var dst any
		switch string(key) {
		case "type":
			dst = &w.Type
		case "subtype":
			dst = &w.Subtype
		case "is_error":
			dst = &w.IsError
		case "duration_ms":
			dst = &w.Duration
		case "duration_api_ms":
			dst = &w.DurationAPI
		case "num_turns":
			dst = &w.NumTurns
		case "session_id":
			dst = &w.SessionID
		case "total_cost_usd":
			reported = kind != jsonparser.Null
			dst = &w.TotalCostUSD
		case "usage":
			dst = &w.Usage
		case "uuid":
			dst = &w.UUID
		default:
			return nil
		}
		if kind == jsonparser.String {
			value = append(append([]byte{'"'}, value...), '"')
		}
		return json.Unmarshal(value, dst)
	}); err != nil {
		return nil, err
	}
	source, err := parseSource(line)
	if err != nil {
		return nil, err
	}
	return &agent.ResultMessage{
		Source:        source,
		MessageType:   string(w.Type),
		Subtype:       string(w.Subtype),
		IsError:       w.IsError,
		DurationMs:    w.Duration.AsDuration().Milliseconds(),
		DurationAPIMs: w.DurationAPI.AsDuration().Milliseconds(),
		NumTurns:      w.NumTurns,
		SessionID:     w.SessionID,
		TotalCostUSD:  w.TotalCostUSD,
		CostReported:  reported,
		Usage:         normalizeUsage(&w.Usage),
		UUID:          w.UUID,
	}, nil
}

func parseSource(line []byte) (source agent.MessageSource, err error) {
	source.ThreadID, err = agent.JSONString(line, "session_id")
	if err != nil {
		return source, fmt.Errorf("session_id: %w", err)
	}
	source.DelegationToolUseID, err = agent.JSONString(line, "parent_tool_use_id")
	if err != nil {
		return source, fmt.Errorf("parent_tool_use_id: %w", err)
	}
	return source, nil
}

// normalizeUsage converts the wire MsgUsage to the backend-neutral agent.Usage.
func normalizeUsage(u *claudecode.MsgUsage) agent.Usage {
	usage := agent.Usage{
		InputTokens:              int(u.InputTokens),
		OutputTokens:             int(u.OutputTokens),
		CacheCreationInputTokens: max(0, int(u.CacheCreationInputTokens)),
		CacheReadInputTokens:     int(u.CacheReadInputTokens),
		ReasoningOutputTokens:    int(u.OutputTokensDetails.ThinkingTokens),
	}
	// A stream_event message_delta omits the top-level breakdown and reports
	// it only per iteration; iterations partition the top-level totals.
	created := u.CacheCreation
	if created.IsZero() {
		for i := range u.Iterations {
			created.Ephemeral1hInputTokens += u.Iterations[i].CacheCreation.Ephemeral1hInputTokens
			created.Ephemeral5mInputTokens += u.Iterations[i].CacheCreation.Ephemeral5mInputTokens
		}
	}
	usage.CacheWrites = tokens.NewCacheWrites(int64(usage.CacheCreationInputTokens), 0,
		tokens.CacheWrite{TTL: time.Hour, Tokens: int64(created.Ephemeral1hInputTokens)},
		tokens.CacheWrite{TTL: 5 * time.Minute, Tokens: int64(created.Ephemeral5mInputTokens)})

	// Claude Code normally gives a subscription's main conversation a one-hour
	// cache. Named subagents have separate five-minute caches. A fork can read
	// the parent's prefix on its first request, but its writes use the auxiliary
	// five-minute bucket. API-key and cloud main conversations also default to
	// five minutes. Settings, environment variables, authentication, and
	// subscription overage can change those choices, so use the response's
	// duration buckets instead of inferring TTL from the agent role:
	// https://code.claude.com/docs/en/prompt-caching#subagents-and-the-cache
	// https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md
	// Anthropic defines the five-minute and one-hour response buckets here:
	// https://platform.claude.com/docs/en/build-with-claude/prompt-caching#1-hour-cache-duration
	// Wire schema: https://github.com/maruel/genai/blob/main/providers/claudecode/dto.go
	// A mixed response has both durations, so its first expiry is five minutes.
	if created.Ephemeral5mInputTokens > 0 {
		usage.CacheTTLSeconds = 300
	} else if created.Ephemeral1hInputTokens > 0 {
		usage.CacheTTLSeconds = 3600
	}
	return usage
}

// ParseInput preserves native prompt timing and ownership without decoding content.
func (p *metadataParser) ParseInput(line []byte) ([]agent.Message, error) {
	typ, source, err := parseRecordHeader(line)
	if err != nil {
		return nil, err
	}
	if claudecode.InputType(typ) != claudecode.InputUser {
		return nil, nil
	}
	if source.ThreadID == "" {
		source.ThreadID = p.sessionID
	}
	return []agent.Message{&agent.UserInputMessage{Source: source}}, nil
}

// metadataState owns model attribution and turn accounting in both projections.
type metadataState struct {
	reasoning     reasoning
	reportedModel string
	sessionID     string
	childModels   map[agent.ToolKey]string
}

func (s *metadataState) apply(source *agent.MessageSource, estimate int, messages []agent.Message, line []byte) {
	s.reasoning.Apply(source, estimate, messages)
	for _, message := range messages {
		switch m := message.(type) {
		case *agent.InitMessage:
			if !m.Source.IsChild() {
				if m.SessionID != "" {
					s.sessionID = m.SessionID
				}
				if m.ReportedModel != "" {
					s.reportedModel = m.ReportedModel
				}
			}
		case *agent.UsageMessage:
			switch {
			case m.Source.IsChild():
				key := agent.ToolKeyForSource(m.Provenance(), "")
				if m.ReportedModel != "" {
					s.childModels[key] = m.ReportedModel
				} else if model := s.childModels[key]; model != "" {
					m.ReportedModel, m.ModelDerived = model, true
				}
			case m.ReportedModel != "":
				s.reportedModel = m.ReportedModel
			case s.reportedModel != "":
				m.ReportedModel, m.ModelDerived = s.reportedModel, true
			}
		case *agent.ResultMessage:
			if !m.Source.IsChild() {
				// Native results are the first report of the active context window.
				m.ContextWindow = resultContextWindow(line, s.reportedModel)
			}
		}
	}
}

// assistantUsage publishes only the token-bearing native assistant reports.
// Reasoning-only deltas have a separate publication rule.
func assistantUsage(u *claudecode.MsgUsage, model string) *agent.UsageMessage {
	if u.InputTokens <= 0 && u.OutputTokens <= 0 && u.CacheCreationInputTokens <= 0 && u.CacheReadInputTokens <= 0 {
		return nil
	}
	return &agent.UsageMessage{Usage: normalizeUsage(u), ReportedModel: model}
}

func hasUsage(u agent.Usage) bool {
	return u.InputTokens > 0 ||
		u.OutputTokens > 0 ||
		u.CacheCreationInputTokens > 0 ||
		u.CacheReadInputTokens > 0 ||
		u.ReasoningOutputTokens > 0
}
