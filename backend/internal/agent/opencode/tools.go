// Projects OpenCode native delegation and inferred skill reads through shared ACP.

package opencode

import (
	"encoding/json"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	"github.com/maruel/genai/acp"
)

type tools struct{ native nativeSubagents }

func (p *tools) Parse(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
	if tool == nil {
		return messages, nil
	}
	if tool.Status == acp.StatusInProgress {
		for i, message := range messages {
			if use, ok := message.(*agent.ToolUseMessage); ok {
				messages = slices.Insert(messages, i+1, inferredSkillReads(use.Name, tool.RawInput, tool.ToolCallID)...)
				break
			}
		}
	}
	native, err := p.native.parse(tool)
	return append(messages, native...), err
}

func newTools() agentacp.ToolParser {
	return &tools{native: nativeSubagents{tasks: make(map[string]agent.NativeSubagent), taskTools: make(map[string]struct{})}}
}

// inferredSkillReads reports the skills an OpenCode tool call opens as files.
// OpenCode has no Skill tool; it scans ~/.claude, ~/.agents, and its own
// config directories, then the model opens the file it wants.
func inferredSkillReads(name string, input json.RawMessage, sourceID string) []agent.Message {
	switch name {
	case "Read":
		var in struct {
			FilePath string `json:"filePath"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil
		}
		return agent.InferredSkillReadFromPath(in.FilePath, sourceID)
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil
		}
		return agent.InferredSkillReadsFromCommand(in.Command, "", sourceID)
	}
	return nil
}
