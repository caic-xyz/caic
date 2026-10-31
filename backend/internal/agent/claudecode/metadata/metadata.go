// Package metadata projects Claude session metadata and normalizes ordered token accounting without retaining transcript content.

package metadata

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

// Reasoning accumulates root-turn thinking usage until its completed result.
// Live parsing and inventory use the same precedence and turn boundary.
type Reasoning struct {
	actual    int
	estimated int
}

// Apply observes one decoded record. Child accounting cannot alter the root
// turn. Explicit result usage wins over preceding actual usage, then estimates.
func (r *Reasoning) Apply(source *agent.MessageSource, estimate int, messages []agent.Message) {
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

func (r *Reasoning) observe(source *agent.MessageSource, estimate, actual int) {
	if !source.IsChild() {
		r.actual += actual
		r.estimated += max(0, estimate)
	}
}

// Parser projects ordered native metadata for inventory. It borrows usage
// fields and never decodes assistant content, tools or streamed thinking text.
type Parser struct {
	reasoning Reasoning
}

// Parse returns initialization and normalized completed-result messages.
// Other records only contribute their root-turn reasoning accounting.
func (p *Parser) Parse(line []byte) ([]agent.Message, error) {
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
			return nil, nil
		}
		usagePath = []string{"event", "usage"}
	case claudecode.OutputSystem:
		subtype, err := borrowedString(line, "subtype")
		if err != nil {
			return nil, err
		}
		if claudecode.SystemSubtype(subtype) == claudecode.SystemInit {
			msg, err := ParseInit(line)
			if err != nil {
				return nil, err
			}
			return []agent.Message{msg}, nil
		}
		if claudecode.SystemSubtype(subtype) != claudecode.SystemThinkingTokens {
			return nil, nil
		}
		estimate, err = ReasoningEstimate(line)
		if err != nil {
			return nil, err
		}
	case claudecode.OutputResult:
		msg, err := ParseResult(line)
		if err != nil {
			return nil, err
		}
		messages := []agent.Message{msg}
		p.reasoning.Apply(msg.Provenance(), 0, messages)
		return messages, nil
	default:
		return nil, nil
	}
	parent, err := agent.JSONString(line, "parent_tool_use_id")
	if err != nil {
		return nil, err
	}
	source := agent.MessageSource{DelegationToolUseID: parent}
	var actual int
	if len(usagePath) != 0 {
		raw, err := agent.JSONField(line, usagePath...)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, nil
		}
		actual, err = reasoningUsage(raw, claudecode.OutputType(typ) == claudecode.OutputAssistant)
		if err != nil {
			return nil, err
		}
	}
	p.reasoning.observe(&source, estimate, actual)
	return nil, nil
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

// reasoningUsage borrows only the scalar fields needed by the ordered fold.
// Inventory has no consumer for cache buckets or per-iteration usage.
func reasoningUsage(raw []byte, assistant bool) (int, error) {
	if string(raw) == "null" {
		return 0, nil
	}
	var counts [4]int64
	var thinking int64
	err := jsonparser.ObjectEach(raw, func(key, value []byte, kind jsonparser.ValueType, _ int) error {
		if kind == jsonparser.Null {
			return nil
		}
		var dst *int64
		switch string(key) {
		case "input_tokens":
			dst = &counts[0]
		case "output_tokens":
			dst = &counts[1]
		case "cache_creation_input_tokens":
			dst = &counts[2]
		case "cache_read_input_tokens":
			dst = &counts[3]
		case "output_tokens_details":
			if kind != jsonparser.Object {
				return fmt.Errorf("%s: expected object", key)
			}
			return jsonparser.ObjectEach(value, func(key, value []byte, kind jsonparser.ValueType, _ int) error {
				if string(key) != "thinking_tokens" || kind == jsonparser.Null {
					return nil
				}
				if kind != jsonparser.Number {
					return errors.New("thinking_tokens: expected integer")
				}
				var err error
				thinking, err = jsonparser.ParseInt(value)
				return err
			})
		default:
			return nil
		}
		if kind != jsonparser.Number {
			return fmt.Errorf("%s: expected integer", key)
		}
		n, err := jsonparser.ParseInt(value)
		*dst = n
		return err
	})
	// Assistant usage is published only when a token count is present.
	// Stream deltas can carry reasoning alone.
	if assistant && counts[0] <= 0 && counts[1] <= 0 && counts[2] <= 0 && counts[3] <= 0 {
		return 0, err
	}
	return int(thinking), err
}

// ReasoningEstimate decodes the incremental system/thinking_tokens estimate.
func ReasoningEstimate(line []byte) (int, error) {
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

// ParseInit decodes native session initialization, including its provenance.
func ParseInit(line []byte) (*agent.InitMessage, error) {
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

// ParseResult decodes a native completed result, including its provenance.
// The ordered Reasoning fold supplies usage omitted from this record.
func ParseResult(line []byte) (*agent.ResultMessage, error) {
	var w claudecode.OutputResultMsg
	if err := json.Unmarshal(line, &w); err != nil {
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
		Result:        w.Result,
		SessionID:     w.SessionID,
		TotalCostUSD:  w.TotalCostUSD,
		Usage:         Usage(&w.Usage),
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

// Usage converts the wire MsgUsage to the backend-neutral agent.Usage.
func Usage(u *claudecode.MsgUsage) agent.Usage {
	usage := agent.Usage{
		InputTokens:              int(u.InputTokens),
		OutputTokens:             int(u.OutputTokens),
		CacheCreationInputTokens: max(0, int(u.CacheCreationInputTokens)),
		CacheReadInputTokens:     int(u.CacheReadInputTokens),
		ReasoningOutputTokens:    int(u.OutputTokensDetails.ThinkingTokens),
	}
	usage.CacheWrites = tokens.NewCacheWrites(int64(usage.CacheCreationInputTokens), 0,
		tokens.CacheWrite{TTL: time.Hour, Tokens: int64(u.CacheCreation.Ephemeral1hInputTokens)},
		tokens.CacheWrite{TTL: 5 * time.Minute, Tokens: int64(u.CacheCreation.Ephemeral5mInputTokens)})

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
	if u.CacheCreation.Ephemeral5mInputTokens > 0 {
		usage.CacheTTLSeconds = 300
	} else if u.CacheCreation.Ephemeral1hInputTokens > 0 {
		usage.CacheTTLSeconds = 3600
	}
	return usage
}
