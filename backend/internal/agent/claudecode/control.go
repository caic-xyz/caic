// Claude Code stdio control requests: answer every permission request, route root questions to the next prompt, and deny subagent questions, including after reconnect.

package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// subagentAskDenial tells a subagent why its AskUserQuestion was rejected.
const subagentAskDenial = "caic cannot route a subagent's question to the user. Continue without asking, or return the question to the parent agent."

// pendingControlAsk keeps the request metadata needed to answer a Claude Code
// AskUserQuestion control request on the next user prompt.
type pendingControlAsk struct {
	requestID string
	toolUseID string
	questions []v3.AskQuestion
}

func pendingAskFromUserAction(action v3.PendingUserAction) pendingControlAsk {
	cp := agent.ClonePendingUserAction(action)
	return pendingControlAsk{
		requestID: cp.RequestID,
		toolUseID: cp.ToolUseID,
		questions: cp.Ask.Questions,
	}
}

func (p *pendingControlAsk) answerResponse(answer string) ([]byte, error) {
	answers := make(map[string]string, len(p.questions))
	for _, q := range p.questions {
		answers[q.Question] = answer
	}
	updatedInput, err := json.Marshal(claudecode.AskUserQuestionUpdatedInput{
		Questions: askQuestionsToClaude(p.questions),
		Answers:   answers,
	})
	if err != nil {
		return nil, err
	}
	return marshalControlResponse(&claudecode.ControlResponse{
		Subtype:   claudecode.ControlResponseSuccess,
		RequestID: p.requestID,
		Response: claudecode.ControlResponsePayload{
			Behavior:     claudecode.ControlCanUseToolBehaviorAllow,
			UpdatedInput: updatedInput,
		},
	})
}

func (p *pendingControlAsk) same(other pendingControlAsk) bool {
	return p.requestID == other.requestID &&
		p.toolUseID == other.toolUseID &&
		reflect.DeepEqual(p.questions, other.questions)
}

// controlConn adapts Claude Code's stdio permission protocol into caic prompts.
//
// It auto-allows non-AskUserQuestion tool permission requests of every owner,
// denies subagent AskUserQuestion requests, records the root AskUserQuestion
// request, and sends the next user prompt as the corresponding
// control_response. Permission requests stay in task history, paired with
// their recorded replies and delivery outcomes, so a reconnect can answer one
// whose reply was never delivered.
type controlConn struct {
	agent.Conn

	mu         sync.Mutex
	pendingAsk *pendingControlAsk
}

func (c *controlConn) SendPrompt(p agent.Prompt, data []byte) error {
	c.mu.Lock()
	pending := c.pendingAsk
	c.mu.Unlock()
	if pending == nil {
		return c.Conn.SendPrompt(p, data)
	}
	if len(p.Images) > 0 {
		return errors.New("AskUserQuestion answers do not support images")
	}
	data, err := pending.answerResponse(p.Text)
	if err != nil {
		return err
	}
	if err := c.Conn.SendPrompt(p, data); err != nil {
		return err
	}
	c.mu.Lock()
	if c.pendingAsk == pending {
		c.pendingAsk = nil
	}
	c.mu.Unlock()
	return nil
}

// HandleMessage intercepts controls without writing stdin.
//
// Claude Code blocks on every can_use_tool request until it reads the reply,
// whichever agent owns the tool call, so permission replies ignore ownership.
// Ownership only routes AskUserQuestion: the single prompt channel answers the
// root agent, and a subagent's question is denied.
func (c *controlConn) HandleMessage(m agent.Message) (response []byte, handled bool, err error) {
	switch m := m.(type) {
	case *agent.PendingUserActionMessage:
		if m.Action.Kind != agent.PendingUserActionAskUserQuestion {
			return nil, false, fmt.Errorf("unsupported pending user action kind %q", m.Action.Kind)
		}
		if m.Source.IsChild() {
			response, err = denyControlResponse(m.Action.RequestID, subagentAskDenial)
			return response, false, err
		}
		return nil, false, c.setPendingAsk(pendingAskFromUserAction(m.Action))
	case *agent.RawMessage:
		switch m.MessageType {
		case string(claudecode.OutputControlRequest):
			return c.handleControlRequest(m.Raw)
		case string(claudecode.OutputControlCancelRequest):
			return nil, true, c.handleControlCancelRequest(m.Raw)
		}
	}
	return nil, false, nil
}

// handleControlRequest answers every request except a valid AskUserQuestion.
// The request stays published so task history pairs it with its reply.
func (c *controlConn) handleControlRequest(raw []byte) (response []byte, handled bool, err error) {
	var msg claudecode.OutputControlRequestMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, true, fmt.Errorf("unmarshal control_request: %w", err)
	}
	can, err := msg.DecodeCanUseTool()
	if err != nil {
		response, err = controlError(msg.RequestID, "invalid can_use_tool request")
		return response, false, err
	}
	if can.Subtype != claudecode.ControlCanUseTool {
		response, err = controlError(msg.RequestID, "unsupported control request subtype: "+string(can.Subtype))
		return response, false, err
	}
	if can.ToolName != "AskUserQuestion" {
		updatedInput, err := rawObjectOrEmpty(can.Input)
		if err != nil {
			return nil, true, fmt.Errorf("marshal %s input: %w", can.ToolName, err)
		}
		response, err = allowControlResponse(msg.RequestID, updatedInput)
		return response, false, err
	}
	inputRaw, err := rawObject(can.Input)
	if err != nil {
		return nil, true, fmt.Errorf("marshal AskUserQuestion input: %w", err)
	}
	var input claudecode.AskUserQuestionInput
	if err := json.Unmarshal(inputRaw, &input); err != nil || len(input.Questions) == 0 {
		response, err = controlError(msg.RequestID, "invalid AskUserQuestion input")
		return response, false, err
	}
	// Valid actionable Asks arrive as normalized PendingUserActionMessage.
	// A valid Ask retained as raw has unresolved ownership; preserve it for
	// inspection without registering a response on the root prompt channel.
	return nil, false, nil
}

// restorePendingActions restores user-facing actions captured before reconnect.
// Claude Code currently supports only one AskUserQuestion action because it has
// a single prompt channel and no action selector.
func (c *controlConn) restorePendingActions(actions []v3.PendingUserAction) error {
	var restoredAsk pendingControlAsk
	hasRestoredAsk := false
	for _, action := range actions {
		switch action.Kind {
		case agent.PendingUserActionAskUserQuestion:
		default:
			return fmt.Errorf("unsupported pending user action kind %q", action.Kind)
		}
		ask := pendingAskFromUserAction(action)
		if hasRestoredAsk {
			if restoredAsk.same(ask) {
				continue
			}
			return errors.New("multiple pending AskUserQuestion actions are not supported")
		}
		if err := c.setPendingAsk(ask); err != nil {
			return err
		}
		restoredAsk = ask
		hasRestoredAsk = true
	}
	return nil
}

func (c *controlConn) handleControlCancelRequest(raw []byte) error {
	var msg claudecode.OutputControlCancelRequestMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("unmarshal control_cancel_request: %w", err)
	}
	c.mu.Lock()
	if c.pendingAsk != nil && c.pendingAsk.requestID == msg.RequestID {
		c.pendingAsk = nil
	}
	c.mu.Unlock()
	return nil
}

func (c *controlConn) setPendingAsk(p pendingControlAsk) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingAsk != nil {
		if c.pendingAsk.same(p) {
			return nil
		}
		return fmt.Errorf("AskUserQuestion control request %q already pending", c.pendingAsk.requestID)
	}
	c.pendingAsk = &p
	return nil
}

// unansweredReplies returns the automatic replies owed for control requests in
// msgs that have neither a delivered reply nor a cancellation. A restarted caic
// imports such requests from the relay snapshot without answering them, and a
// transport failure can lose a recorded reply; Claude Code then waits forever.
// Root questions are excluded: they await the next prompt through
// restorePendingActions.
//
// A reply recorded directly after a delivery start counts only once that
// delivery completed, so a reply whose write failed or was never confirmed is
// sent again. A reply without a delivery start counts as delivered.
func (c *controlConn) unansweredReplies(msgs []agent.Message) ([][]byte, error) {
	settled := map[string]struct{}{}
	attempts := map[string]string{} // delivery UUID to request ID
	started := ""
	for _, m := range msgs {
		start := started
		started = ""
		if s, ok := m.(*agent.SystemMessage); ok {
			switch s.Subtype {
			case agent.SystemSubtypeInputDeliveryStarted:
				started = s.UUID
			case agent.SystemSubtypeInputDeliveryCompleted:
				if id, ok := attempts[s.UUID]; ok {
					settled[id] = struct{}{}
				}
			}
			continue
		}
		raw, ok := m.(*agent.RawMessage)
		if !ok {
			continue
		}
		switch raw.MessageType {
		case string(claudecode.InputControlResponse):
			var msg claudecode.InputControlResponseMsg
			if err := json.Unmarshal(raw.Raw, &msg); err != nil {
				return nil, fmt.Errorf("unmarshal control_response: %w", err)
			}
			if start == "" {
				settled[msg.Response.RequestID] = struct{}{}
			} else {
				attempts[start] = msg.Response.RequestID
			}
		case string(claudecode.OutputControlCancelRequest):
			var msg claudecode.OutputControlCancelRequestMsg
			if err := json.Unmarshal(raw.Raw, &msg); err != nil {
				return nil, fmt.Errorf("unmarshal control_cancel_request: %w", err)
			}
			settled[msg.RequestID] = struct{}{}
		}
	}
	var replies [][]byte
	for _, m := range msgs {
		var requestID string
		switch m := m.(type) {
		case *agent.PendingUserActionMessage:
			if !m.Source.IsChild() {
				continue
			}
			requestID = m.Action.RequestID
		case *agent.RawMessage:
			if m.MessageType != string(claudecode.OutputControlRequest) {
				continue
			}
			var msg claudecode.OutputControlRequestMsg
			if err := json.Unmarshal(m.Raw, &msg); err != nil {
				return nil, fmt.Errorf("unmarshal control_request: %w", err)
			}
			requestID = msg.RequestID
		default:
			continue
		}
		if _, ok := settled[requestID]; ok {
			continue
		}
		response, _, err := c.HandleMessage(m)
		if err != nil {
			return nil, err
		}
		if len(response) > 0 {
			replies = append(replies, response)
		}
	}
	return replies, nil
}

func allowControlResponse(requestID string, updatedInput json.RawMessage) ([]byte, error) {
	if len(updatedInput) == 0 {
		updatedInput = json.RawMessage(`{}`)
	}
	return marshalControlResponse(&claudecode.ControlResponse{Subtype: claudecode.ControlResponseSuccess, RequestID: requestID, Response: claudecode.ControlResponsePayload{Behavior: claudecode.ControlCanUseToolBehaviorAllow, UpdatedInput: updatedInput}})
}

// denyControlResponse rejects a tool call. A denial requires msg, which
// explains the rejection to the model.
func denyControlResponse(requestID, msg string) ([]byte, error) {
	return marshalControlResponse(&claudecode.ControlResponse{Subtype: claudecode.ControlResponseSuccess, RequestID: requestID, Response: claudecode.ControlResponsePayload{Behavior: claudecode.ControlCanUseToolBehaviorDeny, Message: msg}})
}

func controlError(requestID, msg string) ([]byte, error) {
	return marshalControlResponse(&claudecode.ControlResponse{Subtype: claudecode.ControlResponseError, RequestID: requestID, Error: msg})
}

func rawObjectOrEmpty(m map[string]json.RawMessage) (json.RawMessage, error) {
	if m == nil {
		return json.RawMessage(`{}`), nil
	}
	return rawObject(m)
}

func askQuestionsToClaude(in []v3.AskQuestion) []claudecode.AskUserQuestion {
	out := make([]claudecode.AskUserQuestion, len(in))
	for i := range in {
		out[i] = claudecode.AskUserQuestion{
			Question:    in[i].Question,
			Header:      in[i].Header,
			MultiSelect: in[i].MultiSelect,
			Options:     make([]claudecode.AskUserQuestionOption, len(in[i].Options)),
		}
		for j := range in[i].Options {
			out[i].Options[j] = claudecode.AskUserQuestionOption{
				Label:       in[i].Options[j].Label,
				Description: in[i].Options[j].Description,
			}
		}
	}
	return out
}

func marshalControlResponse(response *claudecode.ControlResponse) ([]byte, error) {
	payload, err := json.Marshal(claudecode.InputControlResponseMsg{
		Type:     claudecode.InputControlResponse,
		Response: *response,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal control_response: %w", err)
	}
	payload = append(payload, '\n')
	return payload, nil
}
