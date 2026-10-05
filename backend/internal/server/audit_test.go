// Tests for audit event recording.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/data/audit"
)

func TestAuditStore(t *testing.T) {
	t.Parallel()

	t.Run("historical full and minimal disk shapes", func(t *testing.T) {
		t.Parallel()
		const full = `{"time":"2026-02-05T10:00:00.123456789Z","userID":"usr_1","subject":"task:abc","scopes":["caic:tasks.read"],"operation":"tools/call","name":"tasks_list","args":"{\"limit\":2}","decision":"allow","status":"ok"}`
		var e audit.Event
		if err := json.Unmarshal([]byte(full), &e); err != nil {
			t.Fatal(err)
		}
		if e.Time.Nanosecond() != 123456789 || e.Args != `{"limit":2}` || e.UserID != "usr_1" {
			t.Fatalf("historical event = %+v", e)
		}
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		store := &auditStore{log: testLogger(), path: path}
		if err := store.persistLocked(&e); err != nil {
			t.Fatal(err)
		}
		minimal := audit.Event{Time: time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC), Operation: "resources/read", Name: "caic://tasks", Decision: "allow", Scopes: []string{}}
		if err := store.persistLocked(&minimal); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // Temporary fixture directory.
		if err != nil {
			t.Fatal(err)
		}
		const wantMinimal = `{"time":"2026-02-05T10:00:00Z","operation":"resources/read","name":"caic://tasks","decision":"allow"}`
		if string(raw) != full+"\n"+wantMinimal+"\n" {
			t.Fatalf("audit JSON = %s", raw)
		}
	})

	t.Run("persistence success", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mcp_audit.jsonl")
		store := &auditStore{log: testLogger(), path: path}
		store.record(t.Context(), &audit.Event{Operation: "tools/call", Name: "tasks_list", Decision: "allow", Status: "ok"})

		events := readAuditEvents(t, path)
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one", events)
		}
		event := events[0]
		if event.Operation != "tools/call" || event.Name != "tasks_list" || event.Decision != "allow" || event.Status != "ok" || event.Time.IsZero() {
			t.Fatalf("event = %+v", event)
		}
	})

	t.Run("argument summary preserves values", func(t *testing.T) {
		t.Parallel()

		got := auditArgsSummary(json.RawMessage(`{"token":"ghp_secret","url":"https://user:pass@example.com/repo.git","env":"OPENAI_API_KEY=sk-secret"}`))
		for _, want := range []string{"ghp_secret", "pass", "sk-secret"} {
			if !strings.Contains(got, want) {
				t.Fatalf("audit args = %s, want preserved value %q", got, want)
			}
		}
	})

	t.Run("denied call", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mcp_audit.jsonl")
		store := &auditStore{log: testLogger(), path: path}
		store.record(t.Context(), &audit.Event{Operation: "tools/call", Name: "task_create", Args: auditArgsSummary(json.RawMessage(`{"prompt":"TOKEN=secret"}`)), Decision: "missing required MCP scope: caic:tasks.create", Status: "blocked"})

		events := readAuditEvents(t, path)
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one", events)
		}
		if events[0].Decision == "allow" || !strings.Contains(events[0].Decision, "missing required MCP scope") {
			t.Fatalf("decision = %q", events[0].Decision)
		}
		if !strings.Contains(events[0].Args, "secret") {
			t.Fatalf("args did not preserve the supplied value: %s", events[0].Args)
		}
	})

	t.Run("write failure fails open", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "audit-dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		store := &auditStore{log: testLogger(), path: path}
		user := &auth.User{ID: "usr_1", Username: "alice"}
		store.record(auth.NewContext(t.Context(), user), &audit.Event{Operation: "resources/read", Name: "caic://tasks", Decision: "allow", Status: "ok"})

		events := store.snapshot()
		if len(events) != 1 {
			t.Fatalf("events = %+v, want in-memory event despite write failure", events)
		}
		if events[0].UserID != user.ID {
			t.Fatalf("userID = %q, want %q", events[0].UserID, user.ID)
		}
	})

	t.Run("valid task principal is attributed", func(t *testing.T) {
		t.Parallel()

		store := &auditStore{log: testLogger()}
		id := ksid.NewID()
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: id, Remote: true})
		store.record(ctx, &audit.Event{Operation: "tools/call", Name: "task_create", Decision: "allow"})
		events := store.snapshot()
		if len(events) != 1 || events[0].Subject != "task:"+id.String() {
			t.Fatalf("audit events = %+v", events)
		}
	})
}

func readAuditEvents(t *testing.T, path string) []audit.Event {
	data, err := os.ReadFile(path) //nolint:gosec // test path from t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]audit.Event, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &events[i]); err != nil {
			t.Fatalf("Unmarshal line %d: %v", i+1, err)
		}
	}
	return events
}
