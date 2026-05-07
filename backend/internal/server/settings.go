// Loads and persists server configuration from settings.json.

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// serverSettings holds persistent server configuration stored in settings.json.
type serverSettings struct {
	SessionSecret string `json:"sessionSecret,omitempty"`
}

// loadSettings reads settings from path, generating any missing values and
// writing them back atomically. New fields added to serverSettings are
// automatically populated on first use and persisted.
func loadSettings(path string) (*serverSettings, error) {
	var s serverSettings
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: internal config path
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, err
		}
	}

	dirty := false
	if s.SessionSecret == "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		s.SessionSecret = hex.EncodeToString(raw[:])
		dirty = true
	}

	if dirty {
		if err := writeSettingsAtomic(path, &s); err != nil {
			slog.Warn("could not persist settings", "path", path, "err", err)
		}
	}
	return &s, nil
}

// writeSettingsAtomic writes settings to path via a temp file + rename.
func writeSettingsAtomic(path string, s *serverSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ") //nolint:gosec // G117: sessionSecret is intentionally written to config file owned by the user
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
