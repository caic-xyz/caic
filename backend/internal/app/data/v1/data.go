// Package v1 defines version 1 of the independent app startup settings file schema.
package v1

// Settings stores session and OAuth signing secrets and the Tailscale API
// access token an administrator saved.
type Settings struct {
	SessionSecret string `json:"sessionSecret,omitempty"`
	// TODO: Migrate to a oauth section.
	OAuthPrivateKeyPEM string `json:"mcpOAuthPrivateKeyPEM,omitempty"`
	OAuthKeyID         string `json:"mcpOAuthKeyID,omitempty"`
	TailscaleAPIKey    string `json:"tailscaleAPIKey,omitempty"`
}
