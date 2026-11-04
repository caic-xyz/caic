// Explicit projections between persisted TOML voice settings and gateway runtime config.

package main

import (
	"github.com/caic-xyz/caic/backend/internal/config/data"
	"github.com/maruel/gomode/voicegateway"
)

func voiceConfigToRuntime(c *data.VoiceConfig) voicegateway.Config {
	out := voicegateway.Config{Server: voicegateway.ServerConfig{HTTP: c.Server.HTTP, WebRTCUDPPort: c.Server.WebRTCUDPPort}}
	for i := range c.TrustedIssuers {
		v := &c.TrustedIssuers[i]
		out.TrustedIssuers = append(out.TrustedIssuers, voicegateway.TrustedIssuerConfig{Service: v.Service, Issuer: v.Issuer, PublicKey: v.PublicKey, OAuth: v.OAuth, Scope: v.Scope})
	}
	for i := range c.Realtime {
		v := &c.Realtime[i]
		out.Realtime = append(out.Realtime, voicegateway.RealtimeConfig{Label: v.Label, ModelConfig: voiceModelToRuntime(&v.ModelConfig), CacheDir: v.CacheDir, Threads: v.Threads, Slots: v.Slots, ContextSize: v.ContextSize, MaxOutputTokens: v.MaxOutputTokens, SetupTimeoutSeconds: v.SetupTimeoutSeconds, PendingAudioSeconds: v.PendingAudioSeconds})
	}
	for i := range c.Chat {
		v := &c.Chat[i]
		out.Chat = append(out.Chat, voicegateway.ChatConfig{Label: v.Label, ModelConfig: voiceModelToRuntime(&v.ModelConfig)})
	}
	for i := range c.LocalStack {
		v := &c.LocalStack[i]
		out.LocalStack = append(out.LocalStack, voicegateway.LocalStackConfig{Label: v.Label, TextOnly: v.TextOnly,
			LLM: voiceModelToRuntime(&v.LLM),
			ASR: voicegateway.LocalStackASRConfig{Engine: voicegateway.LocalStackASREngine(v.ASR.Engine), Provider: v.ASR.Provider, Remote: v.ASR.Remote, Model: v.ASR.Model},
			TTS: voicegateway.LocalStackTTSConfig{Engine: voicegateway.LocalStackTTSEngine(v.TTS.Engine), Remote: v.TTS.Remote, Model: v.TTS.Model, Voice: v.TTS.Voice},
		})
	}
	return out
}

func voiceModelToRuntime(c *data.ModelConfig) voicegateway.ModelConfig {
	return voicegateway.ModelConfig{LlamaCPPServer: c.LlamaCPPServer, Provider: c.Provider, Remote: c.Remote, Model: c.Model, APIKeyName: c.APIKeyName}
}
