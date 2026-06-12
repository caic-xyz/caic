// Go Mode service compatibility settings served by caic.

package server

import (
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

const (
	goModeServiceCaic             = "caic"
	goModeAPIVersion              = 1
	goModeBridge                  = 1
	goModeMCPEndpoint             = "/api/caic/v1/mcp"
	goModeEmbeddedVoiceGatewayURL = "/"
)

func newGoModeSettings(voice v1.VoiceGatewayMetadata, authRequired bool) gomode.Settings {
	return gomode.Settings{
		Service:        goModeServiceCaic,
		ServiceVersion: autoupdate.Version,
		APIVersion:     goModeAPIVersion,
		WebShell: gomode.WebShellSettings{
			BridgeVersion: goModeBridge,
			MCP:           newGoModeMCPSettings(authRequired),
			VoiceGateway:  newGoModeVoiceGatewaySettings(voice),
		},
	}
}

func newGoModeMCPSettings(authRequired bool) gomode.MCPSettings {
	return gomode.MCPSettings{
		Endpoint:        goModeMCPEndpoint,
		ProtocolVersion: mcp.ProtocolVersion,
		AuthRequired:    authRequired,
	}
}

func newGoModeVoiceGatewaySettings(voice v1.VoiceGatewayMetadata) gomode.VoiceGatewaySettings {
	settings := gomode.VoiceGatewaySettings{Required: false}
	switch voice.Mode {
	case v1.VoiceGatewayModeDisabled:
	case v1.VoiceGatewayModeEmbedded:
		settings.URL = goModeEmbeddedVoiceGatewayURL
		settings.AuthRequired = voice.AuthRequired
	case v1.VoiceGatewayModeExternal:
		settings.URL = voice.URL
		settings.AuthRequired = voice.AuthRequired
	}
	return settings
}
