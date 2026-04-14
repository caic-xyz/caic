package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTOMLConfig(t *testing.T) {
	t.Run("missing file returns zero config", func(t *testing.T) {
		tc, err := loadTOMLConfig(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if tc.Server.HTTP != "" {
			t.Fatalf("want empty HTTP, got %q", tc.Server.HTTP)
		}
	})

	t.Run("parses all fields", func(t *testing.T) {
		dir := t.TempDir()
		content := `
[core]
root = "/srv/repos"
auto_update = "off"
tailscale_api_key = "tskey_test"

[server]
http = ":9090"
external_url = "https://example.com"
webrtc_port = 3478
geo_db = "GeoLite2-Country.mmdb"
allow_origins = ["local", "CA", "US"]

[ai]
provider = "anthropic"
model = "claude-haiku-4-5-20251001"
gemini_api_key = "AIza_test"

[github]
token = "ghp_test"
oauth_allowed_users = ["alice", "bob"]
app_id = 42
app_allowed_owners = ["org1"]
webhook_secret = "secret123"

[gitlab]
token = "glpat_test"
url = "https://gitlab.example.com"
webhook_secret = "glsecret"

[debug]
log_level = "debug"
pprof = true
`
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tc, err := loadTOMLConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if tc.Server.HTTP != ":9090" {
			t.Errorf("HTTP = %q, want :9090", tc.Server.HTTP)
		}
		if tc.Core.Root != "/srv/repos" {
			t.Errorf("Root = %q, want /srv/repos", tc.Core.Root)
		}
		if tc.Debug.LogLevel != "debug" {
			t.Errorf("LogLevel = %q, want debug", tc.Debug.LogLevel)
		}
		if tc.Core.AutoUpdate != "off" {
			t.Errorf("AutoUpdate = %q, want off", tc.Core.AutoUpdate)
		}
		if tc.GitHub.Token != "ghp_test" {
			t.Errorf("GitHub.Token = %q", tc.GitHub.Token)
		}
		if len(tc.GitHub.OAuthAllowedUsers) != 2 || tc.GitHub.OAuthAllowedUsers[0] != "alice" {
			t.Errorf("GitHub.OAuthAllowedUsers = %v", tc.GitHub.OAuthAllowedUsers)
		}
		if tc.GitHub.AppID != 42 {
			t.Errorf("GitHub.AppID = %d", tc.GitHub.AppID)
		}
		if tc.Server.WebRTCPort != 3478 {
			t.Errorf("Server.WebRTCPort = %d", tc.Server.WebRTCPort)
		}
		if tc.AI.GeminiAPIKey != "AIza_test" {
			t.Errorf("AI.GeminiAPIKey = %q", tc.AI.GeminiAPIKey)
		}
		if tc.Core.TailscaleAPIKey != "tskey_test" {
			t.Errorf("Core.TailscaleAPIKey = %q", tc.Core.TailscaleAPIKey)
		}
		if len(tc.Server.AllowOrigins) != 3 {
			t.Errorf("Server.AllowOrigins = %v", tc.Server.AllowOrigins)
		}
		if !tc.Debug.Pprof {
			t.Error("Debug.Pprof should be true")
		}
	})

	t.Run("unknown field error", func(t *testing.T) {
		dir := t.TempDir()
		content := `bogus_field = "oops"` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTOMLConfig(dir)
		if err == nil {
			t.Fatal("expected error for unknown field")
		}
		if !strings.Contains(err.Error(), "missing in the target struct") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestTomlToServerConfig(t *testing.T) {
	dir := t.TempDir()
	// Write a fake PEM file.
	pemPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(pemPath, []byte("PEM-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	tc := &tomlConfig{
		Core: tomlCore{
			Root: "/repos",
		},
		Debug: tomlDebug{
			LogLevel: "warn",
		},
		Server: tomlServer{
			HTTP:         ":8080",
			GeoDB:        "geo.mmdb",
			AllowOrigins: []string{"local", "tailscale"},
		},
		GitHub: tomlGitHub{
			Token:             "ghp_abc",
			OAuthAllowedUsers: []string{"alice", "bob"},
			AppPrivateKeyPEM:  "key.pem", // relative path
			AppAllowedOwners:  []string{"org1", "org2"},
			WebhookSecret:     "hmac",
		},
	}

	cfg, addr, root, logLevel, err := tomlToServerConfig(tc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":8080" {
		t.Errorf("addr = %q", addr)
	}
	if root != "/repos" {
		t.Errorf("root = %q", root)
	}
	if logLevel != "warn" {
		t.Errorf("logLevel = %q", logLevel)
	}
	if cfg.GitHubToken != "ghp_abc" {
		t.Errorf("GitHubToken = %q", cfg.GitHubToken)
	}
	if cfg.GitHubOAuthAllowedUsers != "alice,bob" {
		t.Errorf("GitHubOAuthAllowedUsers = %q", cfg.GitHubOAuthAllowedUsers)
	}
	if cfg.GitHubAppAllowedOwners != "org1,org2" {
		t.Errorf("GitHubAppAllowedOwners = %q", cfg.GitHubAppAllowedOwners)
	}
	if string(cfg.GitHubAppPrivateKeyPEM) != "PEM-DATA" {
		t.Errorf("GitHubAppPrivateKeyPEM = %q", cfg.GitHubAppPrivateKeyPEM)
	}
	if string(cfg.GitHubWebhookSecret) != "hmac" {
		t.Errorf("GitHubWebhookSecret = %q", cfg.GitHubWebhookSecret)
	}
	if cfg.IPGeoDB != filepath.Join(dir, "geo.mmdb") {
		t.Errorf("IPGeoDB = %q", cfg.IPGeoDB)
	}
	if cfg.IPGeoAllowlist != "local,tailscale" {
		t.Errorf("IPGeoAllowlist = %q", cfg.IPGeoAllowlist)
	}
}

func TestAutoUpdateSchedule(t *testing.T) {
	t.Run("default schedule", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("expected non-nil schedule for default")
		}
	})

	t.Run("off", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: "off"}})
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			t.Error("expected nil schedule for off")
		}
	})

	t.Run("false", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: "false"}})
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			t.Error("expected nil schedule for false")
		}
	})

	t.Run("custom cron", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: "0 3 * * *"}})
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("expected non-nil schedule")
		}
		if len(s.Hour) != 1 || s.Hour[0] != 3 {
			t.Errorf("Hour = %v, want [3]", s.Hour)
		}
	})

	t.Run("invalid cron", func(t *testing.T) {
		_, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: "not a cron"}})
		if err == nil {
			t.Error("expected error for invalid cron")
		}
	})
}

func TestAllowOriginsOrDefault(t *testing.T) {
	t.Run("nil returns default", func(t *testing.T) {
		got := allowOriginsOrDefault(nil)
		if len(got) != 3 || got[0] != "local" || got[1] != "tailscale" || got[2] != "github" {
			t.Errorf("got %v, want [local tailscale github]", got)
		}
	})
	t.Run("empty returns default", func(t *testing.T) {
		got := allowOriginsOrDefault([]string{})
		if len(got) != 3 {
			t.Errorf("got %v, want default", got)
		}
	})
	t.Run("explicit values preserved", func(t *testing.T) {
		got := allowOriginsOrDefault([]string{"CA", "US"})
		if len(got) != 2 || got[0] != "CA" || got[1] != "US" {
			t.Errorf("got %v, want [CA US]", got)
		}
	})
}
