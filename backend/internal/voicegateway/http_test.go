// Tests for the voice gateway protocol HTTP handlers.

package voicegateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewHandler(t *testing.T) {
	t.Parallel()
	t.Run("health", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", http.NoBody)
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp map[string]string
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp["status"] != "ok" {
			t.Fatalf("status field = %q, want ok", resp["status"])
		}
	})

	t.Run("compat", func(t *testing.T) {
		t.Parallel()
		publicKey := newTestPublicKey(t)
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{
			{Service: "caic", Issuer: "https://caic.example.com", PublicKey: publicKey},
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/compat", http.NoBody)
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp Compatibility
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Service != "voice-gateway" {
			t.Fatalf("Service = %q, want voice-gateway", resp.Service)
		}
		if len(resp.ServiceKinds) != 1 || resp.ServiceKinds[0] != "caic" {
			t.Fatalf("ServiceKinds = %v, want [caic]", resp.ServiceKinds)
		}
	})

	t.Run("offer requires protocol version", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/offer", strings.NewReader(`{"sdp":"offer"}`))
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "unsupported protocolVersion") {
			t.Fatalf("body = %q, want unsupported protocolVersion", w.Body.String())
		}
	})

	t.Run("offer requires service identity", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/offer", strings.NewReader(`{"protocolVersion":1,"sdp":"offer"}`))
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "service.kind") {
			t.Fatalf("body = %q, want service.kind error", w.Body.String())
		}
	})

	t.Run("offer reports unavailable bridge after valid request", func(t *testing.T) {
		t.Parallel()
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		token, err := IssueServiceScopedToken(&ScopedTokenClaims{
			ServiceKind:       "caic",
			ServiceInstanceID: "home",
			BackendOrigin:     "https://caic.example.com",
			Subject:           "user-1",
			Capabilities:      []string{"voice.session"},
			Audience:          ScopedTokenAudience,
			Expiry:            time.Now().Add(time.Hour),
		}, privateKey)
		if err != nil {
			t.Fatal(err)
		}
		encodedPublicKey, err := EncodeServiceSigningPublicKey(publicKey)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		body := `{"protocolVersion":1,"sdp":"offer","service":{"kind":"caic","instanceID":"home","baseURL":"https://caic.example.com","token":"` + token + `"}}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/offer", strings.NewReader(body))
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "https://caic.example.com",
			PublicKey: encodedPublicKey,
		}}
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "voice bridge unavailable") {
			t.Fatalf("body = %q, want voice bridge unavailable", w.Body.String())
		}
	})
}

func newTestPublicKey(t *testing.T) string {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeServiceSigningPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
