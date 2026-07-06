// Unit tests for voice gateway API v1 DTO and route declarations.
package v1

import (
	"net/http"
	"testing"
)

func TestRoutes(t *testing.T) {
	t.Parallel()

	t.Run("diagnoseVoiceRTCDeclared", func(t *testing.T) {
		t.Parallel()
		r := routeByName(t, "diagnoseVoiceRTC")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if r.Path != "/api/voicegateway/v1/voice/rtc/{sessionID}/diagnostics" {
			t.Fatalf("path = %q, want voice RTC diagnostics path", r.Path)
		}
		if r.ReqName() != "VoiceRTCDiagnosticsReq" {
			t.Fatalf("request = %q, want VoiceRTCDiagnosticsReq", r.ReqName())
		}
		if r.RespName() != "VoiceRTCDiagnosticsResp" {
			t.Fatalf("response = %q, want VoiceRTCDiagnosticsResp", r.RespName())
		}
	})

	t.Run("closeVoiceRTCDeclared", func(t *testing.T) {
		t.Parallel()
		r := routeByName(t, "closeVoiceRTC")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if r.Path != "/api/voicegateway/v1/voice/rtc/{sessionID}" {
			t.Fatalf("path = %q, want voice RTC close path", r.Path)
		}
		if r.RespName() != "StatusResp" {
			t.Fatalf("response = %q, want StatusResp", r.RespName())
		}
	})
}

func routeByName(t *testing.T, name string) Route {
	for i := range Routes {
		if Routes[i].Name == name {
			return Routes[i]
		}
	}
	t.Fatalf("route %q not found", name)
	return Route{}
}
