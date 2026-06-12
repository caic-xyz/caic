// Package gomode serves Go Mode service compatibility settings.
package gomode

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Settings is the service compatibility document consumed by Go Mode Android.
type Settings struct {
	Service        string           `json:"service"`
	ServiceVersion string           `json:"serviceVersion,omitempty"`
	APIVersion     int              `json:"apiVersion"`
	WebShell       WebShellSettings `json:"webShell"`
}

// ErrorResponse is the standard JSON error envelope for Go Mode SDK clients.
type ErrorResponse struct {
	Error   ErrorBody      `json:"error"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorBody describes a Go Mode API error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WebShellSettings describes the hosted frontend's native-shell contract.
type WebShellSettings struct {
	BridgeVersion int                  `json:"bridgeVersion"`
	MCP           MCPSettings          `json:"mcp"`
	VoiceGateway  VoiceGatewaySettings `json:"voiceGateway"`
}

// MCPSettings describes the service MCP endpoint Android can use for tools and resources.
type MCPSettings struct {
	Endpoint        string `json:"endpoint"`
	ProtocolVersion string `json:"protocolVersion"`
	AuthRequired    bool   `json:"authRequired"`
}

// VoiceGatewaySettings describes the preferred voice gateway for this service.
type VoiceGatewaySettings struct {
	Required      bool   `json:"required"`
	URL           string `json:"url,omitempty"`
	AuthRequired  bool   `json:"authRequired,omitempty"`
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
}

// NewHandler returns a Go Mode service metadata HTTP handler.
func NewHandler(settings *Settings) (http.Handler, error) {
	if settings == nil {
		return nil, errors.New("go mode settings are required")
	}
	cloned := cloneSettings(settings)
	h := &handler{settings: cloned}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/gomode/v1/settings", h.handleSettings)
	return mux, nil
}

func cloneSettings(settings *Settings) Settings {
	return *settings
}

type handler struct {
	settings Settings
}

func (h *handler) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.settings)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("gomode json encode", "err", err)
	}
}
