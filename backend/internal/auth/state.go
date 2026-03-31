// CSRF state cookie helpers for the OAuth 2.0 flow.

package auth

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// StateCookieName is the name of the CSRF state cookie.
const StateCookieName = "caic_oauth_state"

// GenerateState returns a 16-byte random hex string.
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SignState returns "state.hex(HMAC-SHA256(secret, state))".
func SignState(state string, secret []byte) string {
	return state + "." + hmacSHA256(secret, state)
}

// ValidateState splits cookie value on ".", re-computes HMAC, returns
// the bare state string. Returns ("", false) on any mismatch.
func ValidateState(cookie string, secret []byte) (string, bool) {
	dot := strings.LastIndex(cookie, ".")
	if dot < 0 {
		return "", false
	}
	state := cookie[:dot]
	sig := cookie[dot+1:]
	expected := hmacSHA256(secret, state)
	if sig != expected {
		return "", false
	}
	return state, true
}
