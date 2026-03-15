// Shared widget MCP server script embedded for deployment to containers.
package agent

import _ "embed"

// WidgetMCPServerScript is the Python MCP server script that exposes the
// show_widget tool. It is deployed to containers by backends that support
// widget rendering (claude, codex).
//
//go:embed claude/widget-plugin/mcp_server.py
var WidgetMCPServerScript []byte
