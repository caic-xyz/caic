// Package claudecode embeds the widget plugin deployed to containers.
package claudecode

import "embed"

// WidgetPlugin is the embedded widget plugin directory tree deployed to
// containers alongside the relay script. It provides the show_widget MCP
// tool and the widget design skill with progressive-disclosure references.
//
//go:embed all:widget-plugin
var WidgetPlugin embed.FS
