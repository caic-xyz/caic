// Persistent app startup settings.

package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

type settings struct {
	SessionSecret string `json:"sessionSecret,omitempty"`
}

func loadSettings(path string) (*settings, error) {
	var s settings
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

func writeSettingsAtomic(path string, s *settings) error {
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
