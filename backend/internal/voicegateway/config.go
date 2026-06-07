// Standalone voice gateway configuration.

package voicegateway

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/maruel/genai/providers"
	"github.com/pelletier/go-toml/v2"
)

// Config is the static voice gateway configuration.
//
// A gateway instance serves exactly one backend. Operators run multiple
// instances (and point clients at different URLs) to offer multiple profiles.
type Config struct {
	Server         ServerConfig          `toml:"server"`
	Model          string                `toml:"model"`
	Backend        string                `toml:"backend"`
	LocalStack     LocalStackConfig      `toml:"local_stack"`
	TrustedIssuers []TrustedIssuerConfig `toml:"trusted_issuers"`
}

// ServerConfig configures gateway HTTP and WebRTC listeners.
type ServerConfig struct {
	HTTP          string `toml:"http"`
	WebRTCUDPPort int    `toml:"webrtc_udp_port"`
}

// LocalStackConfig configures local model adapters.
type LocalStackConfig struct {
	LLM LocalStackLLMConfig `toml:"llm"`
}

// LocalStackLLMConfig configures the local LLM adapter.
type LocalStackLLMConfig struct {
	Provider string `toml:"provider"`
	Remote   string `toml:"remote"`
	Model    string `toml:"model"`
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

// DefaultConfig returns the standalone voice gateway defaults.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			HTTP:          ":3479",
			WebRTCUDPPort: 0,
		},
		Model:   "gemini-3.1-flash-live-preview",
		Backend: BackendGeminiLive,
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

func (c *Config) validate(requireHTTP bool) error {
	var errs []error
	if requireHTTP && c.Server.HTTP == "" {
		errs = append(errs, errors.New("server.http is required"))
	}
	if c.Server.WebRTCUDPPort < -1 || c.Server.WebRTCUDPPort > 65535 {
		errs = append(errs, fmt.Errorf("server.webrtc_udp_port must be between -1 and 65535, got %d", c.Server.WebRTCUDPPort))
	}
	switch {
	case c.Backend == "":
		errs = append(errs, errors.New("backend is required"))
	case !isKnownBackend(c.Backend):
		errs = append(errs, fmt.Errorf("backend %q is not a known backend", c.Backend))
	}
	for i, issuer := range c.TrustedIssuers {
		errs = append(errs, validateTrustedIssuer(i, issuer))
	}
	errs = append(errs, c.LocalStack.validate())
	return errors.Join(errs...)
}

func isKnownBackend(backendID string) bool {
	_, ok := knownBackends[backendID]
	return ok
}

func (c *LocalStackConfig) validate() error {
	provider := c.LLM.Provider
	if provider == "" {
		if c.LLM.Remote != "" {
			return errors.New("local_stack.llm.provider is required when local_stack.llm.remote is set")
		}
		provider = "llamacpp"
	}
	if _, ok := providers.All[provider]; !ok {
		return fmt.Errorf("local_stack.llm.provider %q is not supported", c.LLM.Provider)
	}
	return validateBaseURL("local_stack.llm.remote", c.LLM.Remote)
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
	if value == "" {
		return nil
	}
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
