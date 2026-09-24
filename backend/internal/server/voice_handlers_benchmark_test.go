// Benchmarks issuance of caic-scoped external voice gateway tokens.

package server

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkExternalVoiceToken(b *testing.B) {
	h := &voiceHandlers{gateway: VoiceGatewayConfig{
		Mode:       VoiceGatewayModeExternal,
		Issuer:     "https://caic.example.com",
		InstanceID: "caic-main",
		SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}}
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/voice/token", http.NoBody)
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.tokenHandler(w, r)
		if w.Code != http.StatusOK {
			b.Fatalf("status = %d", w.Code)
		}
	}
}
