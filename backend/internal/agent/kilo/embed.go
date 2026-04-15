// Embeds the bridge script for Kilo Code integration.

package kilo

import _ "embed"

// BridgeScript is the Python bridge that translates between relay stdin/stdout
// NDJSON and kilo serve HTTP+SSE.
//
//go:embed bridge.py
var BridgeScript []byte
