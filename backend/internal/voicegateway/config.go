// Standalone voice gateway configuration and compatibility metadata.

package voicegateway

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	// DefaultHTTP is the default gateway HTTP signaling address.
	DefaultHTTP = ":3479"
	// DefaultWebRTCUDPPort is the default UDP port for WebRTC ICE.
	DefaultWebRTCUDPPort = 0
	// DefaultGeminiModel is the default Gemini Live model advertised by the gateway.
	DefaultGeminiModel = "gemini-3.1-flash-live-preview"
	// GeminiAPIKeyEnv is the environment variable read for Gemini access.
	GeminiAPIKeyEnv = "GEMINI_API_KEY" //nolint:gosec // G101: environment variable name, not a credential.
	// ProtocolVersion is the current voice gateway compatibility protocol.
	ProtocolVersion = 1
)

// Config is the static voice gateway configuration.
type Config struct {
	Server         ServerConfig          `toml:"server"`
	Model          string                `toml:"model"`
	Auth           AuthConfig            `toml:"auth"`
	TrustedIssuers []TrustedIssuerConfig `toml:"trusted_issuers"`
	Services       []ServiceConfig       `toml:"services"`
}

// ServerConfig configures gateway HTTP and WebRTC listeners.
type ServerConfig struct {
	HTTP          string `toml:"http"`
	WebRTCUDPPort int    `toml:"webrtc_udp_port"`
	ExternalURL   string `toml:"external_url"`
}

// AuthConfig configures gateway-owned authentication.
type AuthConfig struct {
	SessionSecretFile string               `toml:"session_secret_file"`
	AllowedUsers      []string             `toml:"allowed_users"`
	AllowTailscale    bool                 `toml:"allow_tailscale"`
	AllowLocalhost    bool                 `toml:"allow_localhost"`
	Google            OAuthProviderConfig  `toml:"google"`
	GitHub            OAuthProviderConfig  `toml:"github"`
	GitLab            GitLabProviderConfig `toml:"gitlab"`
}

// OAuthProviderConfig configures an OAuth provider.
type OAuthProviderConfig struct {
	ClientID        string   `toml:"client_id"`
	ClientSecretEnv string   `toml:"client_secret_env"`
	AllowedUsers    []string `toml:"allowed_users"`
	AllowedDomains  []string `toml:"allowed_domains"`
}

// GitLabProviderConfig configures GitLab OAuth.
type GitLabProviderConfig struct {
	ClientID        string   `toml:"client_id"`
	ClientSecretEnv string   `toml:"client_secret_env"`
	AllowedUsers    []string `toml:"allowed_users"`
	AllowedDomains  []string `toml:"allowed_domains"`
	BaseURL         string   `toml:"base_url"`
}

// TrustedIssuerConfig configures a service backend trusted to issue scoped tokens.
type TrustedIssuerConfig struct {
	Service string `toml:"service"`
	Issuer  string `toml:"issuer"`
	JWKSURL string `toml:"jwks_url"`
}

// ServiceConfig configures one service backend available through the gateway.
type ServiceConfig struct {
	ID           string   `toml:"id"`
	Kind         string   `toml:"kind"`
	BaseURL      string   `toml:"base_url"`
	Capabilities []string `toml:"capabilities"`
}

// Compatibility is returned by GET /compat on the standalone voice gateway.
type Compatibility struct {
	Service         string   `json:"service"`
	GatewayProtocol int      `json:"gatewayProtocol"`
	ServiceKinds    []string `json:"serviceKinds"`
	Capabilities    []string `json:"capabilities"`
}

// DefaultConfig returns the standalone voice gateway defaults.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			HTTP:          DefaultHTTP,
			WebRTCUDPPort: DefaultWebRTCUDPPort,
		},
		Model: DefaultGeminiModel,
	}
}

// LoadConfig reads config.toml, returning defaults when the file does not exist.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // config path is selected by the operator.
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg := DefaultConfig()
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Validate returns an error if c is not a usable static gateway config.
func (c *Config) Validate() error {
	return c.validate(true)
}

// ValidateEmbedded returns an error if c is not usable by an embedded service gateway.
func (c *Config) ValidateEmbedded() error {
	return c.validate(false)
}

// DefaultConfigPath returns the canonical standalone voice gateway config path.
func DefaultConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "voice-gateway", "config.toml")
}

// GeminiAPIKey returns the configured Gemini API key from the process environment.
func (c *Config) GeminiAPIKey() string {
	return os.Getenv(GeminiAPIKeyEnv)
}

// Compatibility returns public compatibility metadata for c.
func (c *Config) Compatibility() Compatibility {
	kinds := make([]string, 0, len(c.Services))
	seen := map[string]struct{}{}
	for _, svc := range c.Services {
		if svc.Kind == "" {
			continue
		}
		if _, ok := seen[svc.Kind]; ok {
			continue
		}
		seen[svc.Kind] = struct{}{}
		kinds = append(kinds, svc.Kind)
	}
	return Compatibility{
		Service:         "voice-gateway",
		GatewayProtocol: ProtocolVersion,
		ServiceKinds:    kinds,
		Capabilities: []string{
			"voice.gatewayGeminiLive",
		},
	}
}

func (c *Config) validate(requireHTTP bool) error {
	var errs []error
	if requireHTTP && c.Server.HTTP == "" {
		errs = append(errs, errors.New("server.http is required"))
	}
	if c.Server.WebRTCUDPPort < -1 || c.Server.WebRTCUDPPort > 65535 {
		errs = append(errs, fmt.Errorf("server.webrtc_udp_port must be between -1 and 65535, got %d", c.Server.WebRTCUDPPort))
	}
	if c.Server.ExternalURL != "" {
		errs = append(errs, validateBaseURL("server.external_url", c.Server.ExternalURL))
	}
	for i, issuer := range c.TrustedIssuers {
		errs = append(errs, validateTrustedIssuer(i, issuer))
	}
	for i, svc := range c.Services {
		errs = append(errs, validateService(i, svc))
	}
	return errors.Join(errs...)
}

func validateTrustedIssuer(i int, issuer TrustedIssuerConfig) error {
	var errs []error
	prefix := fmt.Sprintf("trusted_issuers[%d]", i)
	if issuer.Service == "" {
		errs = append(errs, fmt.Errorf("%s.service is required", prefix))
	}
	if issuer.Issuer == "" {
		errs = append(errs, fmt.Errorf("%s.issuer is required", prefix))
	} else {
		errs = append(errs, validateBaseURL(prefix+".issuer", issuer.Issuer))
	}
	if issuer.JWKSURL == "" {
		errs = append(errs, fmt.Errorf("%s.jwks_url is required", prefix))
	} else {
		errs = append(errs, validateURL(prefix+".jwks_url", issuer.JWKSURL))
	}
	return errors.Join(errs...)
}

func validateService(i int, svc ServiceConfig) error {
	var errs []error
	prefix := fmt.Sprintf("services[%d]", i)
	if svc.ID == "" {
		errs = append(errs, fmt.Errorf("%s.id is required", prefix))
	}
	if svc.Kind == "" {
		errs = append(errs, fmt.Errorf("%s.kind is required", prefix))
	}
	if svc.BaseURL == "" {
		errs = append(errs, fmt.Errorf("%s.base_url is required", prefix))
	} else {
		errs = append(errs, validateBaseURL(prefix+".base_url", svc.BaseURL))
	}
	return errors.Join(errs...)
}

func validateBaseURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a valid URL: %q", name, value)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http:// or https://, got %q", name, value)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%s must not contain a path: %q", name, value)
	}
	return nil
}

func validateURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a valid URL: %q", name, value)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http:// or https://, got %q", name, value)
	}
	return nil
}
