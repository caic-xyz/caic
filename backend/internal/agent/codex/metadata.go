// Reconstructs payload-free Codex input/activity timing, session metadata, and accounting.

package codex

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/codex"
)

type metadataStringField struct {
	key string
	dst *string
}
type metadataField struct {
	key string
	dst any
}

type metadataParser struct {
	wire *wireFormat
}

func (p *metadataParser) ParseInput(line []byte) ([]agent.Message, error) {
	probe, err := agent.ParseJSONRPCEnvelope(line)
	if err != nil {
		return nil, err
	}
	if err := p.wire.rememberRequest(&probe); err != nil {
		return nil, err
	}
	if probe.Method != "turn/start" {
		return nil, nil
	}
	threadID, err := agent.JSONString(probe.Params, "threadId")
	if err != nil {
		return nil, err
	}
	input := &agent.UserInputMessage{Source: agent.MessageSource{ThreadID: threadID}}
	p.wire.scopeSource(input.Provenance())
	return []agent.Message{input}, nil
}

func (p *metadataParser) ParseOutput(line []byte) ([]agent.Message, error) {
	return p.wire.parseOutput(line, parseMetadataMessage)
}

type recordedRequest struct {
	id       string
	method   string
	threadID string
}

// recordedInputTurn binds a committed start request to its native echo identity.
type recordedInputTurn struct {
	threadID string
	turnID   string
	itemID   string
}

// threadResponseMetadata is shared by the live handshake and both history
// projections; saved turns in resume responses are never needed here.
func threadResponseMetadata(raw []byte) (codex.ThreadStartResponse, error) {
	var r codex.ThreadStartResponse
	var err error
	if r.Thread, err = threadMetadata(raw, "thread"); err != nil {
		return r, err
	}
	if r.Model, err = agent.JSONString(raw, "model"); err != nil {
		return r, err
	}
	effort, err := agent.JSONString(raw, "reasoningEffort")
	if err != nil {
		return r, err
	}
	if effort != "" {
		e := codex.ReasoningEffort(effort)
		r.ReasoningEffort = &e
	}
	return r, nil
}

// threadMetadata deliberately borrows individual scalar fields. Thread.Turns
// can contain the whole saved transcript in a resume reply.
func threadMetadata(raw []byte, key string) (codex.Thread, error) {
	var t codex.Thread
	for _, field := range []metadataStringField{
		{"id", &t.ID}, {"cwd", &t.CWD}, {"model", &t.Model},
		{"cliVersion", &t.CLIVersion}, {"parentThreadId", &t.ParentThreadID},
	} {
		v, err := agent.JSONString(raw, key, field.key)
		if err != nil {
			return t, err
		}
		*field.dst = v
	}
	v, err := agent.JSONString(raw, key, "reasoningEffort")
	if err != nil {
		return t, err
	}
	t.ReasoningEffort = codex.ReasoningEffort(v)
	v, err = agent.JSONString(raw, key, "threadSource")
	if err != nil {
		return t, err
	}
	t.ThreadSource = codex.ThreadSource(v)
	return t, nil
}

func turnMetadata(raw []byte, p *codex.TurnCompletedNotification) error {
	var err error
	if p.ThreadID, err = agent.JSONString(raw, "threadId"); err != nil {
		return err
	}
	if p.Turn.ID, err = agent.JSONString(raw, "turn", "id"); err != nil {
		return err
	}
	status, err := agent.JSONString(raw, "turn", "status")
	if err != nil {
		return err
	}
	p.Turn.Status = codex.TurnStatus(status)
	for _, field := range []metadataField{
		{"durationMs", &p.Turn.Duration}, {"error", &p.Turn.Error},
	} {
		value, err := agent.JSONField(raw, "turn", field.key)
		if err != nil {
			return err
		}
		if len(value) != 0 {
			if err := json.Unmarshal(value, field.dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseMetadataMessage(line []byte, probe *agent.JSONRPCEnvelope) ([]agent.Message, decodedItem, error) {
	if probe.ID != nil && string(probe.ID) != "null" {
		return nil, decodedItem{}, nil
	}
	switch codex.Method(probe.Method) {
	case codex.MethodItemDelta, codex.MethodReasoningSummaryTextDelta, codex.MethodCommandOutputDelta, codex.MethodMcpToolCallProgress:
		threadID, err := agent.JSONString(probe.Params, "threadId")
		if err != nil {
			return nil, decodedItem{}, err
		}
		turnID, err := agent.JSONString(probe.Params, "turnId")
		if err != nil {
			return nil, decodedItem{}, err
		}
		return []agent.Message{&agent.TextDeltaMessage{Source: agent.MessageSource{ThreadID: threadID, TurnID: turnID}}}, decodedItem{}, nil
	case codex.MethodThreadStarted, codex.MethodTurnStarted, codex.MethodTurnCompleted,
		codex.MethodModelRerouted, codex.MethodErrorNotification, codex.MethodThreadStatusChanged:
		return parseMessage(line, probe)
	case codex.MethodItemStarted, codex.MethodItemCompleted:
		raw, typ, err := decodeItem(codex.Method(probe.Method), probe.Params)
		if err != nil {
			return nil, decodedItem{}, err
		}
		r := decodedItem{typ: typ}
		r.source.ThreadID, err = agent.JSONString(probe.Params, "threadId")
		if err != nil {
			return nil, r, err
		}
		r.source.TurnID, err = agent.JSONString(probe.Params, "turnId")
		if err != nil {
			return nil, r, err
		}
		switch typ {
		case codex.ItemTypeSubAgentActivity:
			var a codex.SubAgentActivityItem
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, r, err
			}
			r.activity = &a
		case codex.ItemTypeCollabAgentToolCall:
			var c codex.CollabAgentToolCallItem
			tool, err := agent.JSONString(raw, "tool")
			if err != nil {
				return nil, r, err
			}
			c.Tool = codex.CollabAgentTool(tool)
			receivers, err := agent.JSONField(raw, "receiverThreadIds")
			if err != nil {
				return nil, r, err
			}
			if len(receivers) != 0 {
				if err := json.Unmarshal(receivers, &c.ReceiverThreadIDs); err != nil {
					return nil, r, err
				}
			}
			r.collaboration = &c
		case codex.ItemTypeCommandExecution, codex.ItemTypeFileChange, codex.ItemTypeMCPToolCall, codex.ItemTypeDynamicToolCall, codex.ItemTypeImageGeneration:
			return []agent.Message{&agent.TextDeltaMessage{Source: r.source}}, r, nil
		case codex.ItemTypeAgentMessage, codex.ItemTypeReasoning, codex.ItemTypePlan, codex.ItemTypeWebSearch:
			if codex.Method(probe.Method) == codex.MethodItemCompleted {
				return []agent.Message{&agent.TextDeltaMessage{Source: r.source}}, r, nil
			}
		case codex.ItemTypeUserMessage:
			if codex.Method(probe.Method) == codex.MethodItemStarted {
				return []agent.Message{&agent.UserInputMessage{Source: r.source}}, r, nil
			}
		case codex.ItemTypeContextCompaction:
			if codex.Method(probe.Method) == codex.MethodItemCompleted {
				return []agent.Message{&agent.SystemMessage{Source: r.source, MessageType: "system", Subtype: agent.SystemSubtypeCompactBoundary}}, r, nil
			}
		default:
			return nil, decodedItem{}, nil
		}
		if typ == codex.ItemTypeCollabAgentToolCall {
			return []agent.Message{&agent.TextDeltaMessage{Source: r.source}}, r, nil
		}
		return nil, r, nil
	default:
		return nil, decodedItem{}, nil
	}
}
