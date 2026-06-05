// HTTP handlers for the voice gateway protocol.

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

// MediaBridge is the WebRTC media transport used by the gateway protocol.
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
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("GET /compat", h.handleCompat)
	mux.HandleFunc("POST /offer", h.handleOffer)
	mux.HandleFunc("POST /sessions/{sessionID}", h.handleClose)
	return mux, nil
}

type handler struct {
	cfg    *Config
	bridge MediaBridge
}

type offerReq struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	SDP             string               `json:"sdp"`
	Service         ServiceAuthorization `json:"service"`
}

// ServiceAuthorization authorizes one service-bound voice session.
type ServiceAuthorization struct {
	Kind       string `json:"kind"`
	InstanceID string `json:"instanceID"`
	BaseURL    string `json:"baseURL"`
	Token      string `json:"token"`
}

func (h *handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) handleCompat(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.cfg.Compatibility())
}

func (h *handler) handleOffer(w http.ResponseWriter, r *http.Request) {
	var req offerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.ProtocolVersion != ProtocolVersion {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported protocolVersion"})
		return
	}
	if req.SDP == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sdp is required"})
		return
	}
	if err := validateServiceAuthorization(req.Service); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := verifyServiceToken(h.cfg, req.Service); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	if h.bridge == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "voice bridge unavailable"})
		return
	}
	sdpAnswer, sessionID, err := h.bridge.HandleOffer(r.Context(), req.SDP)
	if err != nil {
		slog.ErrorContext(r.Context(), "offer failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "offer failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"sdp":       sdpAnswer,
		"sessionID": sessionID,
	})
}

func (h *handler) handleClose(w http.ResponseWriter, r *http.Request) {
	if h.bridge == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "voice bridge unavailable"})
		return
	}
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionID is required"})
		return
	}
	h.bridge.Close(sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"protocolVersion": ProtocolVersion,
		"status":          "closed",
	})
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
