// WebRTC voice bridge HTTP handlers.

package server

import (
	"net/http"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

type voiceHandlers struct {
	Bridge  *voicertc.Bridge
	Gateway VoiceGatewayConfig
}

func (h *voiceHandlers) handler() http.Handler {
	return voicegateway.NewEmbeddedHandler(h.mediaBridge)
}

func (h *voiceHandlers) metadata() v1.VoiceGatewayMetadata {
	cfg := h.Gateway
	if cfg.Mode == "" {
		if h.Bridge != nil {
			cfg.Mode = VoiceGatewayModeEmbedded
		} else {
			cfg.Mode = VoiceGatewayModeDisabled
		}
	}
	switch cfg.Mode {
	case VoiceGatewayModeEmbedded:
		if h.Bridge == nil {
			return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
		}
		return v1.VoiceGatewayMetadata{
			Mode:         v1.VoiceGatewayModeEmbedded,
			AuthRequired: false,
			Capabilities: []string{"voice.gatewayGeminiLive"},
		}
	case VoiceGatewayModeExternal:
		return v1.VoiceGatewayMetadata{
			Mode:         v1.VoiceGatewayModeExternal,
			URL:          cfg.URL,
			AuthRequired: true,
			Capabilities: []string{"voice.gatewayGeminiLive"},
		}
	default:
		return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
	}
}

func (h *voiceHandlers) mediaBridge() voicegateway.MediaBridge {
	if h.Bridge == nil {
		return nil
	}
	return h.Bridge
}
