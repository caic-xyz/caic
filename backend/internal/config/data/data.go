// Package data defines the independent TOML configuration schema.
package data

// Config mirrors the TOML file layout at ~/.config/caic/config.toml.
// Zero values mean "not set in file".
// IMPORTANT: When adding or modifying configuration fields, update contrib/config.toml
// accordingly. Document all default values in the example config file.
type Config struct {
	Core         Core               `toml:"core"`
	Server       Server             `toml:"server"`
	AI           AI                 `toml:"ai"`
	Harness      map[string]Harness `toml:"harness"`
	GitHub       GitHub             `toml:"github"`
	GitLab       GitLab             `toml:"gitlab"`
	Google       Google             `toml:"google"`
	VoiceGateway VoiceGateway       `toml:"voice-gateway"`
	Debug        Debug              `toml:"debug"`
}

// Harness is the persisted Harness section.
type Harness struct {
	Env map[string]string `toml:"env"`
}

// Core is the persisted Core section.
type Core struct {
	Root       string            `toml:"root"`
	AutoUpdate *string           `toml:"auto_update"` // nil = default schedule; "" = disabled; else cron expression
	Prune      *string           `toml:"prune"`       // nil = default schedule; "" = disabled; else cron expression
	RepoRepack *string           `toml:"repo_repack"` // nil = default schedule; "" = disabled; else cron expression
	Env        map[string]string `toml:"env"`
}

// Server is the persisted Server section.
type Server struct {
	HTTP           string   `toml:"http"`
	ExternalURL    string   `toml:"external_url"`
	GeoDB          string   `toml:"geo_db"`
	AllowOrigins   []string `toml:"allow_origins"`
	TrustedProxies []string `toml:"trusted_proxies"`
}

// Debug is the persisted Debug section.
type Debug struct {
	LogLevel   string `toml:"log_level"`
	NoLogTime  bool   `toml:"no_log_time"`
	Pprof      bool   `toml:"pprof"`
	CPUProfile string `toml:"cpuprofile"`
	MemProfile string `toml:"memprofile"`
	Trace      string `toml:"trace"`
}

// AI is the persisted AI section.
type AI struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
}

// GitHub is the persisted GitHub section.
type GitHub struct {
	PAT   PAT       `toml:"pat"`
	OAuth OAuth     `toml:"oauth"`
	App   GitHubApp `toml:"app"`
}

// GitHubApp is the persisted GitHubApp section.
type GitHubApp struct {
	ID            int64    `toml:"id"`
	PrivateKeyPEM string   `toml:"private_key_pem"` // file path, read at load time
	AllowedOwners []string `toml:"allowed_owners"`
	WebhookSecret string   `toml:"webhook_secret"`
}

// GitLab is the persisted GitLab section.
type GitLab struct {
	PAT           PAT    `toml:"pat"`
	OAuth         OAuth  `toml:"oauth"`
	URL           string `toml:"url"`
	WebhookSecret string `toml:"webhook_secret"`
}

// PAT is the persisted PAT section.
type PAT struct {
	Token string `toml:"token"`
}

// Google is the persisted Google section.
type Google struct {
	OAuth OAuth `toml:"oauth"`
}

// OAuth is the persisted OAuth section.
type OAuth struct {
	ClientID     string   `toml:"client_id"`
	ClientSecret string   `toml:"client_secret"`
	AllowedUsers []string `toml:"allowed_users"`
}

// VoiceGateway is the persisted VoiceGateway section.
type VoiceGateway struct {
	URL                  string      `toml:"url"`
	InstanceID           string      `toml:"instance_id"`
	TokenMode            string      `toml:"token_mode"`
	SigningPrivateKeyPEM string      `toml:"signing_private_key_pem"`
	Config               VoiceConfig `toml:"config"`
}

// VoiceConfig is the persisted voice gateway configuration.
type VoiceConfig struct {
	Server         ServerConfig          `toml:"server"`
	Model          string                `toml:"model"`
	Backend        string                `toml:"backend"`
	LocalStack     LocalStackConfig      `toml:"local_stack"`
	TrustedIssuers []TrustedIssuerConfig `toml:"trusted_issuers"`
}

// ServerConfig is the persisted voice gateway configuration.
type ServerConfig struct {
	HTTP          string `toml:"http"`
	WebRTCUDPPort int    `toml:"webrtc_udp_port"`
}

// LocalStackConfig is the persisted voice gateway configuration.
type LocalStackConfig struct {
	ASR LocalStackASRConfig `toml:"asr"`
	LLM LocalStackLLMConfig `toml:"llm"`
	TTS LocalStackTTSConfig `toml:"tts"`
}

// LocalStackASRConfig is the persisted voice gateway configuration.
type LocalStackASRConfig struct {
	Engine   LocalStackASREngine `toml:"engine"`
	Provider string              `toml:"provider"`
	Remote   string              `toml:"remote"`
	Model    string              `toml:"model"`
}

// LocalStackLLMConfig is the persisted voice gateway configuration.
type LocalStackLLMConfig struct {
	Provider string `toml:"provider"`
	Remote   string `toml:"remote"`
	Model    string `toml:"model"`
	// APIKeyName names the environment variable containing the bearer token
	// for an openaicompatible endpoint. The credential is never stored in TOML.
	APIKeyName string `toml:"api_key_name"`
}

// LocalStackTTSConfig is the persisted voice gateway configuration.
type LocalStackTTSConfig struct {
	Engine LocalStackTTSEngine `toml:"engine"`
	Remote string              `toml:"remote"`
	Model  string              `toml:"model"`
	Voice  string              `toml:"voice"`
}

// TrustedIssuerConfig is the persisted voice gateway configuration.
type TrustedIssuerConfig struct {
	// Service is the service kind allowed to issue tokens, for example "caic" or "mddb".
	Service string `toml:"service"`
	// Issuer is the backend origin that owns the signing key.
	//
	// It must match the service authorization base URL and the token backend origin.
	Issuer string `toml:"issuer"`
	// PublicKey is the imported Ed25519 public key used to verify scoped tokens from issuer.
	//
	// The expected format is the value returned by gomode.EncodeServiceSigningPublicKey.
	PublicKey string `toml:"public_key"`
	// OAuth enables OAuth 2.0 access token verification through discovery and JWKS.
	OAuth bool `toml:"oauth"`
	// Audience is the required OAuth token audience. It defaults to
	// gomode.ScopedTokenAudience when empty.
	Audience string `toml:"audience"`
	// Scope is the OAuth scope a token must carry. It defaults to
	// DefaultVoiceScope when empty.
	Scope string `toml:"scope"`
}

// LocalStackASREngine selects the persisted speech recognition engine.
type LocalStackASREngine string

// LocalStackTTSEngine selects the persisted speech synthesis engine.
type LocalStackTTSEngine string
