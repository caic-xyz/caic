// Internal backend IDs.

package voicegateway

// Internal backend IDs. A gateway instance serves exactly one of these. They are
// gateway implementation details: they appear in gateway config, logs, and
// tests, but never in the client contract.
const (
	// BackendGeminiLive bridges WebRTC voice sessions to Gemini Live.
	BackendGeminiLive = "gemini-live"
	// BackendLocalCascade is the local VAD/ASR/LLM/TTS cascade.
	BackendLocalCascade = "local-cascade"
)

// knownBackends is the set of backend IDs the gateway recognizes in config.
var knownBackends = map[string]struct{}{
	BackendGeminiLive:   {},
	BackendLocalCascade: {},
}
