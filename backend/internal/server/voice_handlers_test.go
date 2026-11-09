// Tests and benchmarks external voice gateway token minting.

package server

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/gomodedev/gomode"
	"github.com/gomodedev/gomode/oauth"
	"github.com/gomodedev/gomode/voicegateway"
	voicev1 "github.com/gomodedev/gomode/voicegateway/api/v1"
)

type fakeVoiceTokenIssuer struct {
	user     oauth.User
	audience string
	scope    string
	ttl      time.Duration
	token    string
	err      error
}

func (f *fakeVoiceTokenIssuer) IssueNarrowToken(user oauth.User, audience, scope string, ttl time.Duration) (string, error) {
	f.user, f.audience, f.scope, f.ttl = user, audience, scope, ttl
	return f.token, f.err
}

func externalVoiceGateway(tokenMode VoiceTokenMode) *VoiceGatewayConfig {
	cfg := &VoiceGatewayConfig{
		Mode:       VoiceGatewayModeExternal,
		Issuer:     "https://caic.example.com",
		InstanceID: "caic-main",
		TokenMode:  tokenMode,
	}
	if tokenMode != VoiceTokenModeOAuth {
		cfg.SigningKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	}
	return cfg
}

func fetchVoiceToken(t *testing.T, gateway *VoiceGatewayConfig, issuer voiceTokenIssuer) (*httptest.ResponseRecorder, voicev1.ServiceAuthorization) {
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/voice/token", http.NoBody)
	voiceTokenHandler(gateway, issuer)(w, req)
	var service voicev1.ServiceAuthorization
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&service); err != nil {
			t.Fatalf("decode service authorization: %v", err)
		}
	}
	return w, service
}

func TestVoiceTokenHandlerOAuth(t *testing.T) {
	t.Run("fresh narrow authority after re-enable", func(t *testing.T) {
		t.Parallel()
		s, _, original, _ := newMCPOAuthLifecycleRouter(t)
		if err := s.authStore.SetDisabled(original.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := s.authStore.SetDisabled(original.ID, false); err != nil {
			t.Fatal(err)
		}
		fresh, _ := s.authStore.ActiveUser(original.ID)
		for _, user := range []auth.User{fresh, original} {
			req := httptest.NewRequestWithContext(auth.NewContext(t.Context(), &user), http.MethodGet, "/voice/token", http.NoBody)
			response := httptest.NewRecorder()
			voiceTokenHandler(externalVoiceGateway(VoiceTokenModeOAuth), s.oauthServer)(response, req)
			if user.AccountEpoch == fresh.AccountEpoch {
				if response.Code != http.StatusOK {
					t.Fatalf("fresh authority failed: %d %s", response.Code, response.Body.String())
				}
			} else if response.Code != http.StatusInternalServerError {
				t.Fatalf("stale authority issued narrow token: %d", response.Code)
			}
		}
	})

	t.Run("retains authenticated account generation", func(t *testing.T) {
		t.Parallel()
		issuer := &fakeVoiceTokenIssuer{token: "token"}
		req := httptest.NewRequestWithContext(auth.NewContext(t.Context(), &auth.User{ID: "usr_renewed", AccountEpoch: 2}), http.MethodGet, "/voice/token", http.NoBody)
		response := httptest.NewRecorder()
		voiceTokenHandler(externalVoiceGateway(VoiceTokenModeOAuth), issuer)(response, req)
		if response.Code != http.StatusOK || issuer.user.Authority != "2" {
			t.Fatalf("captured voice authority: %d %+v", response.Code, issuer.user)
		}
	})

	t.Parallel()
	issuer := &fakeVoiceTokenIssuer{token: "header.payload.signature"}
	w, service := fetchVoiceToken(t, externalVoiceGateway(VoiceTokenModeOAuth), issuer)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if service.Token != issuer.token || service.Kind != goModeServiceCaic || service.BaseURL != "https://caic.example.com" {
		t.Fatalf("service = %+v", service)
	}
	if issuer.audience != gomode.ScopedTokenAudience || issuer.scope != voicegateway.DefaultVoiceScope {
		t.Fatalf("issued audience/scope = %q/%q", issuer.audience, issuer.scope)
	}
	if issuer.ttl != 5*time.Minute {
		t.Fatalf("issued TTL = %s, want 5m", issuer.ttl)
	}
	if issuer.user.ID != "anonymous" {
		t.Fatalf("issued subject = %q, want anonymous without a session user", issuer.user.ID)
	}
}

func TestVoiceTokenHandlerOAuthRequiresIssuer(t *testing.T) {
	t.Parallel()
	w, _ := fetchVoiceToken(t, externalVoiceGateway(VoiceTokenModeOAuth), nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestVoiceTokenHandlerScoped(t *testing.T) {
	t.Parallel()
	cfg := externalVoiceGateway(VoiceTokenModeScoped)
	w, service := fetchVoiceToken(t, cfg, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	publicKey := ed25519.PublicKey(cfg.SigningKey[ed25519.SeedSize:])
	claims, err := gomode.VerifyServiceScopedToken(service.Token, publicKey, gomode.ScopedTokenAudience)
	if err != nil {
		t.Fatalf("VerifyServiceScopedToken: %v", err)
	}
	if claims.ServiceKind != goModeServiceCaic || claims.ServiceInstanceID != "caic-main" {
		t.Fatalf("claims = %+v", claims)
	}
	if len(claims.Capabilities) != 1 || claims.Capabilities[0] != voicegateway.DefaultVoiceScope {
		t.Fatalf("capabilities = %v, want [%s]", claims.Capabilities, voicegateway.DefaultVoiceScope)
	}
}

func BenchmarkExternalVoiceToken(b *testing.B) {
	handler := voiceTokenHandler(&VoiceGatewayConfig{
		Mode:       VoiceGatewayModeExternal,
		Issuer:     "https://caic.example.com",
		InstanceID: "caic-main",
		SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}, nil)
	r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/voice/token", http.NoBody)
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			b.Fatalf("status = %d", w.Code)
		}
	}
}
