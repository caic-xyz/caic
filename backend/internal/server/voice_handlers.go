// HTTP handlers for the embedded WebRTC bridge and external gateway tokens.

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

// voiceHandlers owns the embedded voice gateway HTTP adapter.
//
// bridge is nil when the voice gateway is disabled or delegated to an external
// gateway. In that state metadata reports disabled/external mode and embedded
// RTC routes return "voice bridge unavailable".
type voiceHandlers struct {
	bridge      *voicertc.Bridge
	gateway     VoiceGatewayConfig
	oauthIssuer voiceTokenIssuer
}

func (h *voiceHandlers) handler() http.Handler {
	return voicegateway.NewEmbeddedHandler(h.mediaBridge)
}

func (h *voiceHandlers) tokenHandler(w http.ResponseWriter, r *http.Request) {
	if h.gateway.Mode != VoiceGatewayModeExternal {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	subject := "anonymous"
	if user, ok := auth.UserFromContext(r.Context()); ok {
		subject = user.ID
	}
	token, err := h.issueToken(subject)
	if err != nil {
		slog.ErrorContext(r.Context(), "issue voice gateway token", "err", err)
		http.Error(w, "voice token unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(voicev1.ServiceAuthorization{
		Kind:       goModeServiceCaic,
		InstanceID: h.gateway.InstanceID,
		BaseURL:    h.gateway.Issuer,
		Token:      token,
	}); err != nil {
		slog.WarnContext(r.Context(), "write voice gateway token", "err", err)
	}
}

// issueToken mints the external gateway token for subject: a narrow OAuth 2.0
// access token when configured, otherwise the transitional scoped Ed25519 token.
func (h *voiceHandlers) issueToken(subject string) (string, error) {
	if h.gateway.TokenMode == VoiceTokenModeOAuth {
		if h.oauthIssuer == nil {
			return "", errors.New("OAuth authorization server is not configured")
		}
		return h.oauthIssuer.IssueNarrowToken(oauth.User{ID: subject, Username: subject}, gomode.ScopedTokenAudience, voicegateway.DefaultVoiceScope, 5*time.Minute)
	}
	claims := &gomode.ScopedTokenClaims{
		ServiceKind:       goModeServiceCaic,
		ServiceInstanceID: h.gateway.InstanceID,
		BackendOrigin:     h.gateway.Issuer,
		Subject:           subject,
		Capabilities:      []string{voicegateway.DefaultVoiceScope},
		Audience:          gomode.ScopedTokenAudience,
		Expiry:            time.Now().Add(5 * time.Minute),
	}
	return gomode.IssueServiceScopedToken(claims, h.gateway.SigningKey)
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
