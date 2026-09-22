// Task access policy shared by HTTP and MCP task operations.

package server

import (
	"context"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// taskAccess identifies the user and optional task principal making a request.
type taskAccess struct {
	userID           string
	delegatingTaskID ksid.ID
}

func taskAccessFromContext(ctx context.Context) taskAccess {
	access := taskAccess{}
	if user, ok := auth.UserFromContext(ctx); ok {
		access.userID = user.ID
	}
	access.delegatingTaskID, _ = taskMCPTaskID(ctx)
	return access
}

// canAccess permits ordinary task access. A user may access their own and
// legacy unowned tasks; a task principal may access its direct children.
func (access taskAccess) canAccess(t *task.Task) bool {
	if access.userID != "" && (t.OwnerID == "" || t.OwnerID == access.userID) {
		return true
	}
	if access.delegatingTaskID != 0 {
		return t.ParentTaskID == access.delegatingTaskID
	}
	return access.userID == ""
}

// canInspect permits stable-ID task detail. A task principal can inspect any
// task by its stable ID, while ordinary callers use the regular owner policy.
func (access taskAccess) canInspect(t *task.Task) bool {
	return access.delegatingTaskID != 0 || access.canAccess(t)
}
