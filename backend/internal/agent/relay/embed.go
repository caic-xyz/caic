// Package relay embeds the version-specific Python relay scripts used inside containers.
package relay

import _ "embed"

// Script is the v1 Python relay that keeps coding agents alive across SSH disconnects.
//
//go:embed relay.py
var Script []byte

// ScriptV2 is the latent v2 Python relay with canonical agent framing.
//
//go:embed relay_v2.py
var ScriptV2 []byte
