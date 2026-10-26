// Audit event recording for OAuth authorization, device login, and MCP tool operations.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/data/audit"
	"github.com/maruel/gomode/devicelogin"
)

type auditStore struct {
	log  *slog.Logger
	path string

	mu     sync.Mutex
	events []audit.Event
}

// RecordOAuth records an OAuth audit event.
func (a *auditStore) RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any) {
	a.record(ctx, &audit.Event{
		UserID:    userID,
		Operation: operation,
		Name:      name,
		Args:      auditValueSummary(args),
		Decision:  decision,
		Status:    status,
	})
}

func (a *auditStore) record(ctx context.Context, e *audit.Event) {
	if a == nil || e == nil {
		return
	}
	e.Time = time.Now().UTC()
	if u, ok := auth.UserFromContext(ctx); ok {
		e.UserID = u.ID
	}
	if p, ok := mcpPrincipalFromContext(ctx); ok {
		if p.TaskID != 0 {
			e.Subject = "task:" + p.TaskID.String()
		} else {
			e.Subject = p.Subject
		}
		e.Scopes = slices.Clone(p.Scopes)
	}
	a.mu.Lock()
	a.events = append(a.events, *e)
	if err := a.persistLocked(e); err != nil {
		a.log.WarnContext(ctx, "persist audit", "err", err)
	}
	a.mu.Unlock()
	a.log.Log(ctx, auditLogLevel(e), "audit", "operation", e.Operation, "name", e.Name, "decision", e.Decision, "status", e.Status, "userID", e.UserID)
}

// auditLogLevel keeps successful allows out of the default logs. The audit trail
// is the persisted event log; the record mirrors only failures and denials.
func auditLogLevel(e *audit.Event) slog.Level {
	if e.Decision != "allow" || (e.Status != "" && e.Status != "ok") {
		return slog.LevelInfo
	}
	return slog.LevelDebug
}

func (a *auditStore) snapshot() []audit.Event {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]audit.Event, len(a.events))
	copy(out, a.events)
	return out
}

func auditArgsSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	data, err := json.Marshal(decoded)
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

func (a *auditStore) persistLocked(e *audit.Event) error {
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
	// Pairing completion requires its audit to reach durable storage before credentials.
	if e.Operation == "device_login" {
		return errors.Join(f.Sync(), f.Close())
	}
	return f.Close()
}

func (a *auditStore) recordDeviceLogin(_ context.Context, e *devicelogin.AuditEvent) error {
	if a.path == "" {
		return errors.New("device login requires a persistent audit path")
	}
	userID, _, ok := strings.Cut(e.Subject, "\x00")
	if !ok {
		return errors.New("invalid approving session")
	}
	decision := "deny"
	if e.Approved {
		decision = "allow"
	}
	status := "ok"
	switch e.Kind {
	case devicelogin.AuditSessionFailed, devicelogin.AuditResponseFailed, devicelogin.AuditRevocationFailed, devicelogin.AuditSessionCreationUncertain:
		status = "failed"
	default:
	}
	event := audit.Event{Time: time.Now().UTC(), UserID: userID, Operation: "device_login", Name: string(e.Kind), Decision: decision, Status: status, Args: auditValueSummary(struct {
		RequestID   string    `json:"requestID"`
		SessionID   string    `json:"sessionID,omitempty"`
		IP          string    `json:"ip"`
		UserAgent   string    `json:"userAgent"`
		RequestedAt time.Time `json:"requestedAt"`
	}{e.RequestID, e.SessionID, e.IP, e.UserAgent, e.CreatedAt})}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.persistLocked(&event); err != nil {
		return err
	}
	a.events = append(a.events, event)
	return nil
}
