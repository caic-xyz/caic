// DPoP sender-constrained token proof-of-possession (RFC 9449).

package oauthserver

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

// DPoPTokenType is the "token_type" value returned for DPoP-bound access tokens.
const DPoPTokenType = "DPoP"

// defaultDPoPMaxAge is the maximum proof age per RFC 9449 §11.1.
const defaultDPoPMaxAge = 5 * time.Minute

// DPoPHeader is the JOSE header of a DPoP proof JWT.
type DPoPHeader struct {
	Typ string    `json:"typ"`
	Alg string    `json:"alg"`
	JWK oauth.JWK `json:"jwk"`
}

// DPoPClaims are the claims in a DPoP proof JWT payload.
type DPoPClaims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	ATH   string `json:"ath,omitempty"`
	Nonce string `json:"nonce,omitempty"`
}

// TokenConfirmation is defined in oauth root package (oauth.TokenConfirmation).

// JWKThumbprint returns the base64url-encoded SHA-256 thumbprint of a oauth.JWK per RFC 7638.
//
// Supported key types: RSA, EC (P-256/P-384/P-521), OKP (Ed25519).
func JWKThumbprint(jwk *oauth.JWK) (string, error) {
	required, err := jwkThumbprintRequired(jwk)
	if err != nil {
		return "", err
	}
	canonical, err := json.Marshal(required)
	if err != nil {
		return "", fmt.Errorf("marshal jwk thumbprint: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// DPoPProof extracts and parses the DPoP proof from the request's DPoP header.
// Returns the parsed header, claims, and nil error on success.
func DPoPProof(r *http.Request) (*DPoPHeader, *DPoPClaims, error) {
	dpop := strings.TrimSpace(r.Header.Get(DPoPTokenType))
	if dpop == "" {
		return nil, nil, errors.New("missing DPOP header")
	}
	parts := strings.Split(dpop, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("invalid dpop proof format")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("decode dpop proof header: %w", err)
	}
	var header DPoPHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, nil, fmt.Errorf("parse dpop proof header: %w", err)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("decode dpop proof payload: %w", err)
	}
	var claims DPoPClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, nil, fmt.Errorf("parse dpop proof claims: %w", err)
	}

	// Verify the JWT signature using the embedded oauth.JWK.
	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, fmt.Errorf("decode dpop proof signature: %w", err)
	}
	pub, _, err := jwkPublicKey(&header.JWK, header.Alg)
	if err != nil {
		return nil, nil, fmt.Errorf("parse dpop proof jwk: %w", err)
	}
	if err := verifyJWTSignature(pub, []byte(signingInput), signature); err != nil {
		return nil, nil, fmt.Errorf("verify dpop proof signature: %w", err)
	}

	return &header, &claims, nil
}

// VerifyDPoPProof validates a DPoP proof per RFC 9449 §4.3.
//
// accessToken is the bound access token (needed for ath validation).
// nonceCheck is called when a nonce is required; it returns true if the nonce is valid.
// jtiCheck, when non-nil, records the proof's jti and returns false on a replay;
// it is supplied only on the resource-server path, where a non-empty jti is also
// required. This function is called for resource-server side proof validation.
func VerifyDPoPProof(r *http.Request, header *DPoPHeader, claims *DPoPClaims, maxAgeDur time.Duration, accessToken string, nonceCheck, jtiCheck func(string) bool) error {
	if header.Typ != "dpop+jwt" {
		return errors.New("dpop proof typ must be dpop+jwt")
	}
	if !isAsymmetricAlg(header.Alg) {
		return fmt.Errorf("dpop proof alg must be an asymmetric algorithm, got %s", header.Alg)
	}
	if claims.HTM != r.Method {
		return fmt.Errorf("dpop proof htm %q does not match request method %q", claims.HTM, r.Method)
	}

	authority, scheme := effectiveRequestHostAndScheme(r)
	expectedHTU := scheme + "://" + authority + r.URL.Path
	if claims.HTU != expectedHTU {
		return fmt.Errorf("dpop proof htu %q does not match request URL %q", claims.HTU, expectedHTU)
	}

	maxAge := int64(max(maxAgeDur/time.Second, 1))
	now := time.Now().Unix()
	if claims.IAT < now-maxAge || claims.IAT > now+maxAge {
		return fmt.Errorf("dpop proof iat %d is outside allowed window (now=%d, maxAge=%ds)", claims.IAT, now, maxAge)
	}

	if claims.Nonce != "" && nonceCheck != nil && !nonceCheck(claims.Nonce) {
		return errors.New("dpop proof nonce is invalid or expired")
	}

	// Resource-server replay prevention (RFC 9449 §11.1): require a jti and
	// reject one already seen within the proof's max-age window.
	if jtiCheck != nil {
		if claims.JTI == "" {
			return errors.New("dpop proof missing required jti claim")
		}
		if !jtiCheck(claims.JTI) {
			return errors.New("dpop proof jti has already been used")
		}
	}

	if accessToken != "" {
		if claims.ATH == "" {
			return errors.New("dpop proof missing required ath claim")
		}
		expectedATH := DPoPAccessTokenHash(accessToken)
		if subtle.ConstantTimeCompare([]byte(claims.ATH), []byte(expectedATH)) != 1 {
			return errors.New("dpop proof ath does not match access token hash")
		}
	}

	return nil
}

// VerifyDPoPProofTokenEndpoint validates a DPoP proof at the token endpoint.
//
// For the token endpoint, ath validation is typically not performed (the token
// is being created), and nonce is validated if a nonce manager is available.
// jti replay tracking is deliberately omitted: single-use codes and refresh
// tokens already bound the token endpoint against replay.
func VerifyDPoPProofTokenEndpoint(r *http.Request, header *DPoPHeader, claims *DPoPClaims, maxAgeDur time.Duration, nonceCheck func(string) bool) error {
	return VerifyDPoPProof(r, header, claims, maxAgeDur, "", nonceCheck, nil)
}

// DPoPAccessTokenHash computes the DPoP ath claim (SHA-256 hash of access token base64url).
func DPoPAccessTokenHash(accessToken string) string {
	digest := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// minRSAModulusBits and maxRSAModulusBits bound a DPoP proof's RSA key.
//
// The floor (2048) rejects keys too weak to meaningfully bind the token; the
// ceiling (8192) caps the CPU a single verify can burn (RFC 9449 §4.3).
const (
	minRSAModulusBits = 2048
	maxRSAModulusBits = 8192
)

// jwkPublicKey converts a oauth.JWK to a Go public key and hash algorithm.
//
// alg is the JOSE "alg" from the proof header; it must agree with the oauth.JWK key
// type per RFC 9449 §4.3, otherwise the proof is rejected.
func jwkPublicKey(jwk *oauth.JWK, alg string) (crypto.PublicKey, crypto.Hash, error) {
	if err := checkAlgKeyType(alg, jwk); err != nil {
		return nil, 0, err
	}
	switch jwk.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(jwk.N)
		if err != nil {
			return nil, 0, fmt.Errorf("decode rsa n: %w", err)
		}
		e, err := base64.RawURLEncoding.DecodeString(jwk.E)
		if err != nil {
			return nil, 0, fmt.Errorf("decode rsa e: %w", err)
		}
		modulus := new(big.Int).SetBytes(n)
		if bits := modulus.BitLen(); bits < minRSAModulusBits || bits > maxRSAModulusBits {
			return nil, 0, fmt.Errorf("rsa modulus %d bits is outside allowed range [%d, %d]", bits, minRSAModulusBits, maxRSAModulusBits)
		}
		pub := &rsa.PublicKey{
			N: modulus,
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
		return pub, crypto.SHA256, nil
	case "EC":
		if jwk.X == "" || jwk.Y == "" || jwk.Crv == "" {
			return nil, 0, errors.New("ec jwk missing x, y, or crv")
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil {
			return nil, 0, fmt.Errorf("decode ec x: %w", err)
		}
		y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
		if err != nil {
			return nil, 0, fmt.Errorf("decode ec y: %w", err)
		}
		var curve elliptic.Curve
		switch jwk.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, 0, fmt.Errorf("unsupported ec curve: %s", jwk.Crv)
		}
		pub := &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}
		return pub, crypto.SHA256, nil
	case "OKP":
		if jwk.Crv != "Ed25519" {
			return nil, 0, fmt.Errorf("unsupported okp curve: %s", jwk.Crv)
		}
		if jwk.X == "" {
			return nil, 0, errors.New("okp jwk missing x")
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil {
			return nil, 0, fmt.Errorf("decode okp x: %w", err)
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, 0, fmt.Errorf("ed25519 key has wrong size: %d", len(x))
		}
		return ed25519.PublicKey(x), crypto.Hash(0), nil
	default:
		return nil, 0, fmt.Errorf("unsupported jwk key type: %s", jwk.Kty)
	}
}

// checkAlgKeyType asserts the proof's JOSE alg agrees with the oauth.JWK key type
// per RFC 9449 §4.3 (EC curves bind to ES*, RSA to RS*/PS*, Ed25519 to EdDSA).
func checkAlgKeyType(alg string, jwk *oauth.JWK) error {
	switch jwk.Kty {
	case "RSA":
		switch alg {
		case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
			return nil
		}
	case "EC":
		want := map[string]string{"P-256": "ES256", "P-384": "ES384", "P-521": "ES512"}[jwk.Crv]
		if want != "" && alg == want {
			return nil
		}
	case "OKP":
		if jwk.Crv == "Ed25519" && alg == "EdDSA" {
			return nil
		}
	default:
		return fmt.Errorf("unsupported jwk key type: %s", jwk.Kty)
	}
	return fmt.Errorf("dpop proof alg %q does not match jwk key type %q/%q", alg, jwk.Kty, jwk.Crv)
}

// verifyJWTSignature verifies a JWT signature against a public key.
// The algorithm is implied by the public key type: RSA uses PKCS1v15 SHA-256,
// ECDSA uses ASN.1 SHA-256, Ed25519 uses the raw message.
func verifyJWTSignature(pub crypto.PublicKey, signingInput, signature []byte) error {
	switch key := pub.(type) {
	case *rsa.PublicKey:
		if len(signature) < 1 {
			return errors.New("rsa signature too short")
		}
		digest := sha256.Sum256(signingInput)
		return verify(pub, digest[:], signature)
	case *ecdsa.PublicKey:
		digest := sha256.Sum256(signingInput)
		return verify(pub, digest[:], signature)
	case ed25519.PublicKey:
		if !ed25519.Verify(key, signingInput, signature) {
			return errors.New("ed25519 signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported public key type: %T", pub)
	}
}

// isAsymmetricAlg returns true if alg denotes an asymmetric signing algorithm.
func isAsymmetricAlg(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512",
		"EdDSA":
		return true
	}
	return false
}

// jwkThumbprintRequired builds the sorted JSON object of required oauth.JWK members per RFC 7638.
func jwkThumbprintRequired(jwk *oauth.JWK) (map[string]string, error) {
	switch jwk.Kty {
	case "RSA":
		if jwk.N == "" || jwk.E == "" {
			return nil, errors.New("rsa jwk missing required members (n, e) for thumbprint")
		}
		return map[string]string{"e": jwk.E, "kty": jwk.Kty, "n": jwk.N}, nil
	case "EC":
		if jwk.Crv == "" || jwk.X == "" || jwk.Y == "" {
			return nil, errors.New("ec jwk missing required members (crv, x, y) for thumbprint")
		}
		return map[string]string{"crv": jwk.Crv, "kty": jwk.Kty, "x": jwk.X, "y": jwk.Y}, nil
	case "OKP":
		if jwk.Crv == "" || jwk.X == "" {
			return nil, errors.New("okp jwk missing required members (crv, x) for thumbprint")
		}
		return map[string]string{"crv": jwk.Crv, "kty": jwk.Kty, "x": jwk.X}, nil
	default:
		return nil, fmt.Errorf("unsupported jwk key type for thumbprint: %s", jwk.Kty)
	}
}
