// Go Mode service items project authoritative, prioritized caic state into the host-neutral client baseline contract.

package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// newActiveTask resolves the focused task against the snapshot that assigns
// display numbers. It returns an empty reference when the task is not visible.
func newActiveTask(tasks []v1.Task, activeTaskID string) v1.ServiceActiveTask {
	number := taskNumberForID(tasks, activeTaskID)
	if activeTaskID == "" || number == 0 {
		return v1.ServiceActiveTask{}
	}
	return v1.ServiceActiveTask{ActiveTaskID: activeTaskID, ActiveTaskNumber: number}
}

// serviceItems projects tasks with bounded titles and snapshot-based references.
// The newest ID comes first, followed by other attention-needed tasks, preserving
// snapshot order within each remaining priority group. A lone task has no reference.
func serviceItems(tasks []v1.Task) []mcpschema.ServiceItem {
	// The service owns ordering, stable references, and attention priority; see
	// github.com/maruel/gomode/docs/ANDROID_SHELL.md#server-owned-model-context.
	// Number tasks only when more than one is visible: a lone task needs no
	// disambiguating reference in voice context.
	numbered := len(tasks) > 1
	items := make([]mcpschema.ServiceItem, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		title, _ := truncateMCPTaskTitle(taskTitle(task))
		reference := ""
		if numbered {
			reference = fmt.Sprintf("Task #%d", i+1)
		}
		items[i] = mcpschema.ServiceItem{
			ID:             task.ID.String(),
			Reference:      reference,
			Title:          title,
			State:          string(task.State),
			NeedsAttention: taskNeedsAttention(task),
			CIStatus:       task.CIStatus,
			ForgePR:        task.ForgePR,
		}
	}
	if len(items) == 0 {
		return items
	}
	newestID := slices.MaxFunc(items, func(a, b mcpschema.ServiceItem) int { return strings.Compare(a.ID, b.ID) }).ID
	slices.SortStableFunc(items, func(a, b mcpschema.ServiceItem) int {
		switch {
		case a.ID == newestID && b.ID == newestID:
			return 0
		case a.ID == newestID:
			return -1
		case b.ID == newestID:
			return 1
		case a.NeedsAttention == b.NeedsAttention:
			return 0
		case a.NeedsAttention:
			return -1
		default:
			return 1
		}
	})
	return items
}

// boundedServiceItems caps the prioritized baseline by item count and encoded
// JSON bytes, including the paging hint and omitted count in the
// byte budget. It fails if even the empty response cannot fit maxBytes.
func boundedServiceItems(tasks []v1.Task, maxBytes int) (mcpschema.ServiceItemsOutput, error) {
	items := serviceItems(tasks)
	if len(items) > mcpServiceItemMaxCount {
		items = items[:mcpServiceItemMaxCount]
	}
	outputForItems := func(items []mcpschema.ServiceItem) mcpschema.ServiceItemsOutput {
		return mcpschema.ServiceItemsOutput{
			Items:         items,
			MoreItemsHint: "For fuller or current detail, call tasks_list and follow nextCursor until it is absent.",
			OmittedCount:  len(tasks) - len(items),
		}
	}
	fitted, err := fitMCPJSONPrefix(items, maxBytes, func(items []mcpschema.ServiceItem) (any, error) {
		return outputForItems(items), nil
	})
	if err != nil {
		return mcpschema.ServiceItemsOutput{}, err
	}
	return outputForItems(fitted), nil
}

// taskNeedsAttention reports an explicit task error or a state requiring user
// input, plan approval, or failure recovery.
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
