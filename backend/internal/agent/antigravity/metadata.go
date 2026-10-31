// Projects Antigravity conversation metadata and deduplicated DONE-step accounting without transcript payloads.

package antigravity

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/providers/antigravity"
)

type accounting struct {
	usage              antigravity.JSONUsage
	model              string
	parents            map[string]string
	rootConversationID string
	done               map[string]struct{}
}

func newAccounting() accounting {
	return accounting{parents: make(map[string]string), done: make(map[string]struct{})}
}

func (s *accounting) observeInit(id, model string) {
	s.rootConversationID = id
	if model != "" {
		s.model = model
	}
}

func (s *accounting) observeRelationships(step *antigravity.StepUpdatePayload) {
	for _, child := range step.SubagentInfo.Subagents {
		if step.ConversationID != "" && child.ConversationID != "" && child.ConversationID != step.ConversationID {
			s.parents[child.ConversationID] = step.ConversationID
		}
	}
}

func (s *accounting) observeStep(step *antigravity.StepUpdatePayload) ([]agent.Message, bool) {
	source := s.conversationSource(step.ConversationID)
	if s.foreignConversation(&source) {
		return nil, false
	}
	key := step.ConversationID + ":" + strconv.FormatInt(step.StepIndex, 10)
	if _, found := s.done[key]; found {
		return nil, false
	}
	if step.State != antigravity.StepDone {
		return nil, true
	}
	s.done[key] = struct{}{}
	if step.Usage == (antigravity.JSONUsage{}) {
		return nil, true
	}
	s.usage.Add(&step.Usage)
	source.ItemID = strconv.FormatInt(step.StepIndex, 10)
	return []agent.Message{&agent.UsageMessage{Source: source, Usage: convertUsage(&step.Usage), ReportedModel: s.model, ModelDerived: true}}, true
}

func (s *accounting) finishConversation(id string) {
	for key := range s.done {
		if strings.HasPrefix(key, id+":") {
			delete(s.done, key)
		}
	}
}

func (s *accounting) finishResult() agent.Usage {
	u := convertUsage(&s.usage)
	s.usage = antigravity.JSONUsage{}
	return u
}

// conversationSource retains native identity. Only explicit delegation chains
// reaching the initialized root establish child ownership; mismatches do not.
func (w *accounting) conversationSource(id string) agent.MessageSource {
	s := agent.MessageSource{ThreadID: id}
	parent := id
	for range len(w.parents) + 1 {
		if parent != "" && parent == w.rootConversationID {
			s.RootThreadID = parent
			break
		}
		var found bool
		parent, found = w.parents[parent]
		if !found {
			break
		}
	}
	return s
}

func (w *accounting) foreignConversation(source *agent.MessageSource) bool {
	return w.rootConversationID != "" && source.ThreadID != "" && source.ThreadID != w.rootConversationID && !source.IsChild()
}

type metadataField struct {
	key string
	dst any
}

type metadataParser struct{ accounting }

func (*metadataParser) ParseInput([]byte) ([]agent.Message, error) { return nil, nil }

func (p *metadataParser) ParseOutput(line []byte) ([]agent.Message, error) {
	event, err := agent.JSONString(line, "event")
	if err != nil {
		return nil, err
	}
	switch antigravity.EventType(event) {
	case antigravity.EventInit:
		id, err := agent.JSONString(line, "conversation_id")
		if err != nil {
			return nil, err
		}
		model, err := agent.JSONString(line, "init", "model")
		if err != nil {
			return nil, err
		}
		p.observeInit(id, model)
		return []agent.Message{&agent.InitMessage{Source: p.conversationSource(id), SessionID: id, ReportedModel: model}}, nil
	case antigravity.EventStepUpdate:
		var step antigravity.StepUpdatePayload
		if step.ConversationID, err = agent.JSONString(line, "step_update", "conversation_id"); err != nil {
			return nil, err
		}
		state, err := agent.JSONString(line, "step_update", "state")
		if err != nil {
			return nil, err
		}
		step.State = antigravity.StepState(state)
		for _, field := range []metadataField{
			{"step_index", &step.StepIndex}, {"usage", &step.Usage},
		} {
			raw, err := agent.JSONField(line, "step_update", field.key)
			if err != nil {
				return nil, err
			}
			if len(raw) != 0 {
				if err := json.Unmarshal(raw, field.dst); err != nil {
					return nil, err
				}
			}
		}
		children, err := agent.JSONField(line, "step_update", "subagent_info", "subagents")
		if err != nil {
			return nil, err
		}
		if len(children) != 0 && string(children) != "null" {
			var childErr error
			_, err = jsonparser.ArrayEach(children, func(value []byte, _ jsonparser.ValueType, _ int, parseErr error) {
				if childErr != nil {
					return
				}
				if parseErr != nil {
					childErr = parseErr
					return
				}
				id, e := agent.JSONString(value, "conversation_id")
				if e != nil {
					childErr = e
					return
				}
				step.SubagentInfo.Subagents = append(step.SubagentInfo.Subagents, antigravity.SubagentSpecInfo{ConversationID: id})
			})
			if err != nil {
				return nil, err
			}
			if childErr != nil {
				return nil, childErr
			}
		}
		p.observeRelationships(&step)
		messages, _ := p.observeStep(&step)
		return messages, nil
	case antigravity.EventResult:
		id, err := agent.JSONString(line, "result", "conversation_id")
		if err != nil {
			return nil, err
		}
		source := p.conversationSource(id)
		if p.foreignConversation(&source) {
			return nil, nil
		}
		p.finishConversation(id)
		if source.IsChild() {
			return nil, nil
		}
		status, err := agent.JSONString(line, "result", "status")
		if err != nil {
			return nil, err
		}
		r := antigravity.JSONOutput{ConversationID: id, Status: antigravity.ResultStatus(status)}
		if r.Error, err = agent.JSONString(line, "result", "error"); err != nil {
			return nil, err
		}
		return []agent.Message{&agent.ResultMessage{Source: source, SessionID: id, MessageType: "result", Subtype: status, IsError: r.AsError() != nil, Usage: p.finishResult()}}, nil
	default:
		return nil, nil
	}
}
