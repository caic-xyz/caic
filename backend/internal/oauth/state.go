// OAuth CSRF state helpers.

package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

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

// ValidateState splits cookie value on ".", re-computes HMAC, returns the bare
// state string. Returns ("", false) on any mismatch.
func ValidateState(cookie string, secret []byte) (string, bool) {
	dot := strings.LastIndex(cookie, ".")
	if dot < 0 {
		return "", false
	}
	state := cookie[:dot]
	sig := cookie[dot+1:]
	expected := hmacSHA256(secret, state)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return state, true
}

func hmacSHA256(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
