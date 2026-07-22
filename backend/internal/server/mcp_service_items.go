// Go Mode service items project caic state into a host-neutral MCP status resource.

package server

import v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"

type serviceItem struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	State          string `json:"state,omitempty"`
	NeedsAttention bool   `json:"needsAttention"`
}

func serviceItems(tasks []v1.Task) []serviceItem {
	items := make([]serviceItem, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		items[i] = serviceItem{
			ID:             task.ID.String(),
			Title:          task.Title,
			State:          string(task.State),
			NeedsAttention: taskNeedsAttention(task),
		}
	}
	return items
}

func taskNeedsAttention(task *v1.Task) bool {
	if task.Error != "" {
		return true
	}
	switch task.State {
	case v1.TaskStateWaiting, v1.TaskStateAsking, v1.TaskStateHasPlan, v1.TaskStateCrashed, v1.TaskStateFailed:
		return true
	default:
		return false
	}
}
