// JWT session management: issue and validate HS256 tokens using stdlib only.

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// jwtHeader is the fixed base64url-encoded JWT header for HS256.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// jwtPayload is the JSON structure for the JWT payload.
type jwtPayload struct {
	UserID   string `json:"uid"`
	Provider string `json:"prv"`
	Username string `json:"usr"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
}

// IssueToken creates a signed JWT for the user. ttl is typically 30 days.
func IssueToken(u *User, secret []byte, ttl time.Duration) (string, error) {
	now := time.Now()
	payload := jwtPayload{
		UserID:   u.ID,
		Provider: string(u.Provider),
		Username: u.Username,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(ttl).Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal jwt payload: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sigInput := jwtHeader + "." + encodedPayload
	sig := hmacSHA256(secret, sigInput)
	return sigInput + "." + sig, nil
}

// ValidateToken parses and verifies a JWT. Returns Claims or an error.
func ValidateToken(token string, secret []byte) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt format")
	}
	sigInput := parts[0] + "." + parts[1]
	expectedSig := hmacSHA256(secret, sigInput)
	if !hmac.Equal([]byte(expectedSig), []byte(parts[2])) {
		return nil, errors.New("invalid jwt signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode jwt payload: %w", err)
	}
	var payload jwtPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("parse jwt payload: %w", err)
	}
	if time.Now().Unix() > payload.Expiry {
		return nil, errors.New("jwt expired")
	}
	return &Claims{
		UserID:   payload.UserID,
		Provider: forge.Kind(payload.Provider),
		Username: payload.Username,
		IssuedAt: time.Unix(payload.IssuedAt, 0),
		Expiry:   time.Unix(payload.Expiry, 0),
	}, nil
}

func hmacSHA256(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
