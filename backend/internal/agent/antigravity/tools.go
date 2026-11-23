// Projects agy ACP tool calls onto caic's canonical tool inputs, outputs, inferred skill reads, and native subagent cards.

package antigravity

import (
	"encoding/json"
	"slices"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
)

// inputKeys maps agy 1.5.9 tool argument names to the input keys shared ACP
// presentation reads, as in acp.EditInput. read_file and edit_file name the
// file Path; run_command names its shell command CommandLine; edit_file
// replaces Target with Replacement.
var inputKeys = map[string]string{
	"CommandLine": "command",
	"Path":        "filePath",
	"Replacement": "newString",
	"Target":      "oldString",
}

// invokeSubagent is agy's delegation tool. Its call completes once agy has
// spawned the asynchronous child; see AGENTS.md.
const invokeSubagent = "invoke_subagent"

// tools projects agy tool calls and its asynchronous delegations.
type tools struct {
	// running holds the delegation cards spawned during the current prompt.
	running []agent.NativeSubagent
}

// Parse canonicalizes the announced input and adds the output that agy
// reports only on completion. An invoke_subagent call becomes a native card.
func (t *tools) Parse(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
	if tool == nil {
		return messages, nil
	}
	if tool.Name == invokeSubagent {
		return t.delegate(tool, messages), nil
	}
	for i, m := range messages {
		use, ok := m.(*agent.ToolUseMessage)
		if !ok || use.ToolUseID != tool.ToolCallID {
			continue
		}
		input, err := canonicalInput(use.Input)
		if err != nil {
			return nil, err
		}
		use.Input = input
		agentacp.AddEditInputView(use)
		messages = slices.Insert(messages, i+1, inferredSkillReads(use.Name, input, tool.ToolCallID)...)
		break
	}
	if tool.Status == acp.StatusCompleted {
		if output := agentacp.ToolOutput(tool); output != "" {
			// The output precedes the result, which follows the ToolUseMessage
			// when the announcement itself completes the call.
			i := slices.IndexFunc(messages, func(m agent.Message) bool {
				r, ok := m.(*agent.ToolResultMessage)
				return ok && r.ToolUseID == tool.ToolCallID
			})
			messages = slices.Insert(messages, max(i, 0), agent.Message(&agent.ToolOutputDeltaMessage{ToolUseID: tool.ToolCallID, Delta: output}))
		}
	}
	return messages, nil
}

// SettlePrompt ends the cards spawned during the prompt. agy keeps each
// child's lifecycle outside ACP, so the prompt response is the only boundary:
// end_turn completes them, cancelled interrupts them, and any other end
// retires them with an unknown outcome. A parent can end its turn before a
// child reports, so a card can complete before its child does; see AGENTS.md.
func (t *tools) SettlePrompt(stop acp.StopReason) []agent.Message {
	messages := make([]agent.Message, 0, len(t.running))
	for i := range t.running {
		s := &t.running[i]
		switch stop {
		case acp.StopReasonEndTurn:
			s.Status = agent.NativeSubagentStatusCompleted
		case acp.StopReasonCancelled:
			s.Status = agent.NativeSubagentStatusInterrupted
		default:
			messages = append(messages, &agent.BackgroundRetiredMessage{ID: s.ID})
			continue
		}
		messages = append(messages, &agent.NativeSubagentMessage{Subagent: *s})
	}
	t.running = t.running[:0]
	return messages
}

// delegate replaces the invoke_subagent tool messages with a native card. The
// first update announces the child; a completed update only acknowledges the
// spawn, so the card keeps running until the prompt settles it.
func (t *tools) delegate(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) []agent.Message {
	messages = agentacp.DropToolMessages(messages, tool.ToolCallID)
	i := slices.IndexFunc(t.running, func(s agent.NativeSubagent) bool { return s.ToolUseID == tool.ToolCallID })
	var s agent.NativeSubagent
	switch {
	case i >= 0 && tool.Status != acp.StatusFailed:
		return messages
	case i >= 0:
		s = t.running[i]
		t.running = slices.Delete(t.running, i, i+1)
	default:
		var in struct {
			Prompt   string `json:"Prompt"`
			Role     string `json:"Role"`
			TypeName string `json:"TypeName"`
		}
		// A malformed input still identifies the delegation by its call.
		_ = json.Unmarshal(tool.RawInput, &in)
		s = agent.NativeSubagent{
			ToolUseID: tool.ToolCallID,
			Scope:     agent.NativeSubagentScopeAgent,
			ID:        "antigravity:tool:" + tool.ToolCallID,
			Label:     in.Role,
			Kind:      in.TypeName,
			Prompt:    in.Prompt,
			Status:    agent.NativeSubagentStatusRunning,
		}
		if tool.Status != acp.StatusFailed {
			t.running = append(t.running, s)
		}
	}
	if tool.Status == acp.StatusFailed {
		s.Status = agent.NativeSubagentStatusFailed
	}
	return append(messages, &agent.NativeSubagentMessage{Subagent: s})
}

func newTools() agentacp.ToolParser { return &tools{} }

// canonicalInput renames agy argument keys; other keys and values stay.
func canonicalInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		// A non-object input has no keys to rename.
		return raw, nil //nolint:nilerr // Keep the native input unchanged.
	}
	renamed := false
	for from, to := range inputKeys {
		if v, ok := in[from]; ok {
			if _, taken := in[to]; !taken {
				in[to] = v
				delete(in, from)
				renamed = true
			}
		}
	}
	if !renamed {
		return raw, nil
	}
	return json.Marshal(in)
}

// inferredSkillReads reports the skill a read_file or run_command call opens.
func inferredSkillReads(name string, input json.RawMessage, sourceID string) []agent.Message {
	switch name {
	case "Read":
		p, err := agent.JSONString(input, "filePath")
		if err != nil {
			return nil
		}
		return agent.InferredSkillReadFromPath(p, sourceID)
	case "Bash":
		cmd, err := agent.JSONString(input, "command")
		if err != nil {
			return nil
		}
		return agent.InferredSkillReadsFromCommand(cmd, "", sourceID)
	}
	return nil
}
