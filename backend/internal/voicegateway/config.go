// Standalone voice gateway configuration and compatibility metadata.

package voicegateway

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
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
)

// Config is the static voice gateway configuration.
type Config struct {
	Server         ServerConfig          `toml:"server"`
	Model          string                `toml:"model"`
	TrustedIssuers []TrustedIssuerConfig `toml:"trusted_issuers"`
}

// ServerConfig configures gateway HTTP and WebRTC listeners.
type ServerConfig struct {
	HTTP          string `toml:"http"`
	WebRTCUDPPort int    `toml:"webrtc_udp_port"`
}

// TrustedIssuerConfig configures a service backend trusted to issue scoped tokens.
type TrustedIssuerConfig struct {
	// Service is the service kind allowed to issue tokens, for example "caic" or "mddb".
	Service string `toml:"service"`
	// Issuer is the backend origin that owns the signing key.
	//
	// It must match the service authorization base URL and the token backend origin.
	Issuer string `toml:"issuer"`
	// PublicKey is the imported Ed25519 public key used to verify tokens from issuer.
	//
	// The expected format is the value returned by EncodeServiceSigningPublicKey.
	PublicKey string `toml:"public_key"`
}

// Compatibility is returned by GET /api/v1/voice/compat on the standalone voice gateway.
type Compatibility struct {
	Service      string   `json:"service"`
	ServiceKinds []string `json:"serviceKinds"`
	Capabilities []string `json:"capabilities"`
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
	kinds := make([]string, 0, len(c.TrustedIssuers))
	seen := map[string]struct{}{}
	for _, issuer := range c.TrustedIssuers {
		if issuer.Service == "" {
			continue
		}
		if _, ok := seen[issuer.Service]; ok {
			continue
		}
		seen[issuer.Service] = struct{}{}
		kinds = append(kinds, issuer.Service)
	}
	return Compatibility{
		Service:      "voice-gateway",
		ServiceKinds: kinds,
		Capabilities: c.capabilities(),
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
	for i, issuer := range c.TrustedIssuers {
		errs = append(errs, validateTrustedIssuer(i, issuer))
	}
	return errors.Join(errs...)
}

func (c *Config) capabilities() []string {
	caps := []string{"voice.gatewayGeminiLive"}
	if len(c.TrustedIssuers) > 0 {
		caps = append(caps, "voice.serviceSignedTokens")
	}
	return caps
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
	if issuer.PublicKey == "" {
		errs = append(errs, fmt.Errorf("%s.public_key is required", prefix))
	} else if _, err := ParseServiceSigningPublicKey(issuer.PublicKey); err != nil {
		errs = append(errs, fmt.Errorf("%s.public_key: %w", prefix, err))
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
