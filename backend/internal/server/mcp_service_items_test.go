// Tests for caic's generic Go Mode service-item projection.

package server

import (
	"slices"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestServiceItems(t *testing.T) {
	t.Parallel()
	tasks := []v1.Task{
		{ID: ksid.NewID(), Title: "Build feature", State: v1.TaskStateRunning},
		{ID: ksid.NewID(), Title: "Review plan", State: v1.TaskStateHasPlan},
		{ID: ksid.NewID(), Title: "Fix tests", State: v1.TaskStateStopped, Error: "lint failed"},
	}

	items := serviceItems(tasks)
	if len(items) != len(tasks) {
		t.Fatalf("item count = %d, want %d", len(items), len(tasks))
	}
	if !items[0].NeedsAttention || !items[1].NeedsAttention {
		t.Fatalf("attention items = %#v", items)
	}
	if items[2].NeedsAttention {
		t.Fatal("running item needs attention")
	}
	if items[1].State != string(v1.TaskStateHasPlan) {
		t.Fatalf("item state = %q, want %q", items[1].State, v1.TaskStateHasPlan)
	}
}

func TestBoundedServiceItems(t *testing.T) {
	t.Parallel()

	tasks := make([]v1.Task, mcpServiceItemMaxCount+1)
	for i := range tasks {
		tasks[i] = v1.Task{ID: ksid.NewID(), Title: strings.Repeat("x", maxMCPTaskTitle), State: v1.TaskStateRunning}
	}
	compareTaskID := func(a, b v1.Task) int { return strings.Compare(a.ID.String(), b.ID.String()) }
	newestID := slices.MaxFunc(tasks, compareTaskID).ID
	oldestID := slices.MinFunc(tasks, compareTaskID).ID
	for i := range tasks {
		if tasks[i].ID == oldestID {
			tasks[i].State = v1.TaskStateAsking
			break
		}
	}

	output, err := boundedServiceItems(tasks, mcpResourceJSONMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Items) != mcpServiceItemMaxCount || output.OmittedCount != 1 {
		t.Fatalf("items = %d, omitted = %d, want %d and 1", len(output.Items), output.OmittedCount, mcpServiceItemMaxCount)
	}
	if output.Items[0].ID != newestID.String() || output.Items[0].NeedsAttention {
		t.Fatalf("first item = %#v, want newest running task", output.Items[0])
	}
	if output.Items[1].ID != oldestID.String() || !output.Items[1].NeedsAttention {
		t.Fatalf("second item = %#v, want older attention task", output.Items[1])
	}
}
