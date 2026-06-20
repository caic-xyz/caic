// Tests for the voice gateway API HTTP handlers.

package voicegateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/gomode"
)

// fakeMediaBridge is a MediaBridge stub for handler tests.
type fakeMediaBridge struct{}

func (f *fakeMediaBridge) HandleOffer(context.Context, string) (sdpAnswer, sessionID string, err error) {
	return "answer-sdp", "session-1", nil
}

func (f *fakeMediaBridge) Close(string) {}

func TestNewHandler(t *testing.T) {
	t.Parallel()
	t.Run("health", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/voicegateway/v1/voice/health", http.NoBody)
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

	t.Run("offer requires sdp", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(`{}`))
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		assertErrorResponse(t, w, http.StatusBadRequest, "BAD_REQUEST", "sdp is required")
	})

	t.Run("offer requires service identity", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(`{"sdp":"offer"}`))
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(w, req)
		assertErrorResponse(t, w, http.StatusBadRequest, "BAD_REQUEST", "service.kind is required")
	})

	t.Run("offer reports unavailable bridge after valid request", func(t *testing.T) {
		t.Parallel()
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		token, err := gomode.IssueServiceScopedToken(&gomode.ScopedTokenClaims{
			ServiceKind:       "caic",
			ServiceInstanceID: "home",
			BackendOrigin:     "https://caic.example.com",
			Subject:           "user-1",
			Capabilities:      []string{"voice.session"},
			Audience:          gomode.ScopedTokenAudience,
			Expiry:            time.Now().Add(time.Hour),
		}, privateKey)
		if err != nil {
			t.Fatal(err)
		}
		encodedPublicKey, err := gomode.EncodeServiceSigningPublicKey(publicKey)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		body := `{"sdp":"offer","service":{"kind":"caic","instanceID":"home","baseURL":"https://caic.example.com","token":"` + token + `"}}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(body))
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
		assertErrorResponse(t, w, http.StatusBadRequest, "BAD_REQUEST", "voice bridge unavailable")
	})

	t.Run("offer reports unavailable typed nil bridge", func(t *testing.T) {
		t.Parallel()
		cfg, service := testServiceAuth(t)
		var bridge *fakeMediaBridge
		handler, err := NewHandler(&cfg, bridge)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		body := `{"sdp":"offer","service":` + service + `}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(body))
		handler.ServeHTTP(w, req)
		assertErrorResponse(t, w, http.StatusBadRequest, "BAD_REQUEST", "voice bridge unavailable")
	})

	t.Run("offer succeeds with trusted service", func(t *testing.T) {
		t.Parallel()
		cfg, service := testServiceAuth(t)
		handler, err := NewHandler(&cfg, &fakeMediaBridge{})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		body := `{"sdp":"offer","service":` + service + `}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(body))
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var resp OfferResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.SDP != "answer-sdp" || resp.SessionID != "session-1" {
			t.Fatalf("resp = %+v, want answer-sdp/session-1", resp)
		}
	})

	t.Run("offer rejects untrusted service", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		handler, err := NewHandler(&cfg, &fakeMediaBridge{})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		body := `{"sdp":"offer","service":{"kind":"caic","instanceID":"home","baseURL":"https://caic.example.com","token":"dummy"}}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(body))
		handler.ServeHTTP(w, req)
		assertErrorResponse(t, w, http.StatusUnauthorized, "UNAUTHORIZED", "no trusted issuer configured for service")
	})
}

func TestNewEmbeddedHandler(t *testing.T) {
	t.Parallel()
	t.Run("offer accepts sdp without service authorization", func(t *testing.T) {
		t.Parallel()
		handler := NewEmbeddedHandler(func() MediaBridge { return &fakeMediaBridge{} })
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/voicegateway/v1/voice/rtc/offer", strings.NewReader(`{"sdp":"offer"}`))
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var resp OfferResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.SDP != "answer-sdp" || resp.SessionID != "session-1" {
			t.Fatalf("resp = %+v, want answer-sdp/session-1", resp)
		}
	})

	t.Run("health is standalone only", func(t *testing.T) {
		t.Parallel()
		handler := NewEmbeddedHandler(func() MediaBridge { return &fakeMediaBridge{} })
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/voicegateway/v1/voice/health", http.NoBody)
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}

// testServiceAuth returns a config with a trusted issuer and a matching service
// authorization JSON fragment for offer requests.
func testServiceAuth(t *testing.T) (cfg Config, service string) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token, err := gomode.IssueServiceScopedToken(&gomode.ScopedTokenClaims{
		ServiceKind:       "caic",
		ServiceInstanceID: "home",
		BackendOrigin:     "https://caic.example.com",
		Subject:           "user-1",
		Capabilities:      []string{"voice.session"},
		Audience:          gomode.ScopedTokenAudience,
		Expiry:            time.Now().Add(time.Hour),
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encodedPublicKey, err := gomode.EncodeServiceSigningPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	cfg.TrustedIssuers = []TrustedIssuerConfig{{
		Service:   "caic",
		Issuer:    "https://caic.example.com",
		PublicKey: encodedPublicKey,
	}}
	service = `{"kind":"caic","instanceID":"home","baseURL":"https://caic.example.com","token":"` + token + `"}`
	return cfg, service
}

func assertErrorResponse(t *testing.T, w *httptest.ResponseRecorder, status int, code, message string) {
	if w.Code != status {
		t.Fatalf("status = %d, want %d", w.Code, status)
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Code != code {
		t.Fatalf("error.code = %q, want %q", resp.Error.Code, code)
	}
	if resp.Error.Message != message {
		t.Fatalf("error.message = %q, want %q", resp.Error.Message, message)
	}
}

func newTestPublicKey(t *testing.T) string {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := gomode.EncodeServiceSigningPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
