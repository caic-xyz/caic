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

	"github.com/caic-xyz/caic/backend/internal/app/data"
)

type settings struct {
	SessionSecret string
	// TODO: Migrate to a oauth section.
	OAuthPrivateKeyPEM string
	OAuthKeyID         string
}

func loadSettings(log *slog.Logger, path string) (*settings, error) {
	var s settings
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: internal config path
		var f data.Settings
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		s = settings{SessionSecret: f.SessionSecret, OAuthPrivateKeyPEM: f.OAuthPrivateKeyPEM, OAuthKeyID: f.OAuthKeyID}
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
			log.Warn("could not persist settings", "path", path, "err", err)
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
	raw, err := json.MarshalIndent(data.Settings{SessionSecret: s.SessionSecret, OAuthPrivateKeyPEM: s.OAuthPrivateKeyPEM, OAuthKeyID: s.OAuthKeyID}, "", "  ") //nolint:gosec // G117: sessionSecret is intentionally written to config file owned by the user
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
