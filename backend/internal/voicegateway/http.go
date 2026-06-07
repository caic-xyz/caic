// HTTP handlers for the voice gateway API.

package voicegateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// MediaBridge is the WebRTC media transport used by the voice gateway API.
type MediaBridge interface {
	HandleOffer(ctx context.Context, sdp string) (sdpAnswer, sessionID string, err error)
	Close(sessionID string)
}

// NewHandler returns a reusable voice gateway HTTP handler.
func NewHandler(
	cfg *Config,
	bridge MediaBridge,
) (http.Handler, error) {
	if cfg == nil {
		return nil, errors.New("voice gateway config is required")
	}
	h := &handler{
		cfg:    cfg,
		bridge: bridge,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/voicegateway/v1/voice/health", h.handleHealth)
	mux.HandleFunc("POST /api/voicegateway/v1/voice/rtc/offer", h.handleOffer)
	mux.HandleFunc("POST /api/voicegateway/v1/voice/rtc/{sessionID}", h.handleClose)
	return mux, nil
}

type handler struct {
	cfg    *Config
	bridge MediaBridge
}

// HealthResp is returned by GET /api/voicegateway/v1/voice/health.
type HealthResp struct {
	Status string `json:"status"`
}

// OfferReq starts a voice gateway WebRTC signaling session.
type OfferReq struct {
	SDP     string               `json:"sdp"`
	Service ServiceAuthorization `json:"service"`
}

// OfferResp returns the WebRTC SDP answer and gateway session ID.
type OfferResp struct {
	SDP       string `json:"sdp"`
	SessionID string `json:"sessionID"`
}

// CloseSessionResp is returned after closing a voice gateway session.
type CloseSessionResp struct {
	Status string `json:"status"`
}

// ErrorResponse is the JSON envelope for voice gateway error responses.
type ErrorResponse struct {
	Error   ErrorDetails   `json:"error"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorDetails holds the code and message within an error response.
type ErrorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ServiceAuthorization authorizes one service-bound voice session.
type ServiceAuthorization struct {
	Kind       string `json:"kind"`
	InstanceID string `json:"instanceID"`
	BaseURL    string `json:"baseURL"`
	Token      string `json:"token"`
}

func (h *handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResp{Status: "ok"})
}

func (h *handler) handleOffer(w http.ResponseWriter, r *http.Request) {
	var req OfferReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if req.SDP == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "sdp is required")
		return
	}
	if err := validateServiceAuthorization(req.Service); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if err := verifyServiceToken(h.cfg, req.Service); err != nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return
	}
	if h.bridge == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "voice bridge unavailable")
		return
	}
	sdpAnswer, sessionID, err := h.bridge.HandleOffer(r.Context(), req.SDP)
	if err != nil {
		slog.ErrorContext(r.Context(), "offer failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "offer failed")
		return
	}
	writeJSON(w, http.StatusOK, OfferResp{SDP: sdpAnswer, SessionID: sessionID})
}

func (h *handler) handleClose(w http.ResponseWriter, r *http.Request) {
	if h.bridge == nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "voice bridge unavailable")
		return
	}
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "sessionID is required")
		return
	}
	h.bridge.Close(sessionID)
	writeJSON(w, http.StatusOK, CloseSessionResp{Status: "closed"})
}

func verifyServiceToken(cfg *Config, s ServiceAuthorization) error {
	for _, issuer := range cfg.TrustedIssuers {
		if issuer.Service != s.Kind || strings.TrimRight(issuer.Issuer, "/") != strings.TrimRight(s.BaseURL, "/") {
			continue
		}
		publicKey, err := ParseServiceSigningPublicKey(issuer.PublicKey)
		if err != nil {
			return err
		}
		claims, err := VerifyServiceScopedToken(s.Token, publicKey, ScopedTokenAudience)
		if err != nil {
			return err
		}
		if claims.ServiceKind != s.Kind {
			return errors.New("scoped token service kind does not match request")
		}
		if claims.ServiceInstanceID != s.InstanceID {
			return errors.New("scoped token service instance does not match request")
		}
		if strings.TrimRight(claims.BackendOrigin, "/") != strings.TrimRight(s.BaseURL, "/") {
			return errors.New("scoped token backend origin does not match request")
		}
		return nil
	}
	return errors.New("no trusted issuer configured for service")
}

func validateServiceAuthorization(s ServiceAuthorization) error {
	if s.Kind == "" {
		return errors.New("service.kind is required")
	}
	if s.InstanceID == "" {
		return errors.New("service.instanceID is required")
	}
	if s.BaseURL == "" {
		return errors.New("service.baseURL is required")
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("service.baseURL must be an http:// or https:// URL")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("service.baseURL must not contain a path")
	}
	if s.Token == "" {
		return errors.New("service.token is required")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("json encode", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Error: ErrorDetails{
			Code:    code,
			Message: message,
		},
	})
}
