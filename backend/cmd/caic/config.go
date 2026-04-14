// TOML configuration file loading.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/server"
	toml "github.com/pelletier/go-toml/v2"
)

// tomlConfig mirrors the TOML file layout at ~/.config/caic/config.toml.
// Zero values mean "not set in file".
// IMPORTANT: When adding or modifying configuration fields, update contrib/config.toml
// accordingly. Document all default values in the example config file.
type tomlConfig struct {
	Core   tomlCore   `toml:"core"`
	Server tomlServer `toml:"server"`
	AI     tomlAI     `toml:"ai"`
	GitHub tomlGitHub `toml:"github"`
	GitLab tomlGitLab `toml:"gitlab"`
	Debug  tomlDebug  `toml:"debug"`
}

type tomlCore struct {
	Root            string `toml:"root"`
	AutoUpdate      string `toml:"auto_update"` // cron expression; empty disables
	TailscaleAPIKey string `toml:"tailscale_api_key"`
}

type tomlServer struct {
	HTTP         string   `toml:"http"`
	ExternalURL  string   `toml:"external_url"`
	WebRTCPort   int      `toml:"webrtc_port"`
	GeoDB        *string  `toml:"geo_db"`
	AllowOrigins []string `toml:"allow_origins"`
}

type tomlDebug struct {
	LogLevel   string `toml:"log_level"`
	NoLogTime  bool   `toml:"no_log_time"`
	Pprof      bool   `toml:"pprof"`
	CPUProfile string `toml:"cpuprofile"`
	MemProfile string `toml:"memprofile"`
	Trace      string `toml:"trace"`
}

type tomlAI struct {
	Provider     string `toml:"provider"`
	Model        string `toml:"model"`
	GeminiAPIKey string `toml:"gemini_api_key"`
}

type tomlGitHub struct {
	Token             string   `toml:"token"`
	OAuthClientID     string   `toml:"oauth_client_id"`
	OAuthClientSecret string   `toml:"oauth_client_secret"`
	OAuthAllowedUsers []string `toml:"oauth_allowed_users"`
	WebhookSecret     string   `toml:"webhook_secret"`
	AppID             int64    `toml:"app_id"`
	AppPrivateKeyPEM  string   `toml:"app_private_key_pem"` // file path, read at load time
	AppAllowedOwners  []string `toml:"app_allowed_owners"`
}

type tomlGitLab struct {
	Token             string   `toml:"token"`
	OAuthClientID     string   `toml:"oauth_client_id"`
	OAuthClientSecret string   `toml:"oauth_client_secret"`
	OAuthAllowedUsers []string `toml:"oauth_allowed_users"`
	URL               string   `toml:"url"`
	WebhookSecret     string   `toml:"webhook_secret"`
}

// defaultConfig returns a tomlConfig with sensible defaults pre-populated.
// TOML decoding overwrites only fields present in the file.
func defaultConfig() tomlConfig {
	return tomlConfig{
		Core:   tomlCore{Root: "."},
		Server: tomlServer{HTTP: ":2242", ExternalURL: "auto"},
		Debug:  tomlDebug{LogLevel: "info"},
	}
}

// loadTOMLConfig reads and parses config.toml from cfgDir.
// Returns a zero-value config if the file does not exist.
// Returns an error if the file exists but is malformed or contains unknown keys.
func loadTOMLConfig(cfgDir string) (tomlConfig, error) {
	path := filepath.Join(cfgDir, "config.toml")
	data, err := os.ReadFile(path) //nolint:gosec // config file from XDG config dir
	if err != nil {
		if os.IsNotExist(err) {
			return defaultConfig(), nil
		}
		return tomlConfig{}, fmt.Errorf("read config: %w", err)
	}
	tc := defaultConfig()
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tc); err != nil {
		return tomlConfig{}, fmt.Errorf("parse %s: %w", path, err)
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
	data, err := os.ReadFile(path) //nolint:gosec // trusted config value
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
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
func tomlToServerConfig(tc *tomlConfig, cfgDir string) (cfg *server.Config, addr, root, logLevel string, err error) {
	pem, err := resolveFilePath(tc.GitHub.AppPrivateKeyPEM, cfgDir)
	if err != nil {
		return nil, "", "", "", err
	}
	// gh CLI fallback: when no token and no OAuth configured, try gh auth token.
	ghToken := tc.GitHub.Token
	if ghToken == "" && tc.GitHub.OAuthClientID == "" {
		ghToken = resolveGitHubTokenFromGH()
	}
	// Gemini API key fallback: if not set in config, use environment variable.
	geminiAPIKey := tc.AI.GeminiAPIKey
	if geminiAPIKey == "" {
		geminiAPIKey = os.Getenv("GEMINI_API_KEY")
	}
	cfg = &server.Config{
		ConfigDir:               cfgDir,
		CacheDir:                cacheDir(),
		GeminiAPIKey:            geminiAPIKey,
		TailscaleAPIKey:         tc.Core.TailscaleAPIKey,
		LLMProvider:             tc.AI.Provider,
		LLMModel:                tc.AI.Model,
		GitHubToken:             ghToken,
		GitHubOAuthClientID:     tc.GitHub.OAuthClientID,
		GitHubOAuthClientSecret: tc.GitHub.OAuthClientSecret,
		GitHubOAuthAllowedUsers: strings.Join(tc.GitHub.OAuthAllowedUsers, ","),
		GitHubWebhookSecret:     []byte(tc.GitHub.WebhookSecret),
		GitHubAppID:             tc.GitHub.AppID,
		GitHubAppPrivateKeyPEM:  pem,
		GitHubAppAllowedOwners:  strings.Join(tc.GitHub.AppAllowedOwners, ","),
		GitLabToken:             tc.GitLab.Token,
		GitLabOAuthClientID:     tc.GitLab.OAuthClientID,
		GitLabOAuthClientSecret: tc.GitLab.OAuthClientSecret,
		GitLabOAuthAllowedUsers: strings.Join(tc.GitLab.OAuthAllowedUsers, ","),
		GitLabURL:               tc.GitLab.URL,
		GitLabWebhookSecret:     []byte(tc.GitLab.WebhookSecret),
		ExternalURL:             tc.Server.ExternalURL,
		WebRTCPort:              tc.Server.WebRTCPort,
		IPGeoDB:                 geoDBOrDefault(tc.Server.GeoDB, cfgDir),
		IPGeoAllowlist:          strings.Join(allowOriginsOrDefault(tc.Server.AllowOrigins), ","),
		Pprof:                   tc.Debug.Pprof,
	}
	return cfg, tc.Server.HTTP, tc.Core.Root, tc.Debug.LogLevel, nil
}

// defaultAllowOrigins is the default allowlist when allow_origins is not set.
var defaultAllowOrigins = []string{"local", "tailscale", "github"}

// allowOriginsOrDefault returns origins if non-empty, otherwise the default.
func allowOriginsOrDefault(origins []string) []string {
	if len(origins) == 0 {
		return defaultAllowOrigins
	}
	return origins
}

// geoDBOrDefault returns the geo_db path from the config, the default path if it exists, or empty.
//
// If geoDB is nil (not set in config), checks for "GeoLite2-Country.mmdb" in cfgDir.
// Returns the path only if it exists; otherwise returns empty string (geoip disabled).
// If geoDB is non-nil, returns the configured value (validation happens in main).
func geoDBOrDefault(geoDB *string, cfgDir string) string {
	if geoDB == nil {
		// Try the default, but only if it exists
		defaultPath := filepath.Join(cfgDir, "GeoLite2-Country.mmdb")
		if _, err := os.Stat(defaultPath); err == nil {
			return defaultPath
		}
		return ""
	}
	// Explicitly set; resolve relative to cfgDir
	return resolvePath(*geoDB, cfgDir)
}

// defaultAutoUpdate is the default cron schedule: daily at 04:50 local time.
const defaultAutoUpdate = "50 4 * * *"

// autoUpdateSchedule returns the parsed auto-update schedule, or nil if
// disabled. The default is "50 4 * * *" (daily at 04:50); set to "false"
// or "off" to disable.
func autoUpdateSchedule(tc *tomlConfig) (*autoupdate.Schedule, error) {
	expr := tc.Core.AutoUpdate
	if expr == "" {
		expr = defaultAutoUpdate
	}
	if expr == "false" || expr == "off" {
		return nil, nil //nolint:nilnil // nil schedule means disabled, not an error
	}
	s, err := autoupdate.ParseSchedule(expr)
	if err != nil {
		return nil, fmt.Errorf("core.auto_update: %w", err)
	}
	return &s, nil
}
