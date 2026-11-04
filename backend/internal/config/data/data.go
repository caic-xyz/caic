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
	ShellEnv   bool              `toml:"shell_env"`   // default true; load interactive login shell exports at startup
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
	Mode                 VoiceGatewayMode `toml:"mode"`
	URL                  string           `toml:"url"`
	InstanceID           string           `toml:"instance_id"`
	TokenMode            string           `toml:"token_mode"`
	SigningPrivateKeyPEM string           `toml:"signing_private_key_pem"`
	Config               VoiceConfig      `toml:"config"`
}

// VoiceConfig is the persisted explicit voice gateway pipeline configuration.
type VoiceConfig struct {
	Server         ServerConfig          `toml:"server"`
	Realtime       []RealtimeConfig      `toml:"realtime"`
	Chat           []ChatConfig          `toml:"chat"`
	LocalStack     []LocalStackConfig    `toml:"local_stack"`
	TrustedIssuers []TrustedIssuerConfig `toml:"trusted_issuers"`
}

// ServerConfig is the persisted gateway listener configuration.
type ServerConfig struct {
	HTTP          string `toml:"http"`
	WebRTCUDPPort int    `toml:"webrtc_udp_port"`
}

// ModelConfig selects an explicit provider, model and endpoint.
type ModelConfig struct {
	LlamaCPPServer bool   `toml:"llamacppsrv"`
	Provider       string `toml:"provider"`
	Remote         string `toml:"remote"`
	Model          string `toml:"model"`
	APIKeyName     string `toml:"api_key_name"`
}

// RealtimeConfig is a native audio pipeline and its managed runtime bounds.
type RealtimeConfig struct {
	ModelConfig

	Label               string `toml:"label"`
	CacheDir            string `toml:"cache_dir"`
	Threads             int    `toml:"threads"`
	Slots               int    `toml:"slots"`
	ContextSize         int    `toml:"context_size"`
	MaxOutputTokens     int64  `toml:"max_output_tokens"`
	SetupTimeoutSeconds int    `toml:"setup_timeout_seconds"`
	PendingAudioSeconds int    `toml:"pending_audio_seconds"`
}

// ChatConfig is a text pipeline with client-owned speech.
type ChatConfig struct {
	ModelConfig

	Label string `toml:"label"`
}

// LocalStackConfig is an explicitly configured ASR, LLM and TTS pipeline.
type LocalStackConfig struct {
	Label    string              `toml:"label"`
	TextOnly bool                `toml:"text_only"`
	ASR      LocalStackASRConfig `toml:"asr"`
	LLM      ModelConfig         `toml:"llm"`
	TTS      LocalStackTTSConfig `toml:"tts"`
}

// LocalStackASRConfig is the speech recognition stage.
type LocalStackASRConfig struct {
	Engine   LocalStackASREngine `toml:"engine"`
	Provider string              `toml:"provider"`
	Remote   string              `toml:"remote"`
	Model    string              `toml:"model"`
}

// LocalStackTTSConfig is the speech synthesis stage.
type LocalStackTTSConfig struct {
	Engine LocalStackTTSEngine `toml:"engine"`
	Remote string              `toml:"remote"`
	Model  string              `toml:"model"`
	Voice  string              `toml:"voice"`
}

// TrustedIssuerConfig configures a service allowed to issue gateway tokens.
type TrustedIssuerConfig struct {
	Service   string `toml:"service"`
	Issuer    string `toml:"issuer"`
	PublicKey string `toml:"public_key"`
	OAuth     bool   `toml:"oauth"`
	Scope     string `toml:"scope"`
}

// VoiceGatewayMode selects whether and where the host serves voice.
type VoiceGatewayMode string

const (
	// VoiceGatewayModeDisabled leaves voice disabled.
	VoiceGatewayModeDisabled VoiceGatewayMode = "disabled"
	// VoiceGatewayModeEmbedded starts configured pipelines in the host.
	VoiceGatewayModeEmbedded VoiceGatewayMode = "embedded"
	// VoiceGatewayModeExternal advertises a separately hosted gateway.
	VoiceGatewayModeExternal VoiceGatewayMode = "external"
)

// LocalStackASREngine selects the speech recognition adapter.
type LocalStackASREngine string

const (
	// LocalStackASRGenAI uses a configured genai provider.
	LocalStackASRGenAI LocalStackASREngine = "genai"
	// LocalStackASROpenAIAudio uses an OpenAI-compatible transcription endpoint.
	LocalStackASROpenAIAudio LocalStackASREngine = "openai-audio"
	// LocalStackASRWhistle runs managed Cactus Whistle recognition.
	LocalStackASRWhistle LocalStackASREngine = "whistle"
	// LocalStackASRWhisperCPP connects to whisper.cpp.
	LocalStackASRWhisperCPP LocalStackASREngine = "whispercpp"
)

// LocalStackTTSEngine selects the speech synthesis adapter.
type LocalStackTTSEngine string

const (
	// LocalStackTTSKittenTTS runs managed KittenTTS synthesis.
	LocalStackTTSKittenTTS LocalStackTTSEngine = "kittentts"
	// LocalStackTTSOpenAIAudio uses an OpenAI-compatible speech endpoint.
	LocalStackTTSOpenAIAudio LocalStackTTSEngine = "openai-audio"
)
