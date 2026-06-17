// Client-side OAuth 2.0 DTOs.

package oauth

// ClientConfig holds OAuth 2.0 token endpoint configuration for one authorization-code client.
type ClientConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	RedirectURI  string
}
