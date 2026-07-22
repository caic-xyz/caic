// Tests for caic's generic Go Mode service-item projection.

package server

import (
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
	if items[0].NeedsAttention {
		t.Fatal("running item needs attention")
	}
	if !items[1].NeedsAttention || !items[2].NeedsAttention {
		t.Fatalf("attention items = %#v", items)
	}
	if items[1].State != string(v1.TaskStateHasPlan) {
		t.Fatalf("item state = %q, want %q", items[1].State, v1.TaskStateHasPlan)
	}
}
