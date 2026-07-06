// HTTP handlers for the embedded WebRTC voice bridge.

package server

import (
	"net/http"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/gomode/voicegateway"
	"github.com/caic-xyz/caic/gomode/voicegateway/voicertc"
)

// voiceHandlers owns the embedded voice gateway HTTP adapter.
//
// bridge is nil when the voice gateway is disabled or delegated to an external
// gateway. In that state metadata reports disabled/external mode and embedded
// RTC routes return "voice bridge unavailable".
type voiceHandlers struct {
	bridge  *voicertc.Bridge
	gateway VoiceGatewayConfig
}

func (h *voiceHandlers) handler() http.Handler {
	return voicegateway.NewEmbeddedHandler(h.mediaBridge)
}

func (h *voiceHandlers) metadata() v1.VoiceGatewayMetadata {
	cfg := h.gateway
	if cfg.Mode == "" {
		if h.bridge != nil {
			cfg.Mode = VoiceGatewayModeEmbedded
		} else {
			cfg.Mode = VoiceGatewayModeDisabled
		}
	}
	switch cfg.Mode {
	case VoiceGatewayModeEmbedded:
		if h.bridge == nil {
			return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
		}
		return v1.VoiceGatewayMetadata{
			Mode:         v1.VoiceGatewayModeEmbedded,
			AuthRequired: false,
			Capabilities: []string{"voice.gatewayGeminiLive", "voice.rtcDiagnostics"},
		}
	case VoiceGatewayModeExternal:
		return v1.VoiceGatewayMetadata{
			Mode:         v1.VoiceGatewayModeExternal,
			URL:          cfg.URL,
			AuthRequired: true,
			Capabilities: []string{"voice.gatewayGeminiLive", "voice.rtcDiagnostics"},
		}
	default:
		return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
	}
}

func (h *voiceHandlers) mediaBridge() voicegateway.MediaBridge {
	if h.bridge == nil {
		return nil
	}
	return h.bridge
}
