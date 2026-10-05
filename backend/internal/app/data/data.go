// Package data defines the independent app startup settings file schema.
package data

// Settings stores session and OAuth signing secrets.
type Settings struct {
	SessionSecret string `json:"sessionSecret,omitempty"`
	// TODO: Migrate to a oauth section.
	OAuthPrivateKeyPEM string `json:"mcpOAuthPrivateKeyPEM,omitempty"`
	OAuthKeyID         string `json:"mcpOAuthKeyID,omitempty"`
}
