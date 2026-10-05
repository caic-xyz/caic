// Explicit projections between persisted TOML voice settings and gateway runtime config.

package main

import (
	"github.com/caic-xyz/caic/backend/internal/config/data"
	"github.com/maruel/gomode/voicegateway"
)

func voiceConfigToRuntime(c *data.VoiceConfig) voicegateway.Config {
	var issuers []voicegateway.TrustedIssuerConfig
	if c.TrustedIssuers != nil {
		issuers = make([]voicegateway.TrustedIssuerConfig, len(c.TrustedIssuers))
	}
	for i := range c.TrustedIssuers {
		v := &c.TrustedIssuers[i]
		issuers[i] = voicegateway.TrustedIssuerConfig{
			Service: v.Service, Issuer: v.Issuer, PublicKey: v.PublicKey,
			OAuth: v.OAuth, Audience: v.Audience, Scope: v.Scope,
		}
	}
	return voicegateway.Config{
		Model: c.Model, Backend: c.Backend, TrustedIssuers: issuers,
		Server: voicegateway.ServerConfig{HTTP: c.Server.HTTP, WebRTCUDPPort: c.Server.WebRTCUDPPort},
		LocalStack: voicegateway.LocalStackConfig{
			ASR: voicegateway.LocalStackASRConfig{
				Engine:   voicegateway.LocalStackASREngine(c.LocalStack.ASR.Engine),
				Provider: c.LocalStack.ASR.Provider, Remote: c.LocalStack.ASR.Remote, Model: c.LocalStack.ASR.Model,
			},
			LLM: voicegateway.LocalStackLLMConfig{
				Provider: c.LocalStack.LLM.Provider, Remote: c.LocalStack.LLM.Remote,
				Model: c.LocalStack.LLM.Model, APIKeyName: c.LocalStack.LLM.APIKeyName,
			},
			TTS: voicegateway.LocalStackTTSConfig{
				Engine: voicegateway.LocalStackTTSEngine(c.LocalStack.TTS.Engine),
				Remote: c.LocalStack.TTS.Remote, Model: c.LocalStack.TTS.Model, Voice: c.LocalStack.TTS.Voice,
			},
		},
	}
}

func voiceConfigToData(c *voicegateway.Config) data.VoiceConfig {
	var issuers []data.TrustedIssuerConfig
	if c.TrustedIssuers != nil {
		issuers = make([]data.TrustedIssuerConfig, len(c.TrustedIssuers))
	}
	for i := range c.TrustedIssuers {
		v := &c.TrustedIssuers[i]
		issuers[i] = data.TrustedIssuerConfig{
			Service: v.Service, Issuer: v.Issuer, PublicKey: v.PublicKey,
			OAuth: v.OAuth, Audience: v.Audience, Scope: v.Scope,
		}
	}
	return data.VoiceConfig{
		Model: c.Model, Backend: c.Backend, TrustedIssuers: issuers,
		Server: data.ServerConfig{HTTP: c.Server.HTTP, WebRTCUDPPort: c.Server.WebRTCUDPPort},
		LocalStack: data.LocalStackConfig{
			ASR: data.LocalStackASRConfig{
				Engine:   data.LocalStackASREngine(c.LocalStack.ASR.Engine),
				Provider: c.LocalStack.ASR.Provider, Remote: c.LocalStack.ASR.Remote, Model: c.LocalStack.ASR.Model,
			},
			LLM: data.LocalStackLLMConfig{
				Provider: c.LocalStack.LLM.Provider, Remote: c.LocalStack.LLM.Remote,
				Model: c.LocalStack.LLM.Model, APIKeyName: c.LocalStack.LLM.APIKeyName,
			},
			TTS: data.LocalStackTTSConfig{
				Engine: data.LocalStackTTSEngine(c.LocalStack.TTS.Engine),
				Remote: c.LocalStack.TTS.Remote, Model: c.LocalStack.TTS.Model, Voice: c.LocalStack.TTS.Voice,
			},
		},
	}
}
