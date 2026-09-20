// Package frontend embeds the built frontend assets and provider logos.
package frontend

import (
	"embed"
	"io/fs"
)

//go:embed dist

// Files contains the built frontend assets.
var Files embed.FS

//go:embed logos

// Logos contains provider logo SVGs and PNGs served at /logos/.
var Logos embed.FS

// LogoFile resolves the embedded logo filename for a provider, preferring
// SVG. It returns an empty string when no logo asset exists.
func LogoFile(provider string) string {
	for _, name := range []string{provider + ".svg", provider + ".png"} {
		if _, err := fs.Stat(Logos, "logos/"+name); err == nil {
			return name
		}
	}
	return ""
}
