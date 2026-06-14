// MCP audit event recording.

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

type mcpAuditStore struct {
	path string

	mu     sync.Mutex
	events []mcpAuditEvent
}

type mcpAuditEvent struct {
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

func (s *mcpAuditStore) record(ctx context.Context, e *mcpAuditEvent) {
	if s == nil || e == nil {
		return
	}
	e.Time = time.Now().UTC()
	if u, ok := auth.UserFromContext(ctx); ok {
		e.UserID = u.ID
	}
	if p, ok := mcpPrincipalFromContext(ctx); ok {
		e.Subject = p.Subject
		e.Scopes = make([]string, 0, len(p.Scopes))
		for scope := range p.Scopes {
			e.Scopes = append(e.Scopes, scope)
		}
		slices.Sort(e.Scopes)
	}
	s.mu.Lock()
	s.events = append(s.events, *e)
	if err := s.persistLocked(e); err != nil {
		slog.WarnContext(ctx, "persist mcp audit", "err", err)
	}
	s.mu.Unlock()
	slog.InfoContext(ctx, "mcp audit", "operation", e.Operation, "name", e.Name, "decision", e.Decision, "status", e.Status, "userID", e.UserID)
}

func (s *mcpAuditStore) snapshot() []mcpAuditEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mcpAuditEvent, len(s.events))
	copy(out, s.events)
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

func (s *mcpAuditStore) persistLocked(e *mcpAuditEvent) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Clean(s.path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
