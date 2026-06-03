// Tests for server startup configuration validation.

package server

import (
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
)

func TestVoiceGatewayConfig(t *testing.T) {
	t.Parallel()
	t.Run("Validate external requires URL", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{Mode: VoiceGatewayModeExternal}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "voice gateway URL") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Validate external validates URL", func(t *testing.T) {
		t.Parallel()
		cfg := VoiceGatewayConfig{Mode: VoiceGatewayModeExternal, URL: "https://voice.example.com"}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Validate embedded validates static config", func(t *testing.T) {
		t.Parallel()
		gatewayConfig := voicegateway.DefaultConfig()
		gatewayConfig.Server.HTTP = ""
		gatewayConfig.Server.WebRTCUDPPort = -2
		cfg := VoiceGatewayConfig{Mode: VoiceGatewayModeEmbedded, Config: gatewayConfig}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "server.webrtc_udp_port") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
