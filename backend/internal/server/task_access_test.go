// Tests for shared HTTP and MCP task access policy.

package server

import (
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/task"
)

func TestTaskAccess(t *testing.T) {
	t.Parallel()
	parentID := ksid.NewID()
	child := &task.Task{OwnerID: "owner", ParentTaskID: parentID}
	foreign := &task.Task{OwnerID: "other", ParentTaskID: ksid.NewID()}
	ownedRoot := &task.Task{OwnerID: "owner"}
	foreignChild := &task.Task{OwnerID: "other", ParentTaskID: parentID}

	t.Run("user", func(t *testing.T) {
		t.Parallel()
		access := taskAccess{userID: "owner"}
		if !access.canAccess(child) || !access.canInspect(child) {
			t.Fatal("owner cannot access or inspect own task")
		}
		if access.canAccess(foreign) || access.canInspect(foreign) {
			t.Fatal("owner can access or inspect foreign task")
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		t.Parallel()
		access := taskAccess{}
		if !access.canAccess(child) || !access.canAccess(foreign) {
			t.Fatal("unauthenticated access denied")
		}
	})

	t.Run("task principal", func(t *testing.T) {
		t.Parallel()
		access := taskAccess{delegatingTaskID: parentID}
		if !access.canAccess(child) || !access.canInspect(child) {
			t.Fatal("task principal cannot access or inspect direct child")
		}
		if access.canAccess(foreign) {
			t.Fatal("task principal can access non-child task")
		}
		if !access.canInspect(foreign) {
			t.Fatal("task principal cannot inspect task by stable ID")
		}
	})

	t.Run("task principal with user", func(t *testing.T) {
		t.Parallel()
		access := taskAccess{userID: "owner", delegatingTaskID: parentID}
		if !access.canAccess(ownedRoot) {
			t.Fatal("task principal cannot access a task owned by its user")
		}
		if !access.canAccess(foreignChild) {
			t.Fatal("task principal cannot access its direct child")
		}
		if access.canAccess(foreign) {
			t.Fatal("task principal can access a task authorized by neither principal")
		}
		if !access.canInspect(foreign) {
			t.Fatal("task principal stable-ID inspection did not bypass user access")
		}
	})
}
