// Projects agy ACP tool calls onto canonical tools, skill reads, subagent cards, background commands, and wake turns.

package antigravity

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
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

// runCommand is agy's shell tool. A call still running after its
// WaitMsBeforeAsync continues in the background; see AGENTS.md.
const runCommand = "run_command"

// exitCode matches the exit status agy reports in a run_command output.
var exitCode = regexp.MustCompile(`Command exited with code (-?\d+)\.`)

type commandCall struct {
	agent.BackgroundCommand

	sessionID string
}

func (c *commandCall) message() *agent.BackgroundCommandMessage {
	return &agent.BackgroundCommandMessage{Source: v3.MessageSource{ThreadID: c.sessionID}, Command: c.BackgroundCommand}
}

// tools projects agy tool calls, its asynchronous delegations and commands,
// and the steps agy runs without a prompt.
type tools struct {
	// running holds the delegation cards spawned during the current prompt.
	running []agent.NativeSubagent
	// commands holds the run_command calls of the current prompt that agy has
	// announced and not settled.
	commands []commandCall
	// detached holds the commands that outlived their prompt.
	detached []commandCall
	// woken reports a turn that agy started without a prompt; see wake.
	woken bool
	// stepText reports that the woken turn's current model call streamed text.
	stepText bool
}

// Parse canonicalizes the announced input and adds the output that agy
// reports only on completion. An invoke_subagent call becomes a native card,
// and the end of a detached run_command settles its background command.
// Outside a prompt, it brackets agy's own steps as a turn.
func (t *tools) Parse(sid string, prompted bool, tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
	if prompted {
		// The prompt's response ends its turn, including one agy started.
		t.woken, t.stepText = false, false
	}
	detached := -1
	if tool != nil {
		detached = slices.IndexFunc(t.detached, func(c commandCall) bool { return c.sessionID == sid && c.ToolUseID == tool.ToolCallID })
	}
	switch {
	case tool == nil:
	case tool.Name == invokeSubagent:
		messages = t.delegate(tool, messages)
	case detached >= 0:
		wakes := t.detached[detached].WakesAgent
		messages = t.settleDetached(detached, tool, messages)
		if !wakes {
			return messages, nil
		}
	default:
		var err error
		if messages, err = t.project(sid, tool, messages); err != nil {
			return nil, err
		}
	}
	if !prompted {
		messages = t.wake(messages)
	}
	return messages, nil
}

// SettlePrompt ends the cards spawned during the prompt and detaches its
// running commands. agy keeps each child's lifecycle outside ACP, so the prompt
// response is the only boundary: end_turn completes them, cancelled interrupts
// them, and any other end retires them with an unknown outcome. A parent can
// end its turn before a child reports, so a card can complete before its child
// does; see AGENTS.md. A command still running at end_turn continues in the
// background, and agy reports its end later. After another end, agy's record
// of the command is unknown, so caic keeps no card for it.
func (t *tools) SettlePrompt(sid string, stop acp.StopReason, r *agent.ResultMessage) []agent.Message {
	t.woken, t.stepText = false, false
	r.Source.ThreadID = sid
	messages := make([]agent.Message, 0, len(t.running)+len(t.commands))
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
	t.commands = slices.DeleteFunc(t.commands, func(c commandCall) bool {
		if c.sessionID != sid {
			return false
		}
		if stop == acp.StopReasonEndTurn {
			t.detached = append(t.detached, c)
			messages = append(messages, c.message())
			r.PendingToolUseIDs = append(r.PendingToolUseIDs, c.ToolUseID)
		}
		return true
	})
	return messages
}

// project canonicalizes a generic tool call and tracks a running run_command.
func (t *tools) project(sid string, tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
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
	t.trackCommand(sid, tool)
	for _, m := range messages {
		m.Provenance().ThreadID = sid
	}
	return messages, nil
}

// trackCommand records a running run_command call until agy settles it.
func (t *tools) trackCommand(sid string, tool *acp.ToolCallUpdateUpdate) {
	if sid == "" || tool.ToolCallID == "" {
		return
	}
	i := slices.IndexFunc(t.commands, func(c commandCall) bool { return c.sessionID == sid && c.ToolUseID == tool.ToolCallID })
	if i < 0 && tool.Name != runCommand {
		return
	}
	switch {
	case tool.Status == acp.StatusCompleted || tool.Status == acp.StatusFailed:
		if i >= 0 {
			t.commands = slices.Delete(t.commands, i, i+1)
		}
	case i < 0 && tool.Status == acp.StatusInProgress:
		var in struct {
			CommandLine string `json:"CommandLine"`
			IsDaemon    bool   `json:"IsDaemon"`
		}
		// A malformed input still identifies the command by its call.
		_ = json.Unmarshal(tool.RawInput, &in)
		t.commands = append(t.commands, commandCall{sessionID: sid,
			ID:        "antigravity:command:" + sid + "/" + tool.ToolCallID,
			Label:     in.CommandLine,
			Status:    agent.BackgroundCommandStatusRunning,
			ToolUseID: tool.ToolCallID,
			// agy notifies the model when a command ends; a daemon is not
			// expected to end, so the agent waits for the user meanwhile.
			WakesAgent: !in.IsDaemon})
	}
}

// settleDetached reports the outcome and preserves the tool's captured output
// for the transcript row in the prompt that launched it.
func (t *tools) settleDetached(i int, tool *acp.ToolCallUpdateUpdate, messages []agent.Message) []agent.Message {
	var status agent.BackgroundCommandStatus
	switch tool.Status {
	case acp.StatusCompleted:
		status = agent.BackgroundCommandStatusCompleted
	case acp.StatusFailed:
		status = agent.BackgroundCommandStatusFailed
	default:
		return agentacp.DropToolMessages(messages, tool.ToolCallID)
	}
	call := t.detached[i]
	c := commandCall{sessionID: call.sessionID, ID: call.ID, Status: status}
	t.detached = slices.Delete(t.detached, i, i+1)
	output := agentacp.ToolOutput(tool)
	if raw, err := agent.JSONField(tool.RawOutput, "exit_code"); err == nil && len(raw) > 0 {
		// Keep the terminal status even if the optional exit code is malformed.
		var code *int
		if json.Unmarshal(raw, &code) == nil {
			c.ExitCode = code
		}
	}
	if c.ExitCode == nil {
		if m := exitCode.FindStringSubmatch(output); m != nil {
			if code, err := strconv.Atoi(m[1]); err == nil {
				c.ExitCode = &code
			}
		}
	}
	if c.ExitCode != nil && *c.ExitCode != 0 {
		c.Status = agent.BackgroundCommandStatusFailed
	}
	if output != "" {
		messages = slices.Insert(messages, 0, agent.Message(&agent.ToolOutputDeltaMessage{ToolUseID: tool.ToolCallID, Delta: output}))
	}
	for _, m := range messages {
		m.Provenance().ThreadID = call.sessionID
	}
	return append(messages, c.message())
}

// wake brackets the steps agy runs without a prompt, such as after a detached
// command ends or a child reports, as a turn. The turn starts before its first
// activity. Each model call ends with a usage_update, which tool calls follow
// when the model requested any; the turn ends at the usage_update of a call
// that streamed text. See AGENTS.md.
func (t *tools) wake(messages []agent.Message) []agent.Message {
	for i := 0; i < len(messages); i++ {
		switch messages[i].(type) {
		case *agent.TextDeltaMessage:
			t.stepText = true
		case *agent.UsageMessage:
			if t.woken && t.stepText {
				t.woken, t.stepText = false, false
				messages = slices.Insert(messages, i+1, agent.Message(&agent.TurnEndedMessage{}))
				i++
			}
			continue
		case *agent.ToolUseMessage, *agent.ToolResultMessage:
			t.stepText = false
		case *agent.ThinkingDeltaMessage, *agent.ToolOutputDeltaMessage, *agent.SkillReadMessage,
			*agent.TodoMessage, *agent.NativeSubagentMessage, *agent.BackgroundCommandMessage:
		default:
			continue
		}
		if !t.woken {
			t.woken = true
			messages = slices.Insert(messages, i, agent.Message(&agent.TurnStartedMessage{}))
			i++
		}
	}
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
