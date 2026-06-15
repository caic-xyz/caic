// Server startup configuration and validation.

package server

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
)

// Config bundles values read once at startup from config.toml, environment
// variables, and CLI flags, then threaded into the server.
type Config struct {
	Dirs    DirsConfig
	Runtime RuntimeConfig
	Agent   AgentConfig
	LLM     LLMConfig
	GitHub  GitHubConfig
	GitLab  GitLabConfig
	Auth    AuthConfig
	Voice   VoiceConfig
	Debug   DebugConfig
	IPGeo   IPGeoConfig
}

// Validate returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	if err := c.Runtime.Validate(); err != nil {
		return err
	}
	if err := c.GitHub.Validate(); err != nil {
		return err
	}
	if err := c.GitLab.Validate(); err != nil {
		return err
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	if err := c.Voice.Validate(); err != nil {
		return err
	}

	if !c.oauthConfigured() {
		return nil
	}
	if c.Auth.ExternalURL == "" {
		return errors.New("external_url is required when OAuth login is configured")
	}
	if !strings.EqualFold(c.Auth.ExternalURL, "auto") && !c.Auth.externalURLUsesHTTPS() {
		return errors.New("external_url must use https:// when OAuth login is configured")
	}
	return nil
}

func (c *Config) oauthConfigured() bool {
	return c.GitHub.OAuthClientID != "" || c.GitLab.OAuthClientID != ""
}

// DirsConfig contains filesystem paths for persistent server state.
type DirsConfig struct {
	ConfigDir string // persistent server state, e.g. ~/.config/caic
	CacheDir  string // logs and cache files, e.g. ~/.cache/caic
}

// RuntimeConfig selects and configures task runtime provisioning.
type RuntimeConfig struct {
	Name            string                // container runtime: "docker" or "podman" (default: "docker")
	TailscaleAPIKey string                // required for Tailscale networking inside runtime instances
	Backend         runtime.Backend       // optional runtime lifecycle override for smoke/e2e tests
	Monitor         runtime.Monitor       // optional runtime stats/events override for smoke/e2e tests
	Inventory       runtime.Inventory     // optional runtime inventory override for smoke/e2e tests
	Privilege       runtime.PrivilegeInfo // optional privileged runtime info override for smoke/e2e tests
	// SkipWarmup skips base-image warmup at startup. Used by e2e fake mode to
	// avoid pulling Docker images that aren't needed for testing.
	SkipWarmup bool
}

// Validate returns an error if the runtime configuration is invalid.
func (c *RuntimeConfig) Validate() error {
	if c.Name != "" && c.Name != "docker" && c.Name != "podman" {
		return fmt.Errorf("core.runtime must be \"docker\" or \"podman\", got %q", c.Name)
	}
	return nil
}

// AgentConfig configures coding-agent process environments.
type AgentConfig struct {
	HarnessEnv map[string][]string            // per-harness KEY=VALUE env vars for runtime instances
	CoreEnv    map[string]string              // server-level KEY=VALUE env vars from [core.env]
	Backends   map[harness.Name]agent.Backend // optional agent backend override for smoke/e2e tests
}

// LLMConfig configures title generation and commit-description LLM calls.
type LLMConfig struct {
	Provider string
	Model    string
	Disable  bool
}

// GitHubConfig configures GitHub API, OAuth, webhooks, and App integration.
type GitHubConfig struct {
	Token             string // PAT for GitHub API access
	OAuthClientID     string // OAuth app client ID
	OAuthClientSecret string
	OAuthAllowedUsers string // comma-separated; required with OAuth
	WebhookSecret     []byte // HMAC secret; enables POST /webhooks/github
	AppID             int64  // GitHub App ID; used with AppPrivateKeyPEM
	AppPrivateKeyPEM  []byte // RSA private key PEM
	AppAllowedOwners  string // comma-separated; if set, reject installs from other owners
}

// Validate returns an error if the GitHub configuration is invalid.
func (c *GitHubConfig) Validate() error {
	if (c.OAuthClientID == "") != (c.OAuthClientSecret == "") {
		return errors.New("github.oauth.client_id and github.oauth.client_secret must both be set or both be unset")
	}
	if c.OAuthClientID != "" && c.OAuthAllowedUsers == "" {
		return errors.New("github.oauth.allowed_users is required when GitHub OAuth login is configured")
	}
	return nil
}

// GitLabConfig configures GitLab API, OAuth, and webhooks.
type GitLabConfig struct {
	Token             string // PAT; mutually exclusive with OAuthClientID
	OAuthClientID     string // OAuth app client ID; mutually exclusive with Token
	OAuthClientSecret string
	OAuthAllowedUsers string // comma-separated; required with OAuth
	URL               string // default "https://gitlab.com"
	WebhookSecret     []byte // X-Gitlab-Token secret; enables POST /webhooks/gitlab
}

// Validate returns an error if the GitLab configuration is invalid.
func (c *GitLabConfig) Validate() error {
	if (c.OAuthClientID == "") != (c.OAuthClientSecret == "") {
		return errors.New("gitlab.oauth.client_id and gitlab.oauth.client_secret must both be set or both be unset")
	}
	if c.URL != "" {
		if err := validateBaseURL("gitlab.url", c.URL); err != nil {
			return err
		}
	}
	if c.Token != "" && c.OAuthClientID != "" {
		return errors.New("gitlab.token and gitlab.oauth.client_id are mutually exclusive: " +
			"remove gitlab.token when using GitLab OAuth login")
	}
	if c.OAuthClientID != "" && c.OAuthAllowedUsers == "" {
		return errors.New("gitlab.oauth.allowed_users is required when GitLab OAuth login is configured")
	}
	return nil
}

// AuthConfig configures public URL state used by OAuth and webhook flows.
type AuthConfig struct {
	// ExternalURL is the public base URL (e.g. https://caic.example.com).
	// "auto" locks the hostname from the first FQDN request.
	ExternalURL string
}

// Validate returns an error if the auth configuration is invalid.
func (c *AuthConfig) Validate() error {
	if c.ExternalURL == "" || strings.EqualFold(c.ExternalURL, "auto") {
		return nil
	}
	if err := validateBaseURL("external_url", c.ExternalURL); err != nil {
		return err
	}
	c.normalizeExternalURL()
	return nil
}

func (c *AuthConfig) normalizeExternalURL() {
	c.ExternalURL = strings.TrimRight(c.ExternalURL, "/")
}

func (c *AuthConfig) externalURLUsesHTTPS() bool {
	u, err := url.Parse(c.ExternalURL)
	return err == nil && u.Scheme == "https"
}

// VoiceConfig configures the WebRTC voice bridge.
type VoiceConfig struct {
	Gateway VoiceGatewayConfig
}

// Validate returns an error if the voice configuration is invalid.
func (c *VoiceConfig) Validate() error {
	return c.Gateway.Validate()
}

// VoiceGatewayMode describes how caic exposes voice gateway support.
type VoiceGatewayMode string

// Voice gateway support modes.
const (
	VoiceGatewayModeDisabled VoiceGatewayMode = "disabled"
	VoiceGatewayModeEmbedded VoiceGatewayMode = "embedded"
	VoiceGatewayModeExternal VoiceGatewayMode = "external"
)

// VoiceGatewayConfig is caic's effective reference to a voice gateway.
type VoiceGatewayConfig struct {
	Mode   VoiceGatewayMode
	URL    string
	Config voicegateway.Config
}

// Validate returns an error if the voice gateway configuration is invalid.
func (c *VoiceGatewayConfig) Validate() error {
	switch c.Mode {
	case "", VoiceGatewayModeDisabled:
		return nil
	case VoiceGatewayModeExternal:
		if c.URL == "" {
			return errors.New("voice gateway URL is required for external mode")
		}
		return validateBaseURL("voice gateway URL", c.URL)
	case VoiceGatewayModeEmbedded:
		return c.Config.ValidateEmbedded()
	default:
		return fmt.Errorf("voice gateway mode must be %q, %q, or %q, got %q", VoiceGatewayModeDisabled, VoiceGatewayModeEmbedded, VoiceGatewayModeExternal, c.Mode)
	}
}

// DebugConfig configures optional server diagnostics.
type DebugConfig struct {
	Pprof bool // expose /debug/pprof/* endpoints
}

// IPGeoConfig configures optional request geolocation allowlisting.
type IPGeoConfig struct {
	// DB is the path to a MaxMind MMDB file (e.g. GeoLite2-Country.mmdb).
	DB string
	// Allowlist is a comma-separated list of permitted country codes, CIDRs, and
	// named origins: "local", "tailscale", "anthropic", "github", and "openai".
	// When set, requests from IPs that do not resolve to an allowed value are
	// rejected with 403. Requires DB for country-code tokens.
	Allowlist string
}

func validateBaseURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a valid URL: %q", name, value)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%s must not contain a path: %q", name, value)
	}
	return nil
}
