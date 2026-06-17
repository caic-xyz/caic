// Client-side OAuth 2.0 DTOs.

package oauth

import "time"

// ClientConfig holds OAuth 2.0 token endpoint configuration for one authorization-code client.
type ClientConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	RedirectURI  string
}

// Token holds an OAuth 2.0 access token and associated metadata.
type Token struct {
	AccessToken  string
	TokenType    string // e.g., "Bearer"; always set by the constructor
	RefreshToken string
	Expiry       time.Time // zero means no expiry
}
