// Package gomode defines Go Mode service discovery and voice token contracts.
package gomode

// settingsPath is the well-known discovery document path for the Go Mode root
// settings. It is a static, publicly readable bootstrap manifest, not a REST
// API, so it lives under /.well-known/ per RFC 8615 rather than /api/.
const settingsPath = "/.well-known/gomode.json"
