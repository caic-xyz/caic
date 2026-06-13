// MCP audit event recording.

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
)

type mcpAuditStore struct {
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

func newMCPAuditStore() *mcpAuditStore {
	return &mcpAuditStore{}
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
	}
	s.mu.Lock()
	s.events = append(s.events, *e)
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
