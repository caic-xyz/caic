// External voice gateway token minting and Go Mode voice gateway metadata.

package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/gomode"
	"github.com/maruel/gomode/oauth"
	"github.com/maruel/gomode/voicegateway"
	voicev1 "github.com/maruel/gomode/voicegateway/api/v1"
	"github.com/maruel/gomode/voicegateway/voicertc"
)

// voiceTokenIssuer mints a narrow OAuth access token for the external gateway.
type voiceTokenIssuer interface {
	IssueNarrowToken(user oauth.User, audience, scope string, ttl time.Duration) (string, error)
}

// voiceTokenHandler serves the external gateway token endpoint. The route is
// registered only in external gateway mode, so the handler assumes it.
func voiceTokenHandler(gateway *VoiceGatewayConfig, issuer voiceTokenIssuer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		subject := "anonymous"
		if user, ok := auth.UserFromContext(r.Context()); ok {
			subject = user.ID
		}
		token, err := issueVoiceToken(gateway, issuer, subject)
		if err != nil {
			slog.ErrorContext(r.Context(), "issue voice gateway token", "err", err)
			http.Error(w, "voice token unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(voicev1.ServiceAuthorization{
			Kind:       goModeServiceCaic,
			InstanceID: gateway.InstanceID,
			BaseURL:    gateway.Issuer,
			Token:      token,
		}); err != nil {
			slog.WarnContext(r.Context(), "write voice gateway token", "err", err)
		}
	}
}

// issueVoiceToken mints the external gateway token for subject: a narrow OAuth
// 2.0 access token when configured, otherwise the transitional scoped Ed25519
// token.
func issueVoiceToken(gateway *VoiceGatewayConfig, issuer voiceTokenIssuer, subject string) (string, error) {
	if gateway.TokenMode == VoiceTokenModeOAuth {
		if issuer == nil {
			return "", errors.New("OAuth authorization server is not configured")
		}
		return issuer.IssueNarrowToken(oauth.User{ID: subject, Username: subject}, gomode.ScopedTokenAudience, voicegateway.DefaultVoiceScope, 5*time.Minute)
	}
	claims := &gomode.ScopedTokenClaims{
		ServiceKind:       goModeServiceCaic,
		ServiceInstanceID: gateway.InstanceID,
		BackendOrigin:     gateway.Issuer,
		Subject:           subject,
		Capabilities:      []string{voicegateway.DefaultVoiceScope},
		Audience:          gomode.ScopedTokenAudience,
		Expiry:            time.Now().Add(5 * time.Minute),
	}
	return gomode.IssueServiceScopedToken(claims, gateway.SigningKey)
}

// voiceGatewayMetadata reports the gateway mode the Go Mode manifest advertises.
// Embedded mode downgrades to disabled when caic has no in-process bridge.
func voiceGatewayMetadata(gateway *VoiceGatewayConfig, bridge *voicertc.Bridge) v1.VoiceGatewayMetadata {
	mode := gateway.Mode
	if mode == "" {
		mode = VoiceGatewayModeDisabled
		if bridge != nil {
			mode = VoiceGatewayModeEmbedded
		}
	}
	switch mode {
	case VoiceGatewayModeEmbedded:
		if bridge == nil {
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
			URL:          gateway.URL,
			AuthRequired: true,
			Capabilities: []string{"voice.gatewayGeminiLive", "voice.rtcDiagnostics"},
		}
	default:
		return v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeDisabled}
	}
}
