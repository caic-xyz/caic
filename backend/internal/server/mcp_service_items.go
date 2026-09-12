// Go Mode service items project authoritative, prioritized caic state into the host-neutral client baseline contract.

package server

import (
	"fmt"
	"slices"
	"strings"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type serviceItem struct {
	ID             string `json:"id"`
	Reference      string `json:"reference"`
	Title          string `json:"title"`
	State          string `json:"state,omitempty"`
	NeedsAttention bool   `json:"needsAttention"`
}

type serviceItemsOutput struct {
	Items         []serviceItem `json:"items"`
	MoreItemsHint string        `json:"moreItemsHint"`
	OmittedCount  int           `json:"omittedCount,omitempty"`
}

func serviceItems(tasks []v1.Task) []serviceItem {
	// The host owns ordering, stable references, and attention priority; see
	// gomode/docs/ANDROID_SHELL.md#service-item-voice-context-ownership.
	items := make([]serviceItem, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		title, _ := truncateMCPTaskTitle(taskTitle(task))
		items[i] = serviceItem{
			ID:             task.ID.String(),
			Reference:      fmt.Sprintf("Task #%d", i+1),
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
