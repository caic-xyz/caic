// OAuth access-token signing and verification.

package oauthserver

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

const accessTokenType = "access_token"

// signingKey holds a private key and its JWS algorithm identifier.
type signingKey struct {
	key crypto.Signer
	alg string // "RS256" or "ES256"
}

// parsedToken holds the decoded header and payload of a verified JWT.
type parsedToken struct {
	header  json.RawMessage
	payload json.RawMessage
}

// GrantTouchFunc marks or validates an OAuth grant during bearer-token verification.
// Returns (active, clientID, error).
type GrantTouchFunc func(grantID string, now time.Time) (active bool, clientID string, err error)

// AccessTokenService signs and verifies OAuth JWT access tokens.
type AccessTokenService struct {
	keys       map[string]signingKey // active signing keys, keyed by KID
	currentKID string                // KID of the key used for new tokens
	ttl        time.Duration
}

// NewAccessTokenService returns an access-token service from configured key material.
//
// keyPEM must hold a PEM-encoded RSA or EC private key; NewAccessTokenService
// errors if it is empty. Persistent key material keeps issued tokens valid
// across restarts and keeps JWKS consumers stable (RFC 9068). If kid is empty, a
// random key ID is generated.
func NewAccessTokenService(keyPEM []byte, kid string, ttl time.Duration) (*AccessTokenService, error) {
	if len(keyPEM) == 0 {
		return nil, errors.New("oauth: signing key PEM is required")
	}
	key, alg, err := accessTokenKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return newAccessTokenService(key, alg, kid, ttl)
}

func newAccessTokenService(key crypto.Signer, alg, kid string, ttl time.Duration) (*AccessTokenService, error) {
	if kid == "" {
		generatedKID, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("generate oauth key id: %w", err)
		}
		kid = generatedKID
	}
	return &AccessTokenService{
		keys:       map[string]signingKey{kid: {key: key, alg: alg}},
		currentKID: kid,
		ttl:        ttl,
	}, nil
}

// JWK returns all active public signing keys as JWKs.
func (s *AccessTokenService) JWK() []oauth.JWK {
	jwks := make([]oauth.JWK, 0, len(s.keys))
	for kid, sk := range s.keys {
		switch pub := sk.key.Public().(type) {
		case *rsa.PublicKey:
			jwks = append(jwks, oauth.RSAJWK(kid, pub))
		case *ecdsa.PublicKey:
			jwks = append(jwks, oauth.ECJWK(kid, pub))
		}
	}
	return jwks
}

// RotateKey generates a new ECDSA P-256 key with a new KID, adds it to the
// active set, and makes it the current signing key. Returns the new KID.
func (s *AccessTokenService) RotateKey() (string, error) {
	return s.RotateKeyWithAlg("ES256")
}

// RotateKeyWithAlg generates a new key of the specified algorithm ("RS256" or
// "ES256") in the active set and makes it the current signing key.
// Returns the new KID.
func (s *AccessTokenService) RotateKeyWithAlg(alg string) (string, error) {
	var sk signingKey
	switch alg {
	case "RS256":
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", fmt.Errorf("generate oauth rotate rsa key: %w", err)
		}
		sk = signingKey{key: rsaKey, alg: "RS256"}
	case "ES256":
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", fmt.Errorf("generate oauth rotate ec key: %w", err)
		}
		sk = signingKey{key: ecKey, alg: "ES256"}
	default:
		return "", fmt.Errorf("unsupported key algorithm: %q", alg)
	}
	kid, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate oauth rotate key id: %w", err)
	}
	s.keys[kid] = sk
	s.currentKID = kid
	return kid, nil
}

// IssueAccessToken signs a JWT access token for user.
func (s *AccessTokenService) IssueAccessToken(issuer string, user oauth.User, audience, scope, grantID string) (string, error) {
	now := time.Now()
	return s.issueAccessTokenAt(&oauth.AccessTokenClaims{
		Issuer:   issuer,
		Subject:  user.ID,
		Audience: audience,
		Username: user.Username,
		Scope:    scope,
		GrantID:  grantID,
		Type:     accessTokenType,
	}, now, now.Add(s.ttl))
}

// IssueDPoPAccessToken signs a DPoP-bound JWT access token with cnf.jkt.
func (s *AccessTokenService) IssueDPoPAccessToken(issuer string, user oauth.User, audience, scope, grantID, dpopJKT string) (string, error) {
	now := time.Now()
	return s.issueAccessTokenAt(&oauth.AccessTokenClaims{
		Issuer:       issuer,
		Subject:      user.ID,
		Audience:     audience,
		Username:     user.Username,
		Scope:        scope,
		GrantID:      grantID,
		Type:         accessTokenType,
		Confirmation: &oauth.TokenConfirmation{JKT: dpopJKT},
	}, now, now.Add(s.ttl))
}

// IssueRegistrationAccessToken issues a short-lived JWT for client registration management (RFC 7592).
// The subject is the client ID and the audience scopes it to the registration endpoint.
func (s *AccessTokenService) IssueRegistrationAccessToken(issuer, clientID string) (string, error) {
	now := time.Now()
	return s.issueAccessTokenAt(&oauth.AccessTokenClaims{
		Issuer:   issuer,
		Subject:  clientID,
		Audience: issuer + "/oauth/register",
		Scope:    "client:manage",
		Type:     "registration_access_token",
	}, now, now.Add(time.Hour))
}

// VerifyRegistrationAccessToken validates a registration access token and returns the client ID from the subject claim.
func (s *AccessTokenService) VerifyRegistrationAccessToken(token, issuer, audience string, now time.Time) (clientID string, err error) {
	claims, err := s.verifyRegistrationClaims(token, issuer, audience, now)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// VerifyAccessToken validates token and returns its bearer claims.
func (s *AccessTokenService) VerifyAccessToken(token, issuer, audience string, now time.Time, touchGrant GrantTouchFunc, session SessionManager) (*oauth.BearerClaims, error) {
	claims, err := s.verifyClaims(token, issuer, audience, now)
	if err != nil {
		return nil, err
	}
	var clientID string
	if claims.GrantID != "" {
		if touchGrant == nil {
			return nil, errors.New("grant liveness callback is required")
		}
		active, cid, err := touchGrant(claims.GrantID, now)
		if err != nil {
			return nil, fmt.Errorf("touch token grant: %w", err)
		}
		if !active {
			return nil, errors.New("token grant is not active")
		}
		clientID = cid
	}
	if session == nil {
		return nil, errors.New("user lookup callback is required")
	}
	user, ok := session.FindUser(claims.Subject)
	if !ok {
		return nil, errors.New("token subject is unknown")
	}
	return &oauth.BearerClaims{
		User:         user,
		Subject:      claims.Subject,
		Username:     claims.Username,
		Issuer:       claims.Issuer,
		Audience:     claims.Audience,
		Scopes:       strings.Fields(claims.Scope),
		Iat:          claims.IssuedAt,
		Exp:          claims.Expiry,
		ClientID:     clientID,
		Confirmation: claims.Confirmation,
	}, nil
}

func (s *AccessTokenService) issueAccessTokenAt(claims *oauth.AccessTokenClaims, issuedAt, expiresAt time.Time) (string, error) {
	alg := s.keys[s.currentKID].alg
	headerJSON, err := json.Marshal(oauth.JWTHeader{Alg: alg, Typ: "JWT", KID: s.currentKID})
	if err != nil {
		return "", err
	}
	claims.IssuedAt = issuedAt.Unix()
	claims.NotBefore = issuedAt.Unix()
	claims.Expiry = expiresAt.Unix()
	payloadJSON, err := json.Marshal(*claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sk := s.keys[s.currentKID]
	signature, err := sign(sk.key, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseAndVerifyJWT splits a JWT, decodes header and payload, verifies the
// signature against the key identified by KID in the JWT header, and returns
// the raw header and payload JSON.
func (s *AccessTokenService) parseAndVerifyJWT(raw string) (parsedToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return parsedToken{}, errors.New("invalid bearer token format")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token header: %w", err)
	}
	var header oauth.JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return parsedToken{}, fmt.Errorf("parse token header: %w", err)
	}
	keyInfo, ok := s.keys[header.KID]
	if !ok || header.Alg != keyInfo.alg {
		return parsedToken{}, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := verify(keyInfo.key.Public(), digest[:], signature); err != nil {
		return parsedToken{}, errors.New("invalid token signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token payload: %w", err)
	}
	return parsedToken{header: headerJSON, payload: payloadJSON}, nil
}

func (s *AccessTokenService) verifyClaims(token, issuer, audience string, now time.Time) (*oauth.AccessTokenClaims, error) {
	parsed, err := s.parseAndVerifyJWT(token)
	if err != nil {
		return nil, err
	}
	var claims oauth.AccessTokenClaims
	if err := json.Unmarshal(parsed.payload, &claims); err != nil {
		return nil, fmt.Errorf("parse token claims: %w", err)
	}
	if claims.Issuer != issuer {
		return nil, errors.New("invalid token issuer")
	}
	if claims.Audience != audience {
		return nil, errors.New("invalid token audience")
	}
	if claims.Type != accessTokenType {
		return nil, errors.New("invalid token type")
	}
	nowUnix := now.Unix()
	clockSkew := int64(60) // ±1 minute per RFC 9068 §2.1
	if claims.NotBefore > nowUnix+clockSkew || claims.Expiry <= nowUnix-clockSkew {
		return nil, errors.New("token is not valid now")
	}
	return &claims, nil
}

func (s *AccessTokenService) verifyRegistrationClaims(token, issuer, audience string, now time.Time) (*oauth.AccessTokenClaims, error) {
	parsed, err := s.parseAndVerifyJWT(token)
	if err != nil {
		return nil, err
	}
	var claims oauth.AccessTokenClaims
	if err := json.Unmarshal(parsed.payload, &claims); err != nil {
		return nil, fmt.Errorf("parse token claims: %w", err)
	}
	if claims.Issuer != issuer {
		return nil, errors.New("invalid token issuer")
	}
	if claims.Audience != audience {
		return nil, errors.New("invalid token audience")
	}
	if claims.Type != "registration_access_token" {
		return nil, errors.New("invalid token type")
	}
	nowUnix := now.Unix()
	clockSkew := int64(60) // ±1 minute per RFC 9068 §2.1
	if claims.NotBefore > nowUnix+clockSkew || claims.Expiry <= nowUnix-clockSkew {
		return nil, errors.New("token is not valid now")
	}
	return &claims, nil
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// sign signs a digest with the given key. The algorithm is implied by the key
// type: RSA uses PKCS1v15 SHA-256, ECDSA uses ASN.1.
func sign(pub crypto.Signer, digest []byte) ([]byte, error) {
	switch key := pub.(type) {
	case *rsa.PrivateKey:
		return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest)
	case *ecdsa.PrivateKey:
		return ecdsa.SignASN1(rand.Reader, key, digest)
	default:
		return nil, fmt.Errorf("unsupported signing key type: %T", pub)
	}
}

// verify verifies a signature against a public key. The algorithm is implied
// by the key type: RSA uses PKCS1v15 SHA-256, ECDSA uses ASN.1.
func verify(pub crypto.PublicKey, digest, signature []byte) error {
	switch key := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest, signature)
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, digest, signature) {
			return errors.New("ecdsa signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported public key type: %T", pub)
	}
}

// accessTokenKey parses a PEM-encoded key and returns it as a crypto.Signer
// with its JWS algorithm.
func accessTokenKey(keyPEM []byte) (crypto.Signer, string, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, "", errors.New("decode oauth signing key PEM")
	}
	// Try PKCS8 first (supports both RSA and EC).
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, "", fmt.Errorf("key type %T does not implement crypto.Signer", key)
		}
		return signer, keyAlg(key), nil
	}
	// Fall back to PKCS1 RSA.
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse oauth signing key: %w", err)
	}
	return key, "RS256", nil
}

// keyAlg returns the JWS algorithm name for a key.
func keyAlg(key any) string {
	switch key.(type) {
	case *rsa.PrivateKey, *rsa.PublicKey:
		return "RS256"
	case *ecdsa.PrivateKey, *ecdsa.PublicKey:
		return "ES256"
	default:
		return ""
	}
}
