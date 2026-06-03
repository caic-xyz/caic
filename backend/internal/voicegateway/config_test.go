// Tests for voice gateway configuration and compatibility metadata.

package voicegateway

import (
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
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		content := `
model = "gemini-live-test"

[server]
http = ":4444"
webrtc_udp_port = 4445
external_url = "https://voice.example.com"

[auth]
session_secret_file = "session_secret"
allowed_users = ["marc@example.com"]
allow_tailscale = true
allow_localhost = true

[auth.google]
client_id = "google-client"
client_secret_env = "GOOGLE_SECRET"
allowed_domains = ["example.com"]

[auth.gitlab]
client_id = "gitlab-client"
client_secret_env = "GITLAB_SECRET"
base_url = "https://gitlab.example.com"
allowed_users = ["marc"]

[[trusted_issuers]]
service = "caic"
issuer = "https://caic.example.com"
jwks_url = "https://caic.example.com/api/v1/voice/jwks"

[[services]]
id = "home-caic"
kind = "caic"
base_url = "https://caic.example.com"
capabilities = ["tasks"]
`
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
		if cfg.Auth.GitLab.BaseURL != "https://gitlab.example.com" {
			t.Errorf("Auth.GitLab.BaseURL = %q, want https://gitlab.example.com", cfg.Auth.GitLab.BaseURL)
		}
		if len(cfg.Services) != 1 || cfg.Services[0].Kind != "caic" {
			t.Errorf("Services = %+v, want one caic service", cfg.Services)
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

	t.Run("rejects invalid service URL", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Services = []ServiceConfig{{
			ID:      "bad",
			Kind:    "caic",
			BaseURL: "not a url",
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "services[0].base_url") {
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
}

func TestConfigCompatibility(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Services = []ServiceConfig{
		{ID: "home-caic", Kind: "caic", BaseURL: "https://caic.example.com"},
		{ID: "work-caic", Kind: "caic", BaseURL: "https://work.example.com"},
		{ID: "docs", Kind: "mddb", BaseURL: "https://mddb.example.com"},
	}
	got := cfg.Compatibility()
	if got.Service != "voice-gateway" {
		t.Errorf("Service = %q, want voice-gateway", got.Service)
	}
	if got.GatewayProtocol != ProtocolVersion {
		t.Errorf("GatewayProtocol = %d, want %d", got.GatewayProtocol, ProtocolVersion)
	}
	if len(got.ServiceKinds) != 2 || got.ServiceKinds[0] != "caic" || got.ServiceKinds[1] != "mddb" {
		t.Errorf("ServiceKinds = %v, want [caic mddb]", got.ServiceKinds)
	}
}
