// Tests for TOML config loading, field mapping, and server config derivation.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTOMLConfig(t *testing.T) {
	t.Parallel()
	t.Run("missing file returns default config", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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

[voice-gateway]
url = "https://voice.example.com"

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
		if tc.VoiceGateway.URL != "https://voice.example.com" {
			t.Errorf("VoiceGateway.URL = %q", tc.VoiceGateway.URL)
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
		t.Parallel()
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
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		piEnv := cfg.Agent.HarnessEnv["pi"]
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

	t.Run("parses embedded voice gateway config", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `
[core.env]
GEMINI_API_KEY = "AIza_test"

[voice-gateway.config]
model = "gemini-live-test"

[voice-gateway.config.server]
webrtc_udp_port = -1
`
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tc, err := loadTOMLConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if tc.VoiceGateway.Config.Model != "gemini-live-test" {
			t.Errorf("VoiceGateway.Config.Model = %q, want gemini-live-test", tc.VoiceGateway.Config.Model)
		}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Config.Model != "gemini-live-test" {
			t.Errorf("Voice.Gateway.Config.Model = %q, want gemini-live-test", cfg.Voice.Gateway.Config.Model)
		}
		if cfg.Voice.Gateway.Config.Server.WebRTCUDPPort != -1 {
			t.Errorf("Voice.Gateway.Config.Server.WebRTCUDPPort = %d, want -1", cfg.Voice.Gateway.Config.Server.WebRTCUDPPort)
		}
	})

	t.Run("rejects embedded voice gateway http", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `
[voice-gateway.config.server]
http = ":3479"
`
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTOMLConfig(dir)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "voice-gateway.config.server.http") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects voice gateway mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `
[voice-gateway]
mode = "embedded"
`
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTOMLConfig(dir)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "missing in the target struct") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unknown field error", func(t *testing.T) {
		t.Parallel()
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
	t.Parallel()
	t.Run("nil with default file returns default path", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		dir := t.TempDir()
		// Don't create the default file
		got := geoDBOrDefault(nil, dir)
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})
	t.Run("explicit value resolved relative to cfgDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		val := "custom.mmdb"
		got := geoDBOrDefault(&val, dir)
		want := filepath.Join(dir, "custom.mmdb")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("explicit absolute path preserved", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		val := "/etc/mmdb/geo.mmdb"
		got := geoDBOrDefault(&val, dir)
		if got != val {
			t.Errorf("got %q, want %q", got, val)
		}
	})
}

func TestTomlToServerConfig(t *testing.T) {
	t.Parallel()
	t.Run("reads config values", func(t *testing.T) {
		t.Parallel()
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
					"DEEPSEEK_API_KEY":  "sk_deepseek_from_core_env",
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

		cfg, addr, root, logLevel, err := tomlToServerConfig(t.Context(), tc, dir)
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
		if cfg.GitHub.Token != "ghp_abc" {
			t.Errorf("GitHubToken = %q", cfg.GitHub.Token)
		}
		if cfg.GitHub.OAuthAllowedUsers != "alice,bob" {
			t.Errorf("GitHubOAuthAllowedUsers = %q", cfg.GitHub.OAuthAllowedUsers)
		}
		if cfg.GitHub.AppAllowedOwners != "org1,org2" {
			t.Errorf("GitHubAppAllowedOwners = %q", cfg.GitHub.AppAllowedOwners)
		}
		if string(cfg.GitHub.AppPrivateKeyPEM) != "PEM-DATA" {
			t.Errorf("GitHubAppPrivateKeyPEM = %q", cfg.GitHub.AppPrivateKeyPEM)
		}
		if string(cfg.GitHub.WebhookSecret) != "hmac" {
			t.Errorf("GitHubWebhookSecret = %q", cfg.GitHub.WebhookSecret)
		}
		if cfg.IPGeo.DB != filepath.Join(dir, "geo.mmdb") {
			t.Errorf("IPGeoDB = %q", cfg.IPGeo.DB)
		}
		if cfg.IPGeo.Allowlist != "local,tailscale" {
			t.Errorf("IPGeoAllowlist = %q", cfg.IPGeo.Allowlist)
		}
		if cfg.Agent.GeminiAPIKey != "AIza_from_core_env" {
			t.Errorf("GeminiAPIKey = %q, want AIza_from_core_env", cfg.Agent.GeminiAPIKey)
		}
		if cfg.Agent.CoreEnv["DEEPSEEK_API_KEY"] != "sk_deepseek_from_core_env" {
			t.Errorf("CoreEnv[DEEPSEEK_API_KEY] = %q, want sk_deepseek_from_core_env", cfg.Agent.CoreEnv["DEEPSEEK_API_KEY"])
		}
		if cfg.Runtime.TailscaleAPIKey != "tskey_from_core_env" {
			t.Errorf("TailscaleAPIKey = %q, want tskey_from_core_env", cfg.Runtime.TailscaleAPIKey)
		}
	})

	t.Run("geo_db defaults to GeoLite2-Country.mmdb if file exists", func(t *testing.T) {
		t.Parallel()
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
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		wantPath := filepath.Join(dir, "GeoLite2-Country.mmdb")
		if cfg.IPGeo.DB != wantPath {
			t.Errorf("IPGeoDB = %q, want %q", cfg.IPGeo.DB, wantPath)
		}
	})

	t.Run("geo_db is empty when unset and default file missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tc := &tomlConfig{
			Server: tomlServer{
				GeoDB: nil, // not set in config
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IPGeo.DB != "" {
			t.Errorf("IPGeoDB = %q, want empty string", cfg.IPGeo.DB)
		}
	})

	t.Run("core env from config file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wantKey := "AIza_from_config"
		tc := &tomlConfig{
			Core: tomlCore{
				Env: map[string]string{
					"GEMINI_API_KEY": wantKey,
				},
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Agent.GeminiAPIKey != wantKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.Agent.GeminiAPIKey, wantKey)
		}
	})

	t.Run("tailscale_api_key from core env", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wantKey := "tskey_config"
		tc := &tomlConfig{
			Core: tomlCore{
				Env: map[string]string{
					"TAILSCALE_API_KEY": wantKey,
				},
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime.TailscaleAPIKey != wantKey {
			t.Errorf("TailscaleAPIKey = %q, want %q", cfg.Runtime.TailscaleAPIKey, wantKey)
		}
	})

	t.Run("external voice gateway", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tc := &tomlConfig{
			VoiceGateway: tomlVoiceGateway{
				URL: "https://voice.example.com",
			},
		}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Mode != "external" {
			t.Errorf("Voice.Gateway.Mode = %q, want external", cfg.Voice.Gateway.Mode)
		}
		if cfg.Voice.Gateway.URL != "https://voice.example.com" {
			t.Errorf("Voice.Gateway.URL = %q, want https://voice.example.com", cfg.Voice.Gateway.URL)
		}
	})

	t.Run("default voice gateway embedded with gemini key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tc := defaultConfig()
		tc.Core.Env = map[string]string{"GEMINI_API_KEY": "AIza_test"}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Mode != "embedded" {
			t.Errorf("Voice.Gateway.Mode = %q, want embedded", cfg.Voice.Gateway.Mode)
		}
	})

	t.Run("external voice gateway wins over gemini key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tc := defaultConfig()
		tc.Core.Env = map[string]string{"GEMINI_API_KEY": "AIza_test"}
		tc.VoiceGateway.URL = "https://voice.example.com"
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Mode != "external" {
			t.Errorf("Voice.Gateway.Mode = %q, want external", cfg.Voice.Gateway.Mode)
		}
		if cfg.Voice.Gateway.URL != "https://voice.example.com" {
			t.Errorf("Voice.Gateway.URL = %q, want https://voice.example.com", cfg.Voice.Gateway.URL)
		}
	})

	t.Run("embedded voice gateway default config", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tc := defaultConfig()
		tc.Core.Env = map[string]string{"GEMINI_API_KEY": "AIza_test"}
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Mode != "embedded" {
			t.Errorf("Voice.Gateway.Mode = %q, want embedded", cfg.Voice.Gateway.Mode)
		}
		if cfg.Voice.Gateway.Config.Server.HTTP != "" {
			t.Errorf("Voice.Gateway.Config.Server.HTTP = %q, want empty", cfg.Voice.Gateway.Config.Server.HTTP)
		}
	})
}

// TestTomlToServerConfigEnvFallback tests env variable fallback behaviour
// which uses t.Setenv and therefore cannot run in parallel.
func TestTomlToServerConfigEnvFallback(t *testing.T) {
	t.Run("core env from env variable fallback", func(t *testing.T) {
		dir := t.TempDir()
		envKey := "AIza_from_env"
		t.Setenv("GEMINI_API_KEY", envKey)
		tc := &tomlConfig{} // empty config, no core.env set
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Agent.GeminiAPIKey != envKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.Agent.GeminiAPIKey, envKey)
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
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Agent.GeminiAPIKey != configKey {
			t.Errorf("GeminiAPIKey = %q, want %q", cfg.Agent.GeminiAPIKey, configKey)
		}
	})

	t.Run("default voice gateway disabled without gemini key", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("GEMINI_API_KEY", "")
		tc := defaultConfig()
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), &tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Voice.Gateway.Mode != "disabled" {
			t.Errorf("Voice.Gateway.Mode = %q, want disabled", cfg.Voice.Gateway.Mode)
		}
	})

	t.Run("tailscale_api_key from env variable fallback", func(t *testing.T) {
		dir := t.TempDir()
		envKey := "tskey_env"
		t.Setenv("TAILSCALE_API_KEY", envKey)
		tc := &tomlConfig{} // empty config
		cfg, _, _, _, err := tomlToServerConfig(t.Context(), tc, dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime.TailscaleAPIKey != envKey {
			t.Errorf("TailscaleAPIKey = %q, want %q", cfg.Runtime.TailscaleAPIKey, envKey)
		}
	})
}

func TestAutoUpdateSchedule(t *testing.T) {
	t.Parallel()
	t.Run("default schedule", func(t *testing.T) {
		t.Parallel()
		s, err := autoUpdateSchedule(&tomlConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("expected non-nil schedule for default")
		}
	})

	t.Run("empty disables", func(t *testing.T) {
		t.Parallel()
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: new(string)}})
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			t.Error("expected nil schedule for empty string")
		}
	})

	t.Run("custom cron", func(t *testing.T) {
		t.Parallel()
		cron := "0 3 * * *"
		s, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: &cron}})
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
		t.Parallel()
		cron := "not a cron"
		_, err := autoUpdateSchedule(&tomlConfig{Core: tomlCore{AutoUpdate: &cron}})
		if err == nil {
			t.Error("expected error for invalid cron")
		}
	})
}

func TestAllowOriginsOrDefault(t *testing.T) {
	t.Parallel()
	t.Run("nil returns default", func(t *testing.T) {
		t.Parallel()
		got := allowOriginsOrDefault(nil)
		if len(got) != 3 || got[0] != "local" || got[1] != "tailscale" || got[2] != "github" {
			t.Errorf("got %v, want [local tailscale github]", got)
		}
	})
	t.Run("empty returns default", func(t *testing.T) {
		t.Parallel()
		got := allowOriginsOrDefault([]string{})
		if len(got) != 3 {
			t.Errorf("got %v, want default", got)
		}
	})
	t.Run("explicit values preserved", func(t *testing.T) {
		t.Parallel()
		got := allowOriginsOrDefault([]string{"CA", "US"})
		if len(got) != 2 || got[0] != "CA" || got[1] != "US" {
			t.Errorf("got %v, want [CA US]", got)
		}
	})
}
