// Tests for voice gateway configuration validation.

package voicegateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	t.Run("missing file returns defaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.HTTP != DefaultHTTP {
			t.Errorf("Server.HTTP = %q, want %q", cfg.Server.HTTP, DefaultHTTP)
		}
		if cfg.Server.WebRTCUDPPort != DefaultWebRTCUDPPort {
			t.Errorf("Server.WebRTCUDPPort = %d, want %d", cfg.Server.WebRTCUDPPort, DefaultWebRTCUDPPort)
		}
		if cfg.Model != DefaultGeminiModel {
			t.Errorf("Model = %q, want %q", cfg.Model, DefaultGeminiModel)
		}
	})

	t.Run("parses static config", func(t *testing.T) {
		t.Parallel()
		publicKey := newTestPublicKey(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		content := fmt.Sprintf(`
model = "gemini-live-test"

[server]
http = ":4444"
webrtc_udp_port = 4445

[[trusted_issuers]]
service = "caic"
issuer = "https://caic.example.com"
public_key = %q
`, publicKey)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Server.HTTP != ":4444" {
			t.Errorf("Server.HTTP = %q, want :4444", cfg.Server.HTTP)
		}
		if cfg.Model != "gemini-live-test" {
			t.Errorf("Model = %q, want gemini-live-test", cfg.Model)
		}
		if len(cfg.TrustedIssuers) != 1 || cfg.TrustedIssuers[0].Service != "caic" {
			t.Errorf("TrustedIssuers = %+v, want one caic issuer", cfg.TrustedIssuers)
		}
	})

	t.Run("unknown field error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("bogus = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "missing in the target struct") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	t.Run("valid default", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid issuer URL", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "not a url",
			PublicKey: newTestPublicKey(t),
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "trusted_issuers[0].issuer") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows disabled webrtc port", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.WebRTCUDPPort = -1
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("embedded allows missing http", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.HTTP = ""
		if err := cfg.ValidateEmbedded(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid webrtc port", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.WebRTCUDPPort = -2
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "server.webrtc_udp_port") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects invalid issuer public key", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "https://caic.example.com",
			PublicKey: "bogus",
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "trusted_issuers[0].public_key") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows trusted public key issuer", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "https://caic.example.com",
			PublicKey: newTestPublicKey(t),
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}
