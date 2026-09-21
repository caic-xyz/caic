// Correlates Claude Agent invocations with native task updates and folds detached shell commands, keeping shell tasks out of the subagent card set.

package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/maruel/genai/providers/claudecode"

	"regexp"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// decodedLine carries the typed records of one wire line that the canonical
// conversion drops. A task record must stay out of the transcript, and a tool
// result is reduced to the fields a message needs, so the parser hands the
// decoded records over rather than making the adapter unmarshal the line again:
// a second decode of every user and task record would cost more than the
// correlation itself.
type decodedLine struct {
	system *claudecode.OutputSystemMsg // Set for a task lifecycle record.
	user   *claudecode.OutputUserMsg   // Set for a decoded user record.
}

// nativeSubagents folds Claude's subagent evidence into canonical lifecycles.
//
// The canonical identity is the native task ID, which every task record carries.
// Keying by the parent's tool use ID instead would split one agent into two
// cards whenever a session resumes from history the wire has not seen: the
// restored card would be keyed by the tool use seen at spawn time, and a later
// task record keyed by the task ID would look like a different agent.
// Its correlation maps are allocated with the wire, created once per session,
// instead of by the first record that uses them.
type nativeSubagents struct {
	timeline   agent.NativeSubagentTimeline
	tasks      map[string]agent.NativeSubagent // canonical task ID → card
	byTool     map[string]string               // tool use ID → canonical task ID
	tools      map[string]agent.NativeSubagent // tool use ID → delegation metadata
	background map[string]bool                 // tool use ID → launched in background
}

// parse folds one decoded line: a delegation tool use records the metadata that
// the task records and tool results settle.
func (n *nativeSubagents) parse(record decodedLine, messages []agent.Message) ([]agent.Message, error) {
	if err := n.parseToolUses(messages); err != nil {
		return nil, err
	}
	switch {
	case record.system != nil:
		return n.parseTask(record.system)
	case record.user != nil:
		return n.parseToolResult(record.user)
	default:
		return nil, nil
	}
}

// parseToolUses remembers the delegation metadata of an Agent invocation. The
// tool use alone is not lifecycle evidence: the task records carry the native
// identity and the reported state, so remember what they do not repeat instead
// of emitting a card here.
func (n *nativeSubagents) parseToolUses(messages []agent.Message) error {
	for _, message := range messages {
		use, ok := message.(*agent.ToolUseMessage)
		if !ok || (use.Name != "Agent" && use.Name != "Task") || use.ToolUseID == "" {
			continue
		}
		var input struct {
			Description     string `json:"description"`
			Prompt          string `json:"prompt"`
			SubagentType    string `json:"subagent_type"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if err := json.Unmarshal(use.Input, &input); err != nil {
			return err
		}
		if input.Prompt == "" {
			continue
		}
		s := agent.NativeSubagent{ToolUseID: use.ToolUseID, Label: input.Description, Prompt: input.Prompt, Status: agent.NativeSubagentStatusUnknown, Background: input.RunInBackground}
		if s.Label == "" {
			s.Label = input.SubagentType
		}
		n.tools[use.ToolUseID] = s
		n.background[use.ToolUseID] = input.RunInBackground
	}
	return nil
}

// parseTask folds one task lifecycle record. Only local_agent tasks are model
// subagents; local_bash and the other task types must never become native agents.
func (n *nativeSubagents) parseTask(ev *claudecode.OutputSystemMsg) ([]agent.Message, error) {
	if ev.TaskType == "local_bash" || ev.TaskID == "" {
		return nil, nil
	}
	canonicalID := "claude:task:" + ev.TaskID
	s, known := n.tasks[canonicalID]
	if !known {
		if ev.TaskType != "local_agent" {
			// A task type the protocol does not identify as a subagent, seen
			// without a prior local_agent task. Do not invent one.
			return nil, nil
		}
		s = agent.NativeSubagent{ID: canonicalID}
	}
	if ev.ToolUseID != "" {
		s.ToolUseID = ev.ToolUseID
		n.byTool[ev.ToolUseID] = canonicalID
	}
	if tool, ok := n.tools[s.ToolUseID]; ok {
		if s.Label == "" {
			s.Label = tool.Label
		}
		if s.Prompt == "" {
			s.Prompt = tool.Prompt
		}
		s.Background = s.Background || tool.Background
	}
	if s.Label == "" {
		s.Label = ev.Description
	}
	if s.Prompt == "" && len(ev.Prompt) > 0 {
		_ = json.Unmarshal(ev.Prompt, &s.Prompt)
	}
	s.Status = claudeSubagentStatus(ev.Status)
	if ev.Subtype == claudecode.SystemTaskStarted {
		s.Status = agent.NativeSubagentStatusRunning
	}
	if ev.Patch.Status != "" {
		s.Status = claudeSubagentStatus(string(ev.Patch.Status))
	}
	// A background task stays background for its whole lifecycle; the task
	// record repeats the fact and is authoritative when the tool use was not
	// retained (for example after resuming from history).
	if ev.IsBackgrounded || ev.Patch.IsBackgrounded {
		s.Background = true
	}
	s.Result = ev.Summary
	n.tasks[canonicalID] = s
	return n.timeline.Observe(&s), nil
}

// parseToolResult settles the card whose delegation this result belongs to.
func (n *nativeSubagents) parseToolResult(ev *claudecode.OutputUserMsg) ([]agent.Message, error) {
	// Child stream messages have parent_tool_use_id; they are not the parent's
	// Agent tool result and must not settle its lifecycle.
	if ev.ParentToolUseID != "" {
		return nil, nil
	}
	var body claudecode.OutputUserBlock
	if len(ev.Message) == 0 || ev.Message[0] != '{' {
		return nil, nil
	}
	if err := json.Unmarshal(ev.Message, &body); err != nil {
		return nil, err
	}
	var out []agent.Message
	for i := range body.Content {
		b := &body.Content[i]
		if b.Type != "tool_result" {
			continue
		}
		canonicalID, known := n.byTool[b.ToolUseID]
		if !known {
			continue
		}
		s := n.tasks[canonicalID]
		var result struct {
			Status string `json:"status"`
		}
		if len(ev.ToolUseResult) > 0 && ev.ToolUseResult[0] == '{' {
			if err := json.Unmarshal(ev.ToolUseResult, &result); err != nil {
				return nil, err
			}
		}
		switch {
		case b.IsError:
			s.Status = agent.NativeSubagentStatusFailed
		case n.background[b.ToolUseID] || result.Status == "async_launched":
			continue
		default:
			s.Status = agent.NativeSubagentStatusCompleted
		}
		var text []string
		blocks := b.Content.TextBlocks()
		for i := range blocks {
			if blocks[i].Text != "" {
				text = append(text, blocks[i].Text)
			}
		}
		s.Result = strings.Join(text, "\n")
		n.tasks[canonicalID] = s
		out = append(out, n.timeline.Observe(&s)...)
	}
	return out, nil
}

// claudeSubagentStatus maps the protocol's reported task status onto the
// canonical lifecycle. "pending" is queued rather than executing, "paused" is a
// resumable non-terminal state, and "killed" is an interruption; anything
// unrecognized stays unknown instead of being inferred.
func claudeSubagentStatus(status string) agent.NativeSubagentStatus {
	switch status {
	case "running", "in_progress":
		return agent.NativeSubagentStatusRunning
	case "paused":
		return agent.NativeSubagentStatusPaused
	case "completed":
		return agent.NativeSubagentStatusCompleted
	case "failed":
		return agent.NativeSubagentStatusFailed
	case "killed", "stopped", "interrupted":
		return agent.NativeSubagentStatusInterrupted
	default:
		return agent.NativeSubagentStatusUnknown
	}
}

// claudeShellIdentity is the canonical card identity for a Claude background
// shell command. It is distinct from the native-subagent adapter's
// "claude:task:" prefix so the two card sets cannot collide in logs even
// though the wire addresses both by native task ID.
func claudeShellIdentity(taskID string) string {
	return "claude:shell:" + taskID
}

// exitCodePattern extracts the exit status Claude Code folds into a task
// notification's summary prose ("Background command ... completed (exit code 0)").
var exitCodePattern = regexp.MustCompile(`\(exit code (\d+)\)`)

// backgroundCommands folds Claude's detached shell evidence into canonical
// lifecycles. It shares the nativeSubagents adapter's decoded records but keeps
// its own card set: a background bash command is not a subagent, and folding it
// through the subagent timeline would feed the task state machine that only
// detached delegations may drive.
// Its card set is allocated with the wire, created once per session, instead of
// by the first record that stores a card.
type backgroundCommands struct {
	timeline agent.BackgroundCommandTimeline
	known    map[string]bool // canonical task ID → the wire has proven this command
}

// parse folds one decoded task record. Only a backgrounded task_started proves
// a command: foreground shell tasks settle inside their own tool call and must
// never become cards, and an update or notification for a task the wire has not
// proven cannot invent one.
func (b *backgroundCommands) parse(record decodedLine) []agent.Message {
	ev := record.system
	if ev == nil {
		return nil
	}
	switch ev.Subtype {
	case claudecode.SystemTaskStarted:
		return b.parseStarted(ev)
	case claudecode.SystemTaskUpdated:
		return b.parseUpdated(ev)
	case claudecode.SystemTaskNotification:
		return b.parseNotification(ev)
	default:
		return nil
	}
}

// parseStarted proves a background command. A local_bash task started without
// is_backgrounded runs synchronously inside its tool result; only a detached
// one outlives that result and needs a card.
func (b *backgroundCommands) parseStarted(ev *claudecode.OutputSystemMsg) []agent.Message {
	if ev.TaskType != "local_bash" || !ev.IsBackgrounded || ev.TaskID == "" {
		return nil
	}
	id := claudeShellIdentity(ev.TaskID)
	b.known[id] = true
	s := agent.BackgroundCommand{ID: id, Label: ev.Description, Status: agent.BackgroundCommandStatusRunning, ToolUseID: ev.ToolUseID}
	return b.timeline.Observe(&s)
}

// parseUpdated folds a task_updated status patch into an already-proven
// command. Claude reports patches without repeating the task type, so the
// patch is trusted only for cards the wire has already seen.
func (b *backgroundCommands) parseUpdated(ev *claudecode.OutputSystemMsg) []agent.Message {
	if ev.TaskID == "" {
		return nil
	}
	id := claudeShellIdentity(ev.TaskID)
	if !b.known[id] {
		return nil
	}
	s := agent.BackgroundCommand{ID: id}
	if ev.Patch.Status != "" {
		s.Status = claudeBackgroundCommandStatus(string(ev.Patch.Status))
	}
	if ev.Patch.IsBackgrounded {
		b.known[id] = true
	}
	return b.timeline.Observe(&s)
}

// parseNotification folds a task_notification into an already-proven command.
// The notification is the authoritative terminal report: its summary carries
// the outcome prose and its output_file points at the captured output.
func (b *backgroundCommands) parseNotification(ev *claudecode.OutputSystemMsg) []agent.Message {
	if ev.TaskID == "" {
		return nil
	}
	id := claudeShellIdentity(ev.TaskID)
	if !b.known[id] {
		return nil
	}
	s := agent.BackgroundCommand{ID: id, Status: claudeBackgroundCommandStatus(ev.Status), Result: ev.Summary, OutputRef: ev.OutputFile}
	if m := exitCodePattern.FindStringSubmatch(ev.Summary); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil {
			s.ExitCode = &code
		}
	}
	return b.timeline.Observe(&s)
}

// claudeBackgroundCommandStatus maps the protocol's reported task status onto
// the canonical lifecycle. "pending" is a queued command that has not started,
// which the canonical vocabulary does not distinguish from running; "killed"
// and its synonyms are interruptions. Anything unrecognized keeps the card's
// existing state instead of inventing one.
func claudeBackgroundCommandStatus(status string) agent.BackgroundCommandStatus {
	switch status {
	case "running", "in_progress", "pending":
		return agent.BackgroundCommandStatusRunning
	case "completed":
		return agent.BackgroundCommandStatusCompleted
	case "failed", "error":
		return agent.BackgroundCommandStatusFailed
	case "killed", "stopped", "interrupted":
		return agent.BackgroundCommandStatusInterrupted
	default:
		return ""
	}
}
