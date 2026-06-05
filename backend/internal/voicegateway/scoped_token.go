// Scoped service tokens accepted by the voice gateway.

package voicegateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ScopedTokenAudience is the expected audience for service-issued gateway tokens.
const ScopedTokenAudience = "voice-gateway"

// ScopedTokenClaims are the service-issued claims required to create a voice session.
type ScopedTokenClaims struct {
	ServiceKind       string    `json:"serviceKind"`
	ServiceInstanceID string    `json:"serviceInstanceID"`
	BackendOrigin     string    `json:"backendOrigin"`
	Subject           string    `json:"sub"`
	Capabilities      []string  `json:"capabilities"`
	Audience          string    `json:"aud"`
	Expiry            time.Time `json:"exp"`
}

// EncodeServiceSigningPublicKey returns the importable form of an Ed25519 public key.
func EncodeServiceSigningPublicKey(k ed25519.PublicKey) (string, error) {
	if len(k) != ed25519.PublicKeySize {
		return "", fmt.Errorf("ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(k), nil
}

// ParseServiceSigningPublicKey parses an imported Ed25519 public key.
func ParseServiceSigningPublicKey(s string) (ed25519.PublicKey, error) {
	encoded := strings.TrimPrefix(s, "ed25519:")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode ed25519 public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// IssueServiceScopedToken signs a short-lived service-scoped gateway token.
func IssueServiceScopedToken(c *ScopedTokenClaims, k ed25519.PrivateKey) (string, error) {
	if len(k) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("ed25519 private key must be %d bytes", ed25519.PrivateKeySize)
	}
	if err := c.Validate(ScopedTokenAudience); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal scoped token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(k, []byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// VerifyServiceScopedToken validates a service-scoped gateway token.
func VerifyServiceScopedToken(token string, k ed25519.PublicKey, audience string) (*ScopedTokenClaims, error) {
	if len(k) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid scoped token format")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode scoped token signature: %w", err)
	}
	if !ed25519.Verify(k, []byte(parts[0]), signature) {
		return nil, errors.New("invalid scoped token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode scoped token: %w", err)
	}
	var claims ScopedTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse scoped token: %w", err)
	}
	if err := claims.Validate(audience); err != nil {
		return nil, err
	}
	return &claims, nil
}

// Validate returns an error if c cannot authorize a service-bound voice session.
func (c *ScopedTokenClaims) Validate(audience string) error {
	var errs []error
	if c.ServiceKind == "" {
		errs = append(errs, errors.New("serviceKind is required"))
	}
	if c.ServiceInstanceID == "" {
		errs = append(errs, errors.New("serviceInstanceID is required"))
	}
	if c.BackendOrigin == "" {
		errs = append(errs, errors.New("backendOrigin is required"))
	} else if err := validateTokenOrigin(c.BackendOrigin); err != nil {
		errs = append(errs, err)
	}
	if c.Subject == "" {
		errs = append(errs, errors.New("sub is required"))
	}
	if c.Audience != audience {
		errs = append(errs, fmt.Errorf("audience must be %q, got %q", audience, c.Audience))
	}
	if c.Expiry.IsZero() {
		errs = append(errs, errors.New("expiry is required"))
	} else if time.Now().After(c.Expiry) {
		errs = append(errs, errors.New("scoped token expired"))
	}
	return errors.Join(errs...)
}

func validateTokenOrigin(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return fmt.Errorf("backendOrigin is not a valid URL: %q", value)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("backendOrigin must use http:// or https://, got %q", value)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("backendOrigin must not contain a path: %q", value)
	}
	return nil
}
