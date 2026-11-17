// Claude background-tool drift: flags backgrounded local_bash tasks that the caic adapter cannot classify by spawning tool.

package main

import (
	"encoding/json"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	claudedto "github.com/maruel/genai/providers/claudecode"
)

// claudeToolKey names a tool use within one Claude session.
type claudeToolKey struct {
	sessionID string
	toolUseID string
}

// claudeBackgroundTools resolves each backgrounded local_bash task to the
// tool use that spawned it. The adapter classifies a watch only by a Monitor
// tool use and anything else as a command, so an unknown or unresolved
// spawning tool silently changes whether a task looks busy.
type claudeBackgroundTools struct {
	names map[claudeToolKey]string // session/tool use ID → tool name
}

// observe returns the drift a Claude output record proves. It decodes leniently:
// the strict DTO check reports schema drift on the same record.
func (c *claudeBackgroundTools) observe(message []byte) error {
	var probe claudedto.OutputTypeProbe
	// A relay diagnostic or malformed record fails the probe; the strict check
	// reports it.
	isProbe := json.Unmarshal(message, &probe) == nil
	switch {
	case isProbe && probe.Type == claudedto.OutputAssistant:
		var msg claudedto.OutputAssistantMsg
		if json.Unmarshal(message, &msg) == nil {
			for i := range msg.Message.Content {
				if b := &msg.Message.Content[i]; b.Type == "tool_use" && b.ID != "" {
					c.names[claudeToolKey{sessionID: msg.SessionID, toolUseID: b.ID}] = b.Name
				}
			}
		}
	case isProbe && probe.Type == claudedto.OutputSystem && claudedto.SystemSubtype(probe.Subtype) == claudedto.SystemTaskStarted:
		var msg claudedto.OutputSystemMsg
		if json.Unmarshal(message, &msg) == nil && msg.TaskType == "local_bash" && msg.IsBackgrounded {
			return c.checkSpawner(msg.SessionID, msg.ToolUseID)
		}
	}
	return nil
}

// checkSpawner reports a background task whose spawning tool the adapter
// cannot classify.
func (c *claudeBackgroundTools) checkSpawner(sessionID, toolUseID string) error {
	name, ok := c.names[claudeToolKey{sessionID: sessionID, toolUseID: toolUseID}]
	if !ok {
		// Every such task in the local corpus resolves; a miss means the
		// correlation changed and a watch would count as a command.
		return fmt.Errorf("backgrounded local_bash task_started has no tool_use with its tool_use_id in session; caic cannot tell a %s watch from a %s command", claudecode.ToolMonitor, claudecode.ToolBash)
	}
	if name != claudecode.ToolBash && name != claudecode.ToolMonitor {
		return fmt.Errorf("backgrounded local_bash task spawned by unknown tool %q; classify it as a command or a watch", name)
	}
	return nil
}
