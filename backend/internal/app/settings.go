// Persistent app startup settings.

package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
)

type settings struct {
	SessionSecret string `json:"sessionSecret,omitempty"`
	// TODO: Migrate to a oauth section.
	OAuthPrivateKeyPEM string `json:"mcpOAuthPrivateKeyPEM,omitempty"`
	OAuthKeyID         string `json:"mcpOAuthKeyID,omitempty"`
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
	if s.OAuthPrivateKeyPEM == "" || s.OAuthKeyID == "" {
		keyPEM, keyID, err := newMCPOAuthSigningKey()
		if err != nil {
			return nil, err
		}
		s.OAuthPrivateKeyPEM = keyPEM
		s.OAuthKeyID = keyID
		dirty = true
	}

	if dirty {
		if err := writeSettingsAtomic(path, &s); err != nil {
			slog.Warn("could not persist settings", "path", path, "err", err)
		}
	}
	return &s, nil
}

func newMCPOAuthSigningKey() (keyPEM, keyID string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block)), hex.EncodeToString(raw[:]), nil
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
