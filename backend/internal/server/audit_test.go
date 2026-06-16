// Tests for audit event recording.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/auth"
)

func TestAuditStore(t *testing.T) {
	t.Parallel()

	t.Run("persistence success", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mcp_audit.jsonl")
		store := &auditStore{path: path}
		store.record(t.Context(), &auditEvent{Operation: "tools/call", Name: "tasks_list", Decision: "allow", Status: "ok"})

		events := readAuditEvents(t, path)
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one", events)
		}
		event := events[0]
		if event.Operation != "tools/call" || event.Name != "tasks_list" || event.Decision != "allow" || event.Status != "ok" || event.Time.IsZero() {
			t.Fatalf("event = %+v", event)
		}
	})

	t.Run("redaction", func(t *testing.T) {
		t.Parallel()

		got := auditArgsSummary(json.RawMessage(`{"token":"ghp_secret","url":"https://user:pass@example.com/repo.git","env":"OPENAI_API_KEY=sk-secret"}`))
		for _, secret := range []string{"ghp_secret", "pass", "sk-secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("audit args contain %q: %s", secret, got)
			}
		}
		for _, want := range []string{redactedValue, "OPENAI_API_KEY=" + redactedValue} {
			if !strings.Contains(got, want) {
				t.Fatalf("audit args = %s, want %q", got, want)
			}
		}
	})

	t.Run("denied call", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mcp_audit.jsonl")
		store := &auditStore{path: path}
		store.record(t.Context(), &auditEvent{Operation: "tools/call", Name: "task_create", Args: auditArgsSummary(json.RawMessage(`{"prompt":"TOKEN=secret"}`)), Decision: "missing required MCP scope: caic:tasks.write", Status: "blocked"})

		events := readAuditEvents(t, path)
		if len(events) != 1 {
			t.Fatalf("events = %+v, want one", events)
		}
		if events[0].Decision == "allow" || !strings.Contains(events[0].Decision, "missing required MCP scope") {
			t.Fatalf("decision = %q", events[0].Decision)
		}
		if strings.Contains(events[0].Args, "secret") {
			t.Fatalf("args were not redacted: %s", events[0].Args)
		}
	})

	t.Run("write failure fails open", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "audit-dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		store := &auditStore{path: path}
		user := &auth.User{ID: "usr_1", Username: "alice"}
		store.record(auth.NewContext(t.Context(), user), &auditEvent{Operation: "resources/read", Name: "caic://tasks", Decision: "allow", Status: "ok"})

		events := store.snapshot()
		if len(events) != 1 {
			t.Fatalf("events = %+v, want in-memory event despite write failure", events)
		}
		if events[0].UserID != user.ID {
			t.Fatalf("userID = %q, want %q", events[0].UserID, user.ID)
		}
	})
}

func readAuditEvents(t *testing.T, path string) []auditEvent {
	data, err := os.ReadFile(path) //nolint:gosec // test path from t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]auditEvent, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &events[i]); err != nil {
			t.Fatalf("Unmarshal line %d: %v", i+1, err)
		}
	}
	return events
}
