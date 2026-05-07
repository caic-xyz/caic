// Tests for TOML config loading, field mapping, and server config derivation.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTOMLConfig(t *testing.T) {
	t.Run("missing file returns default config", func(t *testing.T) {
		tc, err := loadTOMLConfig(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		want := defaultConfig()
		if tc.Server.HTTP != want.Server.HTTP {
			t.Fatalf("want HTTP %q, got %q", want.Server.HTTP, tc.Server.HTTP)
		}
		if tc.Core.Root != want.Core.Root {
			t.Fatalf("want Root %q, got %q", want.Core.Root, tc.Core.Root)
		}
	})

	t.Run("parses all fields", func(t *testing.T) {
		dir := t.TempDir()
		content := `
[core]
root = "/srv/repos"
auto_update = ""

[core.env]
TAILSCALE_API_KEY = "tskey_test"
GEMINI_API_KEY = "AIza_test"

[server]
http = ":9090"
external_url = "https://example.com"
webrtc_port = 3478
geo_db = "GeoLite2-Country.mmdb"
allow_origins = ["local", "CA", "US"]

[ai]
provider = "anthropic"
model = "claude-haiku-4-5-20251001"

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
		if tc.Core.AutoUpdate == nil || *tc.Core.AutoUpdate != "" {
			t.Errorf("AutoUpdate = %v, want empty string", tc.Core.AutoUpdate)
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
		if tc.Core.Env["TAILSCALE_API_KEY"] != "tskey_test" {
			t.Errorf("Core.Env[TAILSCALE_API_KEY] = %q", tc.Core.Env["TAILSCALE_API_KEY"])
		}
		if tc.Core.Env["GEMINI_API_KEY"] != "AIza_test" {
			t.Errorf("Core.Env[GEMINI_API_KEY] = %q", tc.Core.Env["GEMINI_API_KEY"])
		}
		if len(tc.Server.AllowOrigins) != 3 {
			t.Errorf("Server.AllowOrigins = %v", tc.Server.AllowOrigins)
		}
		if !tc.Debug.Pprof {
			t.Error("Debug.Pprof should be true")
		}
	})

	t.Run("parses harness env", func(t *testing.T) {
		dir := t.TempDir()
		content := `
[harness.pi.env]
GEMINI_API_KEY = "AIza_test"
OPENROUTER_API_KEY = "sk-or-test"
`
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tc, err := loadTOMLConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if tc.Harness == nil {
			t.Fatal("Harness is nil")
		}
		pi, ok := tc.Harness["pi"]
		if !ok {
			t.Fatal("missing harness.pi")
		}
		if pi.Env["GEMINI_API_KEY"] != "AIza_test" {
			t.Errorf("GEMINI_API_KEY = %q", pi.Env["GEMINI_API_KEY"])
		}
		if pi.Env["OPENROUTER_API_KEY"] != "sk-or-test" {
			t.Errorf("OPENROUTER_API_KEY = %q", pi.Env["OPENROUTER_API_KEY"])
		}

		// Verify tomlToServerConfig converts to KEY=VALUE slices.
		cfg, _, _, _, err := tomlToServerConfig(&tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		piEnv := cfg.HarnessEnv["pi"]
		if len(piEnv) != 2 {
			t.Fatalf("HarnessEnv[pi] = %v, want 2 entries", piEnv)
		}
		got := make(map[string]struct{})
		for _, kv := range piEnv {
			got[kv] = struct{}{}
		}
		if _, ok := got["GEMINI_API_KEY=AIza_test"]; !ok {
			t.Errorf("missing GEMINI_API_KEY=AIza_test in %v", piEnv)
		}
		if _, ok := got["OPENROUTER_API_KEY=sk-or-test"]; !ok {
			t.Errorf("missing OPENROUTER_API_KEY=sk-or-test in %v", piEnv)
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

func TestGeoDBOrDefault(t *testing.T) {
	t.Run("nil with default file returns default path", func(t *testing.T) {
		dir := t.TempDir()
		// Create the default file
		if err := os.WriteFile(filepath.Join(dir, "GeoLite2-Country.mmdb"), []byte("mmdb"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := geoDBOrDefault(nil, dir)
		want := filepath.Join(dir, "GeoLite2-Country.mmdb")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("nil without default file returns empty", func(t *testing.T) {
		dir := t.TempDir()
		// Don't create the default file
		got := geoDBOrDefault(nil, dir)
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})
	t.Run("explicit value resolved relative to cfgDir", func(t *testing.T) {
		dir := t.TempDir()
		val := "custom.mmdb"
		got := geoDBOrDefault(&val, dir)
		want := filepath.Join(dir, "custom.mmdb")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("explicit absolute path preserved", func(t *testing.T) {
		dir := t.TempDir()
		val := "/etc/mmdb/geo.mmdb"
		got := geoDBOrDefault(&val, dir)
		if got != val {
			t.Errorf("got %q, want %q", got, val)
		}
	})
}

func TestTomlToServerConfig(t *testing.T) {
	t.Run("reads config values", func(t *testing.T) {
		dir := t.TempDir()
		// Write a fake PEM file.
		pemPath := filepath.Join(dir, "key.pem")
		if err := os.WriteFile(pemPath, []byte("PEM-DATA"), 0o600); err != nil {
			t.Fatal(err)
		}

		geoDB := "geo.mmdb"
		tc := &tomlConfig{
			Core: tomlCore{
				Root: "/repos",
				Env: map[string]string{
					"GEMINI_API_KEY":    "AIza_from_core_env",
					"TAILSCALE_API_KEY": "tskey_from_core_env",
				},
			},
			Debug: tomlDebug{
				LogLevel: "warn",
			},
			Server: tomlServer{
				HTTP:         ":2242",
				GeoDB:        &geoDB,
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
		if addr != ":2242" {
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
		if cfg.GeminiAPIKey != "AIza_from_core_env" {
			t.Errorf("GeminiAPIKey = %q, want AIza_from_core_env", cfg.GeminiAPIKey)
		}
		if cfg.TailscaleAPIKey != "tskey_from_core_env" {
			t.Errorf("TailscaleAPIKey = %q, want tskey_from_core_env", cfg.TailscaleAPIKey)
		}
	})

	t.Run("geo_db defaults to GeoLite2-Country.mmdb if file exists", func(t *testing.T) {
		dir := t.TempDir()
		// Create the default file
		if err := os.WriteFile(filepath.Join(dir, "GeoLite2-Country.mmdb"), []byte("mmdb"), 0o600); err != nil {
			t.Fatal(err)
		}
		tc := &tomlConfig{
			Server: tomlServer{
				GeoDB: nil, // not set in config
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		wantPath := filepath.Join(dir, "GeoLite2-Country.mmdb")
		if cfg.IPGeoDB != wantPath {
			t.Errorf("IPGeoDB = %q, want %q", cfg.IPGeoDB, wantPath)
		}
	})

	t.Run("geo_db is empty when unset and default file missing", func(t *testing.T) {
		dir := t.TempDir()
		tc := &tomlConfig{
			Server: tomlServer{
				GeoDB: nil, // not set in config
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IPGeoDB != "" {
			t.Errorf("IPGeoDB = %q, want empty string", cfg.IPGeoDB)
		}
	})

	t.Run("core env from config file", func(t *testing.T) {
		dir := t.TempDir()
		wantKey := "AIza_from_config"
		tc := &tomlConfig{
			Core: tomlCore{
				Env: map[string]string{
					"GEMINI_API_KEY": wantKey,
				},
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GeminiAPIKey != wantKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.GeminiAPIKey, wantKey)
		}
	})

	t.Run("core env from env variable fallback", func(t *testing.T) {
		dir := t.TempDir()
		envKey := "AIza_from_env"
		t.Setenv("GEMINI_API_KEY", envKey)
		tc := &tomlConfig{} // empty config, no core.env set
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GeminiAPIKey != envKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.GeminiAPIKey, envKey)
		}
	})

	t.Run("core env config takes precedence over host env", func(t *testing.T) {
		dir := t.TempDir()
		envKey := "AIza_from_env"
		configKey := "AIza_from_config"
		t.Setenv("GEMINI_API_KEY", envKey)
		tc := &tomlConfig{
			Core: tomlCore{
				Env: map[string]string{
					"GEMINI_API_KEY": configKey,
				},
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GeminiAPIKey != configKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.GeminiAPIKey, configKey)
		}
	})

	t.Run("tailscale_api_key from core env", func(t *testing.T) {
		dir := t.TempDir()
		wantKey := "tskey_config"
		tc := &tomlConfig{
			Core: tomlCore{
				Env: map[string]string{
					"TAILSCALE_API_KEY": wantKey,
				},
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TailscaleAPIKey != wantKey {
			t.Errorf("TailscaleAPIKey = %q, want %q", cfg.TailscaleAPIKey, wantKey)
		}
	})

	t.Run("tailscale_api_key from env variable fallback", func(t *testing.T) {
		dir := t.TempDir()
		envKey := "tskey_env"
		t.Setenv("TAILSCALE_API_KEY", envKey)
		tc := &tomlConfig{} // empty config
		cfg, _, _, _, err := tomlToServerConfig(tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TailscaleAPIKey != envKey {
			t.Errorf("TailscaleAPIKey = %q, want %q", cfg.TailscaleAPIKey, envKey)
		}
	})
}

func ptr(s string) *string { return &s }

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

	t.Run("empty disables", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: ptr("")}})
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			t.Error("expected nil schedule for empty string")
		}
	})

	t.Run("custom cron", func(t *testing.T) {
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: ptr("0 3 * * *")}})
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
		_, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: ptr("not a cron")}})
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
