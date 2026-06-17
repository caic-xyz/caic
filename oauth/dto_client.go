// Client-side OAuth 2.0 DTOs: types consumed by the oauthclient package.
// Pure data — no HTTP, no I/O, no behavior beyond Token.Expired().

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

// expiryDelta is the early-expiry buffer applied by Token.Expired.
// A token is treated as expired this long before its actual Expiry to
// avoid failures from clock skew or in-flight requests.
const expiryDelta = 10 * time.Second

// Expired reports whether t is expired, with a built-in early-expiry buffer
// (10s). A zero Expiry means the token never expires.
// Callers that need exact expiry can compare time.Now().After(t.Expiry).
func (t *Token) Expired() bool {
	return !t.Expiry.IsZero() && time.Now().After(t.Expiry.Add(-expiryDelta))
}
