// Package frontend embeds the built frontend assets and provider logos.
package frontend

import "embed"

//go:embed dist

// Files contains the built frontend assets.
var Files embed.FS

//go:embed logos/*.svg

// Logos contains provider logo SVGs served at /logos/.
var Logos embed.FS
