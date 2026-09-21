// Recognizes OpenCode task delegation only from ACP task identity and structured delegation input.

package opencode

import (
	"encoding/json"
	"strings"

	"github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// Its correlation maps are allocated with the wire, created once per session,
// instead of by the first tool call that needs them.
type nativeSubagents struct {
	timeline  agent.NativeSubagentTimeline
	tasks     map[string]agent.NativeSubagent
	taskTools map[string]struct{}
}

// parse folds one decoded session update. A tool call announcement or update is
// the only update that can prove or settle a delegation, so toolCall is nil for
// every other update; the canonical conversion already decoded the one it hands
// over.
func (n *nativeSubagents) parse(toolCall *opencode.ToolCallUpdateUpdate) ([]agent.Message, error) {
	if toolCall == nil || toolCall.ToolCallID == "" {
		return nil, nil
	}
	// v1.18.31 acp/tool.ts preserves the raw task title on initial events,
	// followed by the task schema. A normalized name such as Agent is not proof.
	if toolCall.Title == "task" && toolCall.Kind == opencode.KindThink {
		n.taskTools[toolCall.ToolCallID] = struct{}{}
	}
	_, knownTask := n.taskTools[toolCall.ToolCallID]
	s, known := n.tasks[toolCall.ToolCallID]
	if !known && !knownTask {
		return nil, nil
	}
	if !known {
		if _, task := n.taskTools[toolCall.ToolCallID]; !task {
			return nil, nil
		}
		var input struct {
			Prompt       string `json:"prompt"`
			Description  string `json:"description"`
			SubagentType string `json:"subagent_type"`
		}
		if len(toolCall.RawInput) == 0 {
			return nil, nil
		}
		if err := json.Unmarshal(toolCall.RawInput, &input); err != nil {
			return nil, err
		}
		if input.Prompt == "" || input.SubagentType == "" {
			return nil, nil
		}
		s = agent.NativeSubagent{ID: "opencode:tool:" + toolCall.ToolCallID, ToolUseID: toolCall.ToolCallID, Label: input.Description, Prompt: input.Prompt}
		n.tasks[toolCall.ToolCallID] = s
	}
	s.Status = agent.NativeSubagentStatusUnknown
	switch toolCall.Status {
	case opencode.StatusPending:
		// Queued does not establish active execution.
	case opencode.StatusInProgress:
		s.Status = agent.NativeSubagentStatusRunning
	case opencode.StatusCompleted:
		s.Status = agent.NativeSubagentStatusCompleted
		s.Result = taskResultText(extractToolOutputDelta(toolCall))
	case opencode.StatusFailed:
		s.Status = agent.NativeSubagentStatusFailed
		s.Result = extractToolError(toolCall)
	}
	return n.timeline.Observe(&s), nil
}

// taskResultText extracts the child's report from the OpenCode task tool output,
// which wraps it in a session envelope:
//
//	<task id="ses_…" state="completed">
//	<task_result>
//	…
//	</task_result>
//	</task>
//
// An unrecognized shape is returned unchanged so the card still shows whatever
// the harness reported rather than silently dropping it.
func taskResultText(output string) string {
	const openTag, closeTag = "<task_result>", "</task_result>"
	_, after, ok := strings.Cut(output, openTag)
	if !ok {
		return output
	}
	body, _, ok := strings.Cut(after, closeTag)
	if !ok {
		return output
	}
	return strings.TrimSpace(body)
}
