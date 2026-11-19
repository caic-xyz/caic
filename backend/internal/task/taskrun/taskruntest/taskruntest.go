// Package taskruntest puts tasks in published states through the taskrun lifecycle and message fold.
package taskruntest

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

// SetState puts tk in published state s and keeps a task already in s. A
// lifecycle phase replaces the current one. Waiting ends a running turn, or
// starts a session that reports no turn. Running, asking, and has_plan start
// a session whose fold reports a turn, a question, or a plan; they require a
// task without a timeline.
func SetState(tk *taskrun.Task, s task.State) {
	if tk.GetState() == s {
		return
	}
	var history []agent.Message
	switch s {
	case task.StatePending, task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateStopping, task.StateStopped, task.StatePurging, task.StatePurged, task.StateCrashed, task.StateFailed:
		tk.Restore(s, time.Time{})
		return
	case task.StateWaiting:
		if tk.SessionLost(time.Time{}) {
			return
		}
	case task.StateRunning:
		history = []agent.Message{&agent.TurnStartedMessage{}}
	case task.StateAsking:
		history = []agent.Message{&agent.TurnStartedMessage{}, &agent.AskMessage{ToolUseID: "question"}}
	case task.StateHasPlan:
		history = []agent.Message{
			&agent.TurnStartedMessage{},
			&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/root/.claude/plans/plan.md","content":"plan"}`)},
			&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
			&agent.ResultMessage{},
		}
	default:
		panic("taskruntest: unsupported state " + string(s))
	}
	tk.Restore(task.StateStarting, time.Time{})
	if len(history) > 0 {
		tk.SeedTimelineParts(agenttest.TimedMessages(history), nil, nil)
	}
	tk.GoLive()
}
