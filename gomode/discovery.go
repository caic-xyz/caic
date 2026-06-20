// Package gomode defines Go Mode service discovery and voice token contracts.
package gomode

// settingsPath is the well-known discovery document path for the Go Mode root
// settings. It is a static, publicly readable bootstrap manifest, not a REST
// API, so it lives under /.well-known/ per RFC 8615 rather than /api/.
const settingsPath = "/.well-known/gomode.json"

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
	ToolGroups    []ToolGroup          `json:"toolGroups"`
	VoiceGateway  VoiceGatewaySettings `json:"voiceGateway"`
}

// ToolGroup describes one MCP tool group the shell can activate as a skill:
// a static set of tools plus instructions behind one endpoint. The shell
// discovers every group's name and description up front and loads a group's
// tools on demand (progressive disclosure), so the active tool set changes by
// activating groups, not by mutating a group's tools.
type ToolGroup struct {
	Name            string              `json:"name"`
	Description     string              `json:"description,omitempty"`
	Endpoint        string              `json:"endpoint"`
	ProtocolVersion string              `json:"protocolVersion"` // MCP server version, always "2026-07-28". TODO: Is that useful?
	AuthRequired    bool                `json:"authRequired"`
	Activation      ToolGroupActivation `json:"activation,omitzero"`
}

// ToolGroupActivation carries hints the shell matches against current context to
// decide when to load a group's tools. Matching is on-device: the service never
// receives the user's context or location.
type ToolGroupActivation struct {
	Routes       []string   `json:"routes,omitempty,omitzero"` // TODO: Will probably remove.
	LocationTags []Location `json:"locationTags,omitempty,omitzero"`
	Keywords     []string   `json:"keywords,omitempty,omitzero"`
}

// Location is a location triggered by a physical location, SSID or other means.
type Location struct {
	Latitude  float64 `json:"latitude,omitzero"`
	Longitude float64 `json:"longitude,omitzero"`
	SSID      string  `json:"ssid,omitzero"`
}

// VoiceGatewaySettings describes the preferred voice gateway for this service.
type VoiceGatewaySettings struct {
	Required      bool   `json:"required"`
	URL           string `json:"url,omitempty"`
	AuthRequired  bool   `json:"authRequired,omitempty"`
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
}
