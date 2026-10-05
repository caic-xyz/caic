// Package relay embeds the maintained Python relay used for writable task logs inside containers.
package relay

import _ "embed"

// ScriptV2 is the maintained Python relay with canonical agent framing.
//
//go:embed relay_v2.py
var ScriptV2 []byte
