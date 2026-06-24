// Claude Code stdio control requests for permission prompts.

package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// pendingControlAsk keeps the request metadata needed to answer a Claude Code
// AskUserQuestion control request on the next user prompt.
type pendingControlAsk struct {
	requestID string
	toolUseID string
	questions []agent.AskQuestion
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

// controlConn adapts Claude Code's stdio permission protocol into caic prompts.
//
// It auto-allows non-AskUserQuestion tool permission requests, records
// AskUserQuestion control requests, and sends the next user prompt as the
// corresponding control_response.
type controlConn struct {
	agent.Conn

	mu         sync.Mutex
	pendingAsk *pendingControlAsk
}

func (c *controlConn) SendPrompt(p agent.Prompt) error {
	c.mu.Lock()
	pending := c.pendingAsk
	if pending == nil {
		c.mu.Unlock()
		return c.Conn.SendPrompt(p)
	}
	if len(p.Images) > 0 {
		c.mu.Unlock()
		return errors.New("AskUserQuestion answers do not support images")
	}
	data, err := pending.answerResponse(p.Text)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := c.SendRaw(data); err != nil {
		c.mu.Unlock()
		return err
	}
	c.pendingAsk = nil
	c.mu.Unlock()
	return nil
}

func (c *controlConn) ReadMessages(r io.Reader, msgCh chan<- agent.Message) error {
	proxy := make(chan agent.Message, 1)
	errc := make(chan error, 1)
	go func() {
		defer close(errc)
		var controlErr error
		for m := range proxy {
			handled, err := c.handleControlMessage(m)
			if err != nil && controlErr == nil {
				controlErr = err
			}
			if controlErr != nil || handled {
				continue
			}
			msgCh <- m
		}
		if controlErr != nil {
			errc <- controlErr
		}
	}()
	err := c.Conn.ReadMessages(r, proxy)
	close(proxy)
	if controlErr := <-errc; controlErr != nil {
		return errors.Join(err, controlErr)
	}
	return err
}

func (c *controlConn) handleControlMessage(m agent.Message) (bool, error) {
	if pending, ok := m.(*agent.PendingUserActionMessage); ok {
		switch pending.Action.Kind {
		case agent.PendingUserActionAskUserQuestion:
			return false, c.setPendingAsk(pendingAskFromUserAction(pending.Action))
		default:
			return false, fmt.Errorf("unsupported pending user action kind %q", pending.Action.Kind)
		}
	}
	raw, ok := m.(*agent.RawMessage)
	if !ok {
		return false, nil
	}
	switch raw.MessageType {
	case string(claudecode.OutputControlRequest):
		drop, err := c.handleControlRequest(raw.Raw)
		return drop, err
	case string(claudecode.OutputControlCancelRequest):
		return true, c.handleControlCancelRequest(raw.Raw)
	default:
		return false, nil
	}
}

func (c *controlConn) handleControlRequest(raw []byte) (bool, error) {
	var msg claudecode.OutputControlRequestMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return true, fmt.Errorf("unmarshal control_request: %w", err)
	}
	can, err := msg.DecodeCanUseTool()
	if err != nil {
		return true, c.sendControlError(msg.RequestID, "invalid can_use_tool request")
	}
	if can.Subtype != claudecode.ControlCanUseTool {
		return true, c.sendControlError(msg.RequestID, "unsupported control request subtype: "+string(can.Subtype))
	}
	if can.ToolName != "AskUserQuestion" {
		updatedInput, err := rawObjectOrEmpty(can.Input)
		if err != nil {
			return true, fmt.Errorf("marshal %s input: %w", can.ToolName, err)
		}
		return true, c.sendAllowControlResponse(msg.RequestID, updatedInput)
	}
	inputRaw, err := rawObject(can.Input)
	if err != nil {
		return true, fmt.Errorf("marshal AskUserQuestion input: %w", err)
	}
	var input claudecode.AskUserQuestionInput
	if err := json.Unmarshal(inputRaw, &input); err != nil || len(input.Questions) == 0 {
		return true, c.sendControlError(msg.RequestID, "invalid AskUserQuestion input")
	}
	return false, c.setPendingAsk(pendingControlAsk{
		requestID: msg.RequestID,
		toolUseID: can.ToolUseID,
		questions: askQuestionsFromClaude(input.Questions),
	})
}

// restorePendingActions restores user-facing actions captured before reconnect.
// Claude Code currently supports only one AskUserQuestion action because it has
// a single prompt channel and no action selector.
func (c *controlConn) restorePendingActions(actions []agent.PendingUserAction) error {
	restoredAsk := false
	for _, action := range actions {
		switch action.Kind {
		case agent.PendingUserActionAskUserQuestion:
		default:
			return fmt.Errorf("unsupported pending user action kind %q", action.Kind)
		}
		if restoredAsk {
			return errors.New("multiple pending AskUserQuestion actions are not supported")
		}
		if err := c.setPendingAsk(pendingAskFromUserAction(action)); err != nil {
			return err
		}
		restoredAsk = true
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
		if c.pendingAsk.requestID == p.requestID && c.pendingAsk.toolUseID == p.toolUseID {
			return nil
		}
		return fmt.Errorf("AskUserQuestion control request %q already pending", c.pendingAsk.requestID)
	}
	c.pendingAsk = &p
	return nil
}

func (c *controlConn) sendAllowControlResponse(requestID string, updatedInput json.RawMessage) error {
	if len(updatedInput) == 0 {
		updatedInput = json.RawMessage(`{}`)
	}
	return c.sendControlResponse(&claudecode.ControlResponse{
		Subtype:   claudecode.ControlResponseSuccess,
		RequestID: requestID,
		Response: claudecode.ControlResponsePayload{
			Behavior:     claudecode.ControlCanUseToolBehaviorAllow,
			UpdatedInput: updatedInput,
		},
	})
}

func (c *controlConn) sendControlError(requestID, msg string) error {
	return c.sendControlResponse(&claudecode.ControlResponse{
		Subtype:   claudecode.ControlResponseError,
		RequestID: requestID,
		Error:     msg,
	})
}

func (c *controlConn) sendControlResponse(response *claudecode.ControlResponse) error {
	payload, err := marshalControlResponse(response)
	if err != nil {
		return err
	}
	return c.SendRaw(payload)
}

func rawObjectOrEmpty(m map[string]json.RawMessage) (json.RawMessage, error) {
	if m == nil {
		return json.RawMessage(`{}`), nil
	}
	return rawObject(m)
}

func askQuestionsToClaude(in []agent.AskQuestion) []claudecode.AskUserQuestion {
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

func pendingAskFromUserAction(action agent.PendingUserAction) pendingControlAsk {
	cp := agent.ClonePendingUserAction(action)
	return pendingControlAsk{
		requestID: cp.RequestID,
		toolUseID: cp.ToolUseID,
		questions: cp.Ask.Questions,
	}
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
