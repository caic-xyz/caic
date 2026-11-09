// Request principal authority and task access policy shared by HTTP and MCP operations.

package server

import (
	"context"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

type principalKind int

const (
	principalAnonymous principalKind = iota
	principalLocal
	principalUser
	principalTask
)

// requestPrincipal separates local, human, and delegated task authority.
// A task never inherits administrator authority from an attached human context.
type requestPrincipal struct {
	kind   principalKind
	user   *auth.User
	taskID ksid.ID
}

func requestPrincipalFromContext(ctx context.Context) requestPrincipal {
	if id, ok := taskMCPTaskID(ctx); ok {
		return requestPrincipal{kind: principalTask, taskID: id}
	}
	if u, ok := auth.UserFromContext(ctx); ok && u.ID != "" {
		return requestPrincipal{kind: principalUser, user: u}
	}
	if p, ok := mcpPrincipalFromContext(ctx); ok && p.Remote {
		return requestPrincipal{}
	}
	if auth.LocalOperatorFromContext(ctx) {
		return requestPrincipal{kind: principalLocal}
	}
	return requestPrincipal{}
}

func (p requestPrincipal) role() auth.Role {
	switch p.kind {
	case principalLocal:
		return auth.RoleAdmin
	case principalUser:
		return p.user.Role
	default:
		return ""
	}
}

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
func (access taskAccess) canAccess(t *taskrun.Task) bool {
	return access.canAccessOwner(t.OwnerID, t.ParentTaskID)
}

func (access taskAccess) canAccessOwner(ownerID string, parentTaskID ksid.ID) bool {
	if access.userID != "" && (ownerID == "" || ownerID == access.userID) {
		return true
	}
	if access.delegatingTaskID != 0 {
		return parentTaskID == access.delegatingTaskID
	}
	return access.userID == ""
}

// canInspect permits stable-ID task detail. A task principal can inspect any
// task by its stable ID, while ordinary callers use the regular owner policy.
func (access taskAccess) canInspect(t *taskrun.Task) bool {
	return access.delegatingTaskID != 0 || access.canAccess(t)
}
