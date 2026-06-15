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
	goModeTasksGroupName          = "tasks"
	goModeTasksGroupDescription   = "caic coding-agent task management: create, monitor, answer, and control tasks."
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
			ToolGroups:    newGoModeToolGroups(authRequired),
			VoiceGateway:  newGoModeVoiceGatewaySettings(voice),
		},
	}
}

// newGoModeToolGroups returns the MCP tool groups the shell can activate. caic
// exposes a single group today; the list shape lets the shell host more groups
// (skills) without a manifest contract change.
func newGoModeToolGroups(authRequired bool) []gomode.ToolGroup {
	return []gomode.ToolGroup{
		{
			Name:            goModeTasksGroupName,
			Description:     goModeTasksGroupDescription,
			Endpoint:        goModeMCPEndpoint,
			ProtocolVersion: mcp.ProtocolVersion,
			AuthRequired:    authRequired,
		},
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
