// Persistent host sessions bind credentials to users and expiry with individual revocation.

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/auth/data/v1"
)

// SessionInfo describes the device that receives a normal host session.
type SessionInfo struct {
	Source    string
	IP        string
	UserAgent string
}

// Session is a persisted host login, independent of MCP authorization grants.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	Source    string
	IP        string
	UserAgent string
	TokenHash [32]byte
}

func loadSessions(path string) (out []Session, retErr error) {
	f, err := os.Open(path) //nolint:gosec // Startup-owned path beside the users file.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open sessions: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	var data v1.SessionsFile
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	if data.Version != v1.Version {
		return nil, fmt.Errorf("unsupported sessions version %d", data.Version)
	}
	out = make([]Session, len(data.Sessions))
	for i := range data.Sessions {
		r := &data.Sessions[i]
		out[i] = Session{ID: r.ID, UserID: r.UserID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, Source: r.Source, IP: r.IP, UserAgent: r.UserAgent, TokenHash: r.TokenHash}
	}
	return out, nil
}

func saveSessions(path string, sessions []Session) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sessions-*")
	if err != nil {
		return fmt.Errorf("create sessions: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	records := make([]v1.Session, len(sessions))
	for i := range sessions {
		r := &sessions[i]
		records[i] = v1.Session{ID: r.ID, UserID: r.UserID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, Source: r.Source, IP: r.IP, UserAgent: r.UserAgent, TokenHash: r.TokenHash}
	}
	err = json.NewEncoder(f).Encode(v1.SessionsFile{Version: v1.Version, Sessions: records})
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return fmt.Errorf("write sessions: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace sessions: %w", err)
	}
	return nil
}
