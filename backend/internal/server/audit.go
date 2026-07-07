// Audit event recording for OAuth authorization and MCP tool operations.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
)

type auditStore struct {
	path string

	mu     sync.Mutex
	events []auditEvent
}

type auditEvent struct {
	Time      time.Time `json:"time"`
	UserID    string    `json:"userID,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Scopes    []string  `json:"scopes,omitempty"`
	Operation string    `json:"operation"`
	Name      string    `json:"name"`
	Args      string    `json:"args,omitempty"`
	Decision  string    `json:"decision"`
	Status    string    `json:"status,omitempty"`
}

// RecordOAuth records an OAuth audit event.
func (a *auditStore) RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any) {
	a.record(ctx, &auditEvent{
		UserID:    userID,
		Operation: operation,
		Name:      name,
		Args:      auditValueSummary(args),
		Decision:  decision,
		Status:    status,
	})
}

func (a *auditStore) record(ctx context.Context, e *auditEvent) {
	if a == nil || e == nil {
		return
	}
	e.Time = time.Now().UTC()
	if u, ok := auth.UserFromContext(ctx); ok {
		e.UserID = u.ID
	}
	if p, ok := mcpPrincipalFromContext(ctx); ok {
		e.Subject = p.Subject
		e.Scopes = slices.Clone(p.Scopes)
	}
	a.mu.Lock()
	a.events = append(a.events, *e)
	if err := a.persistLocked(e); err != nil {
		slog.WarnContext(ctx, "persist audit", "err", err)
	}
	a.mu.Unlock()
	slog.InfoContext(ctx, "audit", "operation", e.Operation, "name", e.Name, "decision", e.Decision, "status", e.Status, "userID", e.UserID)
}

func (a *auditStore) snapshot() []auditEvent {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditEvent, len(a.events))
	copy(out, a.events)
	return out
}

func auditArgsSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return redactString(string(raw))
	}
	data, err := json.Marshal(redactAny(decoded))
	if err != nil {
		return ""
	}
	return string(data)
}

func auditValueSummary(v any) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return auditArgsSummary(data)
}

func (a *auditStore) persistLocked(e *auditEvent) error {
	if a.path == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Clean(a.path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	return f.Close()
}
