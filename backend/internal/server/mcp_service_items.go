// Go Mode service items project authoritative, prioritized caic state into the host-neutral client baseline contract.

package server

import (
	"fmt"
	"slices"
	"strings"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// serviceItem is a task entry in the host-neutral baseline for native clients.
// References identify positions in the original task snapshot, independently
// of the priority order in which items are delivered.
type serviceItem struct {
	ID             string `json:"id"`
	Reference      string `json:"reference,omitempty"`
	Title          string `json:"title"`
	State          string `json:"state,omitempty"`
	NeedsAttention bool   `json:"needsAttention"`
}

// serviceItemsOutput carries a bounded baseline and guidance for fetching
// tasks omitted by either the item-count or encoded-byte limit.
type serviceItemsOutput struct {
	Items         []serviceItem `json:"items"`
	MoreItemsHint string        `json:"moreItemsHint"`
	OmittedCount  int           `json:"omittedCount,omitempty"`
}

// serviceItems projects tasks with bounded titles and snapshot-based references.
// The newest ID comes first, followed by other attention-needed tasks, preserving
// snapshot order within each remaining priority group. A lone task has no reference.
func serviceItems(tasks []v1.Task) []serviceItem {
	// The service owns ordering, stable references, and attention priority; see
	// github.com/maruel/gomode/docs/ANDROID_SHELL.md#service-item-voice-context-ownership.
	// Number tasks only when more than one is visible: a lone task needs no
	// disambiguating reference in voice context.
	numbered := len(tasks) > 1
	items := make([]serviceItem, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		title, _ := truncateMCPTaskTitle(taskTitle(task))
		reference := ""
		if numbered {
			reference = fmt.Sprintf("Task #%d", i+1)
		}
		items[i] = serviceItem{
			ID:             task.ID.String(),
			Reference:      reference,
			Title:          title,
			State:          string(task.State),
			NeedsAttention: taskNeedsAttention(task),
		}
	}
	if len(items) == 0 {
		return items
	}
	newestID := slices.MaxFunc(items, func(a, b serviceItem) int { return strings.Compare(a.ID, b.ID) }).ID
	slices.SortStableFunc(items, func(a, b serviceItem) int {
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
// JSON bytes, including the paging hint and omitted count in the byte budget.
// It fails if even the empty response cannot fit maxBytes.
func boundedServiceItems(tasks []v1.Task, maxBytes int) (serviceItemsOutput, error) {
	items := serviceItems(tasks)
	if len(items) > mcpServiceItemMaxCount {
		items = items[:mcpServiceItemMaxCount]
	}
	outputForItems := func(items []serviceItem) serviceItemsOutput {
		return serviceItemsOutput{
			Items:         items,
			MoreItemsHint: "For fuller or current detail, call tasks_list and follow nextCursor until it is absent.",
			OmittedCount:  len(tasks) - len(items),
		}
	}
	fitted, err := fitMCPJSONPrefix(items, maxBytes, func(items []serviceItem) (any, error) {
		return outputForItems(items), nil
	})
	if err != nil {
		return serviceItemsOutput{}, err
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
