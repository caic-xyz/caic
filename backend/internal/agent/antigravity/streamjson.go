// Decodes agy stream-json records that tasks wrote before agy ran in ACP mode.

package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	antigravityv1 "github.com/caic-xyz/caic/backend/internal/agent/antigravity/data/v1"
	"github.com/maruel/genai/providers/antigravity"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// newStreamWire creates independent per-session stream-json parser state.
func newStreamWire() *streamWire {
	return &streamWire{steps: make(map[string]*step), textConversations: make(map[string]struct{}), accounting: newAccounting()}
}

// step retains lifecycle state only until the result. IDs include the native
// conversation ID because child conversations can use the same step index.
type step struct {
	started  bool
	text     strings.Builder
	overflow bool
}

// streamWire state belongs to the single goroutine that parses ordered input
// and output records.
type streamWire struct {
	accounting

	steps             map[string]*step
	textConversations map[string]struct{}
	// inTurn is set from a root user_input step until the root result.
	inTurn bool
	// queued counts prompts delivered during a root turn whose own turn has
	// not started. agy starts one turn for each after the result; AGENTS.md
	// holds the evidence.
	queued int
}

// ParseInput decodes client prompts and counts those delivered during a turn.
// Images are either a path text block with caic_image or, in older records,
// a text data URL.
func (w *streamWire) ParseInput(line []byte) ([]agent.Message, error) {
	var ev antigravityv1.InputMessage
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("antigravity input: %w", err)
	}
	switch antigravity.EventType(ev.Event) {
	case antigravity.EventUser:
		m := &agent.UserInputMessage{}
		for _, c := range ev.Message.Content {
			if c.Type != "text" {
				return nil, fmt.Errorf("antigravity: unsupported input content %q", c.Type)
			}
			if c.CaicImage != nil {
				m.Images = append(m.Images, v3.ImageData{MediaType: c.CaicImage.MediaType, Data: c.CaicImage.Data})
			} else if mediaType, payload, ok := agent.ParseImageDataURL(c.Text); ok {
				m.Images = append(m.Images, v3.ImageData{MediaType: mediaType, Data: payload})
			} else {
				m.Text += c.Text
			}
		}
		if w.inTurn {
			w.queued++
		}
		return []agent.Message{m}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: ev.Event, Raw: bytes.Clone(line)}}, nil
	}
}

// ParseOutput converts agy stdout into neutral messages.
// Unknown fields are accepted here; check-agent-logs owns strict DTO checks.
func (w *streamWire) ParseOutput(line []byte) ([]agent.Message, error) {
	var ev antigravity.StreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("antigravity event: %w", err)
	}
	switch ev.Event {
	case antigravity.EventInit:
		w.observeInit(ev.ConversationID, ev.Init.Model)
		return []agent.Message{&agent.InitMessage{Source: w.conversationSource(ev.ConversationID), SessionID: ev.ConversationID, Cwd: ev.Init.Cwd, Tools: ev.Init.Tools, ReportedModel: ev.Init.Model}}, nil
	case antigravity.EventStepUpdate:
		s := &ev.StepUpdate
		w.observeRelationships(s)
		source := w.conversationSource(s.ConversationID)
		index, err := agent.JSONField(line, "step_update", "step_index")
		if err != nil {
			return nil, err
		}
		if len(index) != 0 {
			source.ItemID = strconv.FormatInt(s.StepIndex, 10)
		}
		if w.foreignConversation(&source) {
			// A different conversation is not proven to be a task child. Retain
			// the record without applying root tool/usage effects or presenting it.
			return []agent.Message{&agent.RawMessage{Source: source, MessageType: string(ev.Event), Raw: bytes.Clone(line)}}, nil
		}
		usage, accepted := w.observeStep(s)
		if !accepted {
			return nil, nil
		}
		messages := append(w.parseStep(s, !source.IsChild()), usage...)
		for _, message := range messages {
			*message.Provenance() = source
		}
		return messages, nil
	case antigravity.EventResult:
		r := &ev.Result
		source := w.conversationSource(r.ConversationID)
		if w.foreignConversation(&source) {
			return []agent.Message{&agent.RawMessage{Source: source, MessageType: string(ev.Event), Raw: bytes.Clone(line)}}, nil
		}
		m := &agent.ResultMessage{Source: source, MessageType: "result", Subtype: string(r.Status), SessionID: r.ConversationID, Result: r.Response}
		if err := r.AsError(); err != nil {
			m.IsError = true
			m.Result = err.Error()
		}
		var messages []agent.Message
		if _, textSeen := w.textConversations[r.ConversationID]; !textSeen && r.Response != "" {
			messages = append(messages, &agent.TextMessage{Source: source, Text: r.Response})
		}
		delete(w.textConversations, r.ConversationID)
		for key := range w.steps {
			if strings.HasPrefix(key, r.ConversationID+":") {
				delete(w.steps, key)
			}
		}
		w.finishConversation(r.ConversationID)
		if source.IsChild() {
			// Declared child completion cannot finish the root turn or consume
			// its inclusive usage. No native lifecycle status is inferred; the
			// response text above already carries a successful outcome.
			if m.IsError {
				messages = append(messages, &agent.SystemMessage{Source: source, MessageType: "system", Subtype: agent.SystemSubtypeError, Detail: m.Result})
			}
			return messages, nil
		}
		// Result usage, duration, and turn count are conversation-wide, including
		// resumed processes. Only DONE steps from the task's proven conversations
		// contribute to the neutral per-turn usage.
		m.Usage = w.finishResult()
		m.TurnContinues = w.queued > 0
		w.inTurn = false
		return append(messages, m), nil
	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(ev.Event), Raw: bytes.Clone(line)}}, nil
	}
}

func (w *streamWire) parseStep(s *antigravity.StepUpdatePayload, root bool) []agent.Message {
	id := s.ConversationID + ":" + strconv.FormatInt(s.StepIndex, 10)
	st := w.steps[id]
	if st == nil {
		st = &step{}
		w.steps[id] = st
	}
	var msgs []agent.Message
	switch s.StepType {
	case antigravity.StepAgentResponse:
		if s.TextDelta != "" {
			w.textConversations[s.ConversationID] = struct{}{}
			msgs = append(msgs, &agent.TextDeltaMessage{Text: s.TextDelta})
			// Deltas remain authoritative if a response exceeds the bounded
			// finalization buffer. Do not retain an incomplete duplicate.
			if st.overflow || len(s.TextDelta) > (1<<20)-st.text.Len() {
				st.text.Reset()
				st.overflow = true
			} else {
				st.text.WriteString(s.TextDelta)
			}
		}
		if terminalStep(s.State) && !st.overflow && st.text.Len() != 0 {
			msgs = append(msgs, &agent.TextMessage{Text: st.text.String()})
			st.text.Reset()
		}
	case antigravity.StepTool:
		if !st.started {
			name := s.ToolName
			if name == "" {
				name = s.ToolInfo.Name
			}
			msgs = append(msgs, &agent.ToolUseMessage{ToolUseID: id, Name: name, Input: s.ToolInfo.Parameters})
			st.started = true
		}
		if terminalStep(s.State) {
			if s.ToolInfo.Output != "" {
				msgs = append(msgs, &agent.ToolOutputDeltaMessage{ToolUseID: id, Delta: s.ToolInfo.Output})
			}
			msgs = append(msgs, &agent.ToolResultMessage{ToolUseID: id, DurationMs: int64(s.DurationSeconds * 1000), Error: s.ToolInfo.Error.Message})
		}
	case antigravity.StepSystemMessage:
		if s.TextDelta != "" {
			msgs = append(msgs, &agent.SystemMessage{MessageType: "system", Subtype: agent.SystemSubtypeNotice, Detail: s.TextDelta})
		}
	case antigravity.StepUserInput:
		// agy opens every turn with this step, including one for a prompt that
		// arrived during the previous turn. The recorded stdin retains its text.
		if !st.started && root {
			msgs = append(msgs, &agent.TurnStartedMessage{})
			w.inTurn = true
			w.queued = max(0, w.queued-1)
		}
		st.started = true
	case antigravity.StepFinish, antigravity.StepErrorMessage:
		// finish and error_message precede the result, which carries the outcome.
	}
	return msgs
}

// terminalStep reports whether a step ended. agy 1.5.9 moves a failed tool step
// from ACTIVE to ERROR and never reports it DONE.
func terminalStep(state antigravity.StepState) bool {
	return state == antigravity.StepDone || state == antigravity.StepError
}

func convertUsage(u *antigravity.JSONUsage) agent.Usage {
	// agy reports uncached input and cached input separately. The recorded
	// cached-tools fixture has cache_read_tokens greater than input_tokens;
	// subtracting cache reads produces negative input. Output includes thinking.
	return agent.Usage{
		InputTokens:           int(u.InputTokens),
		CacheReadInputTokens:  int(u.CacheReadTokens),
		OutputTokens:          int(u.OutputTokens),
		ReasoningOutputTokens: int(u.ThinkingTokens),
	}
}
