// Go Mode service items project prioritized, bounded caic state into a host-neutral MCP status resource.

package server

import (
	"encoding/json"
	"slices"
	"strings"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type serviceItem struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	State          string `json:"state,omitempty"`
	NeedsAttention bool   `json:"needsAttention"`
}

type serviceItemsOutput struct {
	Items        []serviceItem `json:"items"`
	OmittedCount int           `json:"omittedCount,omitempty"`
}

func serviceItems(tasks []v1.Task) []serviceItem {
	items := make([]serviceItem, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		title, _ := truncateMCPTaskTitle(task.Title)
		items[i] = serviceItem{
			ID:             task.ID.String(),
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
	best := 0
	low, high := 0, len(items)
	for low <= high {
		middle := low + (high-low)/2
		output := serviceItemsOutput{Items: items[:middle], OmittedCount: len(tasks) - middle}
		data, err := json.Marshal(output)
		if err != nil {
			return serviceItemsOutput{}, err
		}
		if len(data) <= maxBytes {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return serviceItemsOutput{Items: items[:best], OmittedCount: len(tasks) - best}, nil
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
