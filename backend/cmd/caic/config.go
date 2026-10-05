// TOML configuration file loading.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/config/data"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/maruel/gomode/voicegateway"
)

// defaultConfig returns a data.Config with sensible defaults pre-populated.
// TOML decoding overwrites only fields present in the file.
func defaultConfig() data.Config {
	return data.Config{
		Core: data.Core{Root: "."},
		Server: data.Server{
			HTTP:           ":2242",
			ExternalURL:    "auto",
			AllowOrigins:   slices.Clone(defaultAllowOrigins),
			TrustedProxies: []string{},
		},
		Harness: map[string]data.Harness{
			"claude":   {},
			"codex":    {},
			"opencode": {},
			"pi":       {},
		},
		VoiceGateway: data.VoiceGateway{
			TokenMode: string(server.VoiceTokenModeScoped),
			Config:    embeddedVoiceGatewayConfigDefaults(),
		},
		Debug: data.Debug{LogLevel: "info"},
	}
}

func embeddedVoiceGatewayConfigDefaults() data.VoiceConfig {
	cfg := voicegateway.DefaultConfig()
	cfg.Server.HTTP = ""
	return voiceConfigToData(&cfg)
}

// loadTOMLConfig reads and parses config.toml from cfgDir.
// Returns a zero-value config if the file does not exist.
// Returns an error if the file exists but is malformed or contains unknown keys.
func loadTOMLConfig(cfgDir string) (data.Config, error) {
	path := filepath.Join(cfgDir, "config.toml")
	raw, err := os.ReadFile(path) //nolint:gosec // config file from XDG config dir
	if err != nil {
		if os.IsNotExist(err) {
			return defaultConfig(), nil
		}
		return data.Config{}, fmt.Errorf("read config: %w", err)
	}
	tc := defaultConfig()
	dec := toml.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tc); err != nil {
		return data.Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if tc.VoiceGateway.Config.Server.HTTP != "" {
		return data.Config{}, fmt.Errorf("parse %s: voice-gateway.config.server.http is not supported; use server.http", path)
	}
	slog.Info("loaded config", "path", path)
	return tc, nil
}

// resolveFilePath resolves a path relative to cfgDir and reads the file content.
// Returns nil if path is empty.
func resolveFilePath(path, cfgDir string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cfgDir, path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // trusted config value
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, nil
}

// resolvePath resolves a path relative to cfgDir. Returns "" if path is empty.
func resolvePath(path, cfgDir string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		return filepath.Join(cfgDir, path)
	}
	return path
}

// tomlToServerConfig converts a parsed TOML config into a server.Config.
// cfgDir is used to resolve relative file paths.
func tomlToServerConfig(ctx context.Context, tc *data.Config, cfgDir string) (cfg *server.Config, addr, root, logLevel string, err error) {
	githubKeyPEM, err := resolveFilePath(tc.GitHub.App.PrivateKeyPEM, cfgDir)
	if err != nil {
		return nil, "", "", "", err
	}
	prune, err := pruneSchedule(tc)
	if err != nil {
		return nil, "", "", "", err
	}
	repoRepack, err := repoRepackSchedule(tc)
	if err != nil {
		return nil, "", "", "", err
	}
	// gh CLI fallback: when no token and no OAuth configured, try gh auth token.
	// TODO: remove OAuth guard once gh auth token reliably provides a scoped PAT.
	ghToken := tc.GitHub.PAT.Token
	if ghToken == "" && tc.GitHub.OAuth.ClientID == "" {
		ghToken = resolveGitHubTokenFromGH(ctx)
	}
	// Resolve core env vars: explicit config values take precedence over the host environment.
	tailscaleAPIKey := coreEnvOrDefault(tc.Core.Env, "TAILSCALE_API_KEY")
	geminiAPIKey := coreEnvOrDefault(tc.Core.Env, "GEMINI_API_KEY")
	voiceGatewayMode := server.VoiceGatewayModeDisabled
	if tc.VoiceGateway.URL != "" {
		voiceGatewayMode = server.VoiceGatewayModeExternal
	} else if geminiAPIKey != "" {
		voiceGatewayMode = server.VoiceGatewayModeEmbedded
	}
	var voiceTokenMode server.VoiceTokenMode
	switch tc.VoiceGateway.TokenMode {
	case "", string(server.VoiceTokenModeScoped):
		voiceTokenMode = server.VoiceTokenModeScoped
	case string(server.VoiceTokenModeOAuth):
		voiceTokenMode = server.VoiceTokenModeOAuth
	default:
		return nil, "", "", "", fmt.Errorf("voice gateway token_mode must be %q or %q, got %q", server.VoiceTokenModeScoped, server.VoiceTokenModeOAuth, tc.VoiceGateway.TokenMode)
	}
	var voiceSigningKey ed25519.PrivateKey
	if voiceGatewayMode == server.VoiceGatewayModeExternal && tc.VoiceGateway.SigningPrivateKeyPEM != "" {
		keyPEM, err := resolveFilePath(tc.VoiceGateway.SigningPrivateKeyPEM, cfgDir)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("voice gateway signing key: %w", err)
		}
		block, _ := pem.Decode(keyPEM)
		if block == nil {
			return nil, "", "", "", errors.New("voice gateway signing key: invalid PEM")
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("voice gateway signing key: %w", err)
		}
		var ok bool
		voiceSigningKey, ok = key.(ed25519.PrivateKey)
		if !ok {
			return nil, "", "", "", errors.New("voice gateway signing key must be Ed25519")
		}
	}

	// Convert per-harness env maps to KEY=VALUE slices.
	harnessEnv := make(map[string][]string, len(tc.Harness))
	for name, h := range tc.Harness {
		for k, v := range h.Env {
			harnessEnv[name] = append(harnessEnv[name], k+"="+v)
		}
	}

	cfg = &server.Config{
		Dirs: server.DirsConfig{
			ConfigDir: cfgDir,
			CacheDir:  cacheDir(),
		},
		Runtime: server.RuntimeConfig{
			TailscaleAPIKey:    tailscaleAPIKey,
			ImagePruneSchedule: prune,
			RepoRepackSchedule: repoRepack,
		},
		Agent: server.AgentConfig{
			HarnessEnv: harnessEnv,
			CoreEnv:    tc.Core.Env,
		},
		LLM: server.LLMConfig{
			Provider: tc.AI.Provider,
			Model:    tc.AI.Model,
		},
		GitHub: server.GitHubConfig{
			Token:             ghToken,
			OAuthClientID:     tc.GitHub.OAuth.ClientID,
			OAuthClientSecret: tc.GitHub.OAuth.ClientSecret,
			OAuthAllowedUsers: tc.GitHub.OAuth.AllowedUsers,
			WebhookSecret:     []byte(tc.GitHub.App.WebhookSecret),
			AppID:             tc.GitHub.App.ID,
			AppPrivateKeyPEM:  githubKeyPEM,
			AppAllowedOwners:  tc.GitHub.App.AllowedOwners,
		},
		GitLab: server.GitLabConfig{
			Token:             tc.GitLab.PAT.Token,
			OAuthClientID:     tc.GitLab.OAuth.ClientID,
			OAuthClientSecret: tc.GitLab.OAuth.ClientSecret,
			OAuthAllowedUsers: tc.GitLab.OAuth.AllowedUsers,
			URL:               tc.GitLab.URL,
			WebhookSecret:     []byte(tc.GitLab.WebhookSecret),
		},
		Google: server.GoogleConfig{
			OAuthClientID:     tc.Google.OAuth.ClientID,
			OAuthClientSecret: tc.Google.OAuth.ClientSecret,
			OAuthAllowedUsers: tc.Google.OAuth.AllowedUsers,
		},
		Auth: server.AuthConfig{
			ExternalURL:    tc.Server.ExternalURL,
			TrustedProxies: tc.Server.TrustedProxies,
		},
		Voice: server.VoiceConfig{
			Gateway: server.VoiceGatewayConfig{
				Mode:       voiceGatewayMode,
				URL:        tc.VoiceGateway.URL,
				Issuer:     tc.Server.ExternalURL,
				InstanceID: tc.VoiceGateway.InstanceID,
				TokenMode:  voiceTokenMode,
				SigningKey: voiceSigningKey,
				Config:     voiceConfigToRuntime(&tc.VoiceGateway.Config),
			},
		},
		Debug: server.DebugConfig{
			Pprof: tc.Debug.Pprof,
		},
		IPGeo: server.IPGeoConfig{
			DB:        geoDBOrDefault(tc.Server.GeoDB, cfgDir),
			Allowlist: strings.Join(allowOriginsOrDefault(tc.Server.AllowOrigins), ","),
		},
	}
	return cfg, tc.Server.HTTP, tc.Core.Root, tc.Debug.LogLevel, nil
}

// defaultAllowOrigins is the default allowlist when allow_origins is not set.
var defaultAllowOrigins = []string{"local", "tailscale", "github"}

// allowOriginsOrDefault returns origins if non-empty, otherwise the default.
// defaultConfig pre-populates the default, so this guards against a config that
// explicitly sets allow_origins = [], which would otherwise produce an empty
// (and rejected) allowlist.
func allowOriginsOrDefault(origins []string) []string {
	if len(origins) == 0 {
		return defaultAllowOrigins
	}
	return origins
}

// geoDBOrDefault returns the geo_db path to use, or empty when geoip is disabled.
//
// If geoDB is empty (not set in config), checks for "GeoLite2-Country.mmdb" in
// cfgDir and returns that path only if it exists; otherwise returns empty string
// (geoip disabled). If geoDB is set, returns the configured value resolved
// relative to cfgDir (validation happens in main).
func geoDBOrDefault(geoDB, cfgDir string) string {
	if geoDB == "" {
		// Try the default, but only if it exists.
		defaultPath := filepath.Join(cfgDir, "GeoLite2-Country.mmdb")
		if _, err := os.Stat(defaultPath); err == nil {
			return defaultPath
		}
		return ""
	}
	// Explicitly set; resolve relative to cfgDir.
	return resolvePath(geoDB, cfgDir)
}

// coreEnvOrDefault returns the value for key from the core env map, falling
// back to the host environment variable of the same name when the map does
// not contain the key.
func coreEnvOrDefault(env map[string]string, key string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return os.Getenv(key)
}

// defaultAutoUpdate is the default cron schedule: daily at 04:50 local time.
const defaultAutoUpdate = "50 4 * * *"

// defaultPrune is the default cron schedule: daily at 05:00 local time.
const defaultPrune = "0 5 * * *"

// defaultRepoRepack repacks large repositories daily at 02:00 local time.
const defaultRepoRepack = "0 2 * * *"

// autoUpdateSchedule returns the parsed auto-update schedule, or nil if
// disabled. When auto_update is not set in the config file, the default
// schedule "50 4 * * *" (daily at 04:50) is used. Set to "" to disable.
func autoUpdateSchedule(tc *data.Config) (*autoupdate.Schedule, error) {
	if tc.Core.AutoUpdate == nil {
		s, err := autoupdate.ParseSchedule(defaultAutoUpdate)
		if err != nil {
			return nil, fmt.Errorf("core.auto_update: %w", err)
		}
		return &s, nil
	}
	if *tc.Core.AutoUpdate == "" {
		return nil, nil //nolint:nilnil // nil schedule means disabled, not an error
	}
	s, err := autoupdate.ParseSchedule(*tc.Core.AutoUpdate)
	if err != nil {
		return nil, fmt.Errorf("core.auto_update: %w", err)
	}
	return &s, nil
}

// pruneSchedule returns the parsed image-prune schedule, or nil if disabled.
// When prune is not set in the config file, the default schedule "0 5 * * *"
// (daily at 05:00) is used. Set to "" to disable.
func pruneSchedule(tc *data.Config) (*autoupdate.Schedule, error) {
	if tc.Core.Prune == nil {
		s, err := autoupdate.ParseSchedule(defaultPrune)
		if err != nil {
			return nil, fmt.Errorf("core.prune: %w", err)
		}
		return &s, nil
	}
	if *tc.Core.Prune == "" {
		return nil, nil //nolint:nilnil // nil schedule means disabled, not an error
	}
	s, err := autoupdate.ParseSchedule(*tc.Core.Prune)
	if err != nil {
		return nil, fmt.Errorf("core.prune: %w", err)
	}
	return &s, nil
}

func repoRepackSchedule(tc *data.Config) (*autoupdate.Schedule, error) {
	if tc.Core.RepoRepack == nil {
		s, err := autoupdate.ParseSchedule(defaultRepoRepack)
		if err != nil {
			return nil, fmt.Errorf("core.repo_repack: %w", err)
		}
		return &s, nil
	}
	if *tc.Core.RepoRepack == "" {
		return nil, nil //nolint:nilnil // nil schedule means disabled, not an error
	}
	s, err := autoupdate.ParseSchedule(*tc.Core.RepoRepack)
	if err != nil {
		return nil, fmt.Errorf("core.repo_repack: %w", err)
	}
	return &s, nil
}
