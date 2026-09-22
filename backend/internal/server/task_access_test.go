// Tests for shared HTTP and MCP task access policy.

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
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

func TestTaskAccessTransportMatrix(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	owner := &auth.User{ID: "owner"}
	ctx := auth.NewContext(t.Context(), owner)
	mineID := ksid.NewID()
	mine := mustNewTask(t, mineID, agent.Prompt{Text: "owned task"}, harness.Claude)
	mine.OwnerID = owner.ID
	insertTestTask(s, mineID.String(), mine)
	foreignID := ksid.NewID()
	foreign := mustNewTask(t, foreignID, agent.Prompt{Text: "foreign task"}, harness.Claude)
	foreign.OwnerID = "other"
	insertTestTask(s, foreignID.String(), foreign)

	registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
	if !ok {
		t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
	}
	mcpCtx := newMCPPrincipalContext(ctx, &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeTasksRead}})
	mineURI := "caic://tasks/" + mineID.String()
	foreignURI := "caic://tasks/" + foreignID.String()

	t.Run("list API", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks", nil)
		w := httptest.NewRecorder()
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var tasks []struct {
			ID ksid.ID `json:"id"`
		}
		if err := json.NewDecoder(w.Body).Decode(&tasks); err != nil {
			t.Fatalf("decode task list: %v", err)
		}
		if len(tasks) != 1 || tasks[0].ID != mineID {
			t.Fatalf("task list = %+v, want only %s", tasks, mineID)
		}
	})

	t.Run("task list SSE", func(t *testing.T) {
		t.Parallel()
		streamCtx, cancel := context.WithCancel(ctx)
		cancel()
		streamCtx = context.WithValue(streamCtx, httpLoggerKey{}, slog.New(slog.DiscardHandler))
		req := httptest.NewRequestWithContext(streamCtx, http.MethodGet, "/tasks/events", nil)
		w := httptest.NewRecorder()
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, mineID.String()) {
			t.Fatalf("task-list SSE does not contain owned task %s: %s", mineID, body)
		}
		if strings.Contains(body, foreignID.String()) {
			t.Fatalf("task-list SSE contains foreign task %s: %s", foreignID, body)
		}
	})

	t.Run("MCP resource read", func(t *testing.T) {
		t.Parallel()
		result, err := registry.ReadResource(mcpCtx, mineURI)
		if err != nil {
			t.Fatalf("read owned task resource: %v", err)
		}
		if len(result.Contents) != 1 || !strings.Contains(result.Contents[0].Text, mineID.String()) {
			t.Fatalf("owned task resource = %+v, want %s", result.Contents, mineID)
		}
		if _, err := registry.ReadResource(mcpCtx, foreignURI); err == nil {
			t.Fatalf("read foreign task resource succeeded")
		}
	})

	t.Run("MCP subscription", func(t *testing.T) {
		t.Parallel()
		sources, err := registry.subscriptionSources(mcpCtx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{mineURI}})
		if err != nil {
			t.Fatalf("subscribe to owned task resource: %v", err)
		}
		if _, err := registry.subscriptionSources(mcpCtx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{foreignURI}}); err == nil {
			t.Fatal("subscribe to foreign task resource succeeded")
		}
		update := sources.taskUpdate(mcpCtx, registry)
		if len(update.ResourceURIs) != 1 || update.ResourceURIs[0] != mineURI {
			t.Fatalf("subscription update = %+v, want only %s", update, mineURI)
		}
	})
}
