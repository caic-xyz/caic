// Tests external voice gateway token issuance selection and scoped verification.

package server

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maruel/gomode"
	"github.com/maruel/gomode/oauth"
	"github.com/maruel/gomode/voicegateway"
	voicev1 "github.com/maruel/gomode/voicegateway/api/v1"
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

func externalVoiceGateway(tokenMode VoiceTokenMode) VoiceGatewayConfig {
	cfg := VoiceGatewayConfig{
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

func fetchVoiceToken(t *testing.T, h *voiceHandlers) (*httptest.ResponseRecorder, voicev1.ServiceAuthorization) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/voice/token", http.NoBody)
	h.tokenHandler(w, req)
	var service voicev1.ServiceAuthorization
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&service); err != nil {
			t.Fatalf("decode service authorization: %v", err)
		}
	}
	return w, service
}

func TestVoiceTokenHandlerOAuth(t *testing.T) {
	t.Parallel()
	issuer := &fakeVoiceTokenIssuer{token: "header.payload.signature"}
	h := &voiceHandlers{gateway: externalVoiceGateway(VoiceTokenModeOAuth), oauthIssuer: issuer}
	w, service := fetchVoiceToken(t, h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if service.Token != issuer.token || service.Kind != goModeServiceCaic || service.BaseURL != h.gateway.Issuer {
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
	h := &voiceHandlers{gateway: externalVoiceGateway(VoiceTokenModeOAuth)}
	w, _ := fetchVoiceToken(t, h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestVoiceTokenHandlerScoped(t *testing.T) {
	t.Parallel()
	h := &voiceHandlers{gateway: externalVoiceGateway(VoiceTokenModeScoped)}
	w, service := fetchVoiceToken(t, h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	publicKey := ed25519.PublicKey(h.gateway.SigningKey[ed25519.SeedSize:])
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

func TestVoiceTokenHandlerDisabled(t *testing.T) {
	t.Parallel()
	h := &voiceHandlers{gateway: VoiceGatewayConfig{Mode: VoiceGatewayModeEmbedded}}
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/voice/token", http.NoBody)
	h.tokenHandler(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 outside external mode", w.Code)
	}
}
