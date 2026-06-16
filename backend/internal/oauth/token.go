// OAuth access-token signing and verification.

package oauth

import (
	"crypto"
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
)

const accessTokenType = "access_token"

// GrantTouchFunc marks or validates an OAuth grant during bearer-token verification.
// Returns (active, clientID, error).
type GrantTouchFunc func(grantID string, now time.Time) (active bool, clientID string, err error)

// UserLookupFunc returns the resource owner for a bearer-token subject.
type UserLookupFunc func(subject string) (User, bool)

// AccessTokenService signs and verifies OAuth JWT access tokens.
type AccessTokenService struct {
	key *rsa.PrivateKey
	kid string
	ttl time.Duration
}

// NewAccessTokenService returns an access-token service from configured key material.
//
// If keyPEM is empty, NewAccessTokenService generates a new RSA key. If kid is
// empty, it generates a random key ID.
func NewAccessTokenService(keyPEM []byte, kid string, ttl time.Duration) (*AccessTokenService, error) {
	key, err := accessTokenKey(keyPEM)
	if err != nil {
		return nil, err
	}
	if kid == "" {
		generatedKID, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("generate oauth key id: %w", err)
		}
		kid = generatedKID
	}
	return &AccessTokenService{key: key, kid: kid, ttl: ttl}, nil
}

// JWK returns the public signing key as an RSA JWK.
func (s *AccessTokenService) JWK() JWK {
	pub := s.key.PublicKey
	return RSAJWK(s.kid, &pub)
}

// IssueAccessToken signs a JWT access token for user.
func (s *AccessTokenService) IssueAccessToken(issuer string, user User, audience, scope, grantID string) (string, error) {
	now := time.Now()
	return s.issueAccessTokenAt(issuer, user, audience, scope, grantID, now, now.Add(s.ttl))
}

// VerifyAccessToken validates token and returns its bearer claims.
func (s *AccessTokenService) VerifyAccessToken(token, issuer, audience string, now time.Time, touchGrant GrantTouchFunc, findUser UserLookupFunc) (*BearerClaims, error) {
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
	if findUser == nil {
		return nil, errors.New("user lookup callback is required")
	}
	user, ok := findUser(claims.Subject)
	if !ok {
		return nil, errors.New("token subject is unknown")
	}
	return &BearerClaims{
		User:     user,
		Subject:  claims.Subject,
		Username: claims.Username,
		Issuer:   claims.Issuer,
		Audience: claims.Audience,
		Scopes:   strings.Fields(claims.Scope),
		Iat:      claims.IssuedAt,
		Exp:      claims.Expiry,
		ClientID: clientID,
	}, nil
}

func (s *AccessTokenService) issueAccessTokenAt(issuer string, user User, audience, scope, grantID string, issuedAt, expiresAt time.Time) (string, error) {
	headerJSON, err := json.Marshal(JWTHeader{Alg: JWTAlgRS256, Typ: "JWT", KID: s.kid})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(AccessTokenClaims{
		Issuer:    issuer,
		Subject:   user.ID,
		Audience:  audience,
		Username:  user.Username,
		Scope:     scope,
		GrantID:   grantID,
		IssuedAt:  issuedAt.Unix(),
		NotBefore: issuedAt.Unix(),
		Expiry:    expiresAt.Unix(),
		Type:      accessTokenType,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func accessTokenKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	if len(keyPEM) == 0 {
		generated, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate oauth signing key: %w", err)
		}
		return generated, nil
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("decode oauth signing key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse oauth signing key: %w", err)
	}
	return key, nil
}

func (s *AccessTokenService) verifyClaims(token, issuer, audience string, now time.Time) (*AccessTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid bearer token format")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode token header: %w", err)
	}
	var header JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse token header: %w", err)
	}
	if header.Alg != JWTAlgRS256 || header.KID != s.kid {
		return nil, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, errors.New("invalid token signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode token payload: %w", err)
	}
	var claims AccessTokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
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
	if claims.NotBefore > nowUnix || claims.Expiry <= nowUnix {
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
