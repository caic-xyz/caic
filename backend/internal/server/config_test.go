// Tests for server startup configuration validation.

package server

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/maruel/gomode/voicegateway"
)

func TestVoiceGatewayConfig(t *testing.T) {
	t.Parallel()
	t.Run("Validate external requires URL", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{Mode: VoiceGatewayModeExternal}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "voice gateway URL") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Validate external validates URL", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{
			Mode:       VoiceGatewayModeExternal,
			URL:        "https://voice.example.com",
			Issuer:     "https://caic.example.com",
			InstanceID: "caic-main",
			SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Validate external accepts oauth token mode without signing key", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{
			Mode:       VoiceGatewayModeExternal,
			URL:        "https://voice.example.com",
			Issuer:     "https://caic.example.com",
			InstanceID: "caic-main",
			TokenMode:  VoiceTokenModeOAuth,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Validate external requires signing key for scoped tokens", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{
			Mode:       VoiceGatewayModeExternal,
			URL:        "https://voice.example.com",
			Issuer:     "https://caic.example.com",
			InstanceID: "caic-main",
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "Ed25519 signing key") {
			t.Fatalf("Validate() error = %v, want signing key requirement", err)
		}
	})

	t.Run("Validate external rejects unknown token mode", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{
			Mode:       VoiceGatewayModeExternal,
			URL:        "https://voice.example.com",
			Issuer:     "https://caic.example.com",
			InstanceID: "caic-main",
			TokenMode:  "bogus",
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "token mode") {
			t.Fatalf("Validate() error = %v, want token mode error", err)
		}
	})

	t.Run("Validate external rejects insecure gateway", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{
			Mode:       VoiceGatewayModeExternal,
			URL:        "http://voice.example.com",
			Issuer:     "https://caic.example.com",
			InstanceID: "caic-main",
			SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("Validate() error = %v, want HTTPS requirement", err)
		}
		cfg.URL = "http://127.0.0.1:2242"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("loopback gateway rejected: %v", err)
		}
	})

	t.Run("Validate embedded validates static config", func(t *testing.T) {
		t.Parallel()
		gatewayConfig := voicegateway.DefaultConfig()
		gatewayConfig.Server.HTTP = ""
		gatewayConfig.Server.WebRTCUDPPort = -2
		cfg := VoiceGatewayConfig{Mode: VoiceGatewayModeEmbedded, Config: gatewayConfig}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "server.webrtc_udp_port") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	t.Run("both empty is valid", func(t *testing.T) {
		t.Parallel()
		if err := (&Config{}).Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("PAT only is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{Token: "ghp_abc"}, GitLab: GitLabConfig{Token: "glpat-abc"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL and allowlist is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice", "bob"}},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("ExternalURL auto is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "auto"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL auto is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice"}},
			Auth:   AuthConfig{ExternalURL: "auto"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth without ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth without allowlist is valid (allows any user)", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("GitLab OAuth without allowlist is valid (allows any user)", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitLab: GitLabConfig{OAuthClientID: "id", OAuthClientSecret: "sec"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with http ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice"}},
			Auth:   AuthConfig{ExternalURL: "http://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("OAuth with loopback http ExternalURL is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec"},
			Auth:   AuthConfig{ExternalURL: "http://127.0.0.1:2242"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("invalid ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "not a url"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("invalid trusted proxy is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "auto", TrustedProxies: []string{"not-a-prefix"}}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with subpath is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "https://caic.example.com/sub"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with trailing slash is valid and stripped", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "https://caic.example.com/"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		if c.Auth.ExternalURL != "https://caic.example.com" {
			t.Fatalf("ExternalURL trailing slash not stripped: %q", c.Auth.ExternalURL)
		}
	})
	t.Run("invalid GitLabURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{URL: "not a url"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLabURL with subpath is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{URL: "https://gitlab.example.com/sub"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth ID without secret is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientID: "id"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth secret without ID is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientSecret: "sec"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth ID without secret is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{OAuthClientID: "id"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth secret without ID is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{OAuthClientSecret: "sec"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub PAT and OAuth together is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{Token: "ghp_abc", OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice"}},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("GitLab PAT and OAuth together is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitLab: GitLabConfig{Token: "glpat-abc", OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: []string{"alice"}},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("invalid voice gateway is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			Voice: VoiceConfig{
				Gateway: VoiceGatewayConfig{Mode: VoiceGatewayModeExternal},
			},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
}
