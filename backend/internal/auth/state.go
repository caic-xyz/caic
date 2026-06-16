// CSRF state cookie helpers for the OAuth 2.0 flow.

package auth

import "github.com/caic-xyz/caic/backend/internal/oauth"

// StateCookieName is the name of the CSRF state cookie.
const StateCookieName = "caic_oauth_state"

// GenerateState returns a 16-byte random hex string.
func GenerateState() (string, error) {
	return oauth.GenerateState()
}

// SignState returns "state.hex(HMAC-SHA256(secret, state))".
func SignState(state string, secret []byte) string {
	return oauth.SignState(state, secret)
}

// ValidateState splits cookie value on ".", re-computes HMAC, returns
// the bare state string. Returns ("", false) on any mismatch.
func ValidateState(cookie string, secret []byte) (string, bool) {
	return oauth.ValidateState(cookie, secret)
}
