// Persistent host sessions bind credentials to users and expiry with individual revocation.

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionInfo describes the device that receives a normal host session.
type SessionInfo struct {
	Source    string
	IP        string
	UserAgent string
}

// Session is a persisted host login, independent of MCP authorization grants.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userID"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Source    string    `json:"source"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"userAgent"`
	TokenHash [32]byte  `json:"tokenHash"`
}

type sessionsFile struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
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
	var data sessionsFile
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	if data.Version != 1 {
		return nil, errors.New("unsupported sessions version")
	}
	return data.Sessions, nil
}

func saveSessions(path string, sessions []Session) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sessions-*")
	if err != nil {
		return fmt.Errorf("create sessions: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	err = json.NewEncoder(f).Encode(sessionsFile{Version: 1, Sessions: sessions})
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
