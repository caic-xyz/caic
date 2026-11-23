// Projects agy ACP tool calls onto caic's canonical tool inputs, outputs, and inferred skill reads.

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

// tools maps no native subagents: agy's ACP output reports only the spawn of an
// invoke_subagent child, never its lifecycle (see AGENTS.md).
type tools struct{}

// Parse canonicalizes the announced input and adds the output that agy
// reports only on completion.
func (tools) Parse(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
	if tool == nil {
		return messages, nil
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

// SettlePrompt adds nothing: agy reports each tool's end on its own call.
func (tools) SettlePrompt(acp.StopReason) []agent.Message { return nil }

func newTools() agentacp.ToolParser { return tools{} }

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
