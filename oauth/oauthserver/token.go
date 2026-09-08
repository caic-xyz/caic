// OAuth access-token signing and verification.

package oauthserver

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

const (
	accessTokenType      = "access_token"
	accessTokenHeaderTyp = "at+jwt"
	tokenClockSkew       = time.Minute
)

// signingKey holds a private key and its JWS algorithm identifier.
type signingKey struct {
	key         crypto.Signer
	alg         string // "RS256" or "ES256"
	verifyUntil time.Time
}

func configureAccessTokenService(state *Store, keyPEM []byte, kid string, ttl time.Duration, now time.Time) (*AccessTokenService, error) {
	configuredKey, configuredAlg, err := accessTokenKey(keyPEM)
	if err != nil {
		return nil, err
	}
	if kid == "" && state.currentSigningKID != "" {
		for _, stored := range state.accessTokenSigningKeys {
			if stored.KID != state.currentSigningKID {
				continue
			}
			storedKey, _, parseErr := accessTokenKey([]byte(stored.PrivateKeyPEM))
			if parseErr == nil && samePublicKey(storedKey, configuredKey) {
				kid = stored.KID
			}
			break
		}
	}
	if kid == "" {
		kid, err = randomToken()
		if err != nil {
			return nil, fmt.Errorf("generate oauth key id: %w", err)
		}
	}
	next := state.snapshot()
	changed, err := reconcileSigningKeys(&next, string(keyPEM), kid, ttl, now)
	if err != nil {
		return nil, err
	}
	if changed {
		err = state.transact(func(file *storeFile) bool {
			file.AccessTokenSigningKeys = slices.Clone(next.AccessTokenSigningKeys)
			file.CurrentSigningKID = next.CurrentSigningKID
			return true
		})
	}
	if err != nil {
		return nil, fmt.Errorf("persist oauth signing keys: %w", err)
	}
	keys := make(map[string]signingKey, len(state.accessTokenSigningKeys))
	for _, stored := range state.accessTokenSigningKeys {
		key, alg, parseErr := accessTokenKey([]byte(stored.PrivateKeyPEM))
		if parseErr != nil {
			return nil, fmt.Errorf("parse stored oauth signing key %q: %w", stored.KID, parseErr)
		}
		keys[stored.KID] = signingKey{key: key, alg: alg, verifyUntil: stored.VerifyUntil}
	}
	current := keys[state.currentSigningKID]
	if !samePublicKey(current.key, configuredKey) || current.alg != configuredAlg {
		return nil, errors.New("oauth: configured signing key does not match durable current key")
	}
	return &AccessTokenService{keys: keys, currentKID: state.currentSigningKID, ttl: ttl}, nil
}

func reconcileSigningKeys(file *storeFile, keyPEM, kid string, ttl time.Duration, now time.Time) (bool, error) {
	if ttl <= 0 {
		return false, errors.New("oauth: access token TTL must be positive")
	}
	configuredKey, _, err := accessTokenKey([]byte(keyPEM))
	if err != nil {
		return false, err
	}
	seen := make(map[string]struct{}, len(file.AccessTokenSigningKeys))
	currentCount := 0
	for _, stored := range file.AccessTokenSigningKeys {
		if stored.KID == "" || stored.PrivateKeyPEM == "" {
			return false, errors.New("oauth: durable signing key is incomplete")
		}
		if _, duplicate := seen[stored.KID]; duplicate {
			return false, fmt.Errorf("oauth: duplicate durable signing key ID %q", stored.KID)
		}
		seen[stored.KID] = struct{}{}
		if _, _, parseErr := accessTokenKey([]byte(stored.PrivateKeyPEM)); parseErr != nil {
			return false, fmt.Errorf("oauth: parse durable signing key %q: %w", stored.KID, parseErr)
		}
		if stored.VerifyUntil.IsZero() {
			currentCount++
			if stored.KID != file.CurrentSigningKID {
				return false, errors.New("oauth: durable signing key ring has an unexpected active key")
			}
		}
	}
	if len(file.AccessTokenSigningKeys) > 0 && currentCount != 1 {
		return false, errors.New("oauth: durable signing key ring must have exactly one active key")
	}
	changed := false
	retained := file.AccessTokenSigningKeys[:0]
	for _, key := range file.AccessTokenSigningKeys {
		if !key.VerifyUntil.IsZero() && !now.Before(key.VerifyUntil) {
			changed = true
			continue
		}
		retained = append(retained, key)
	}
	file.AccessTokenSigningKeys = retained
	if len(retained) == 0 {
		file.AccessTokenSigningKeys = []storedSigningKey{{KID: kid, PrivateKeyPEM: keyPEM}}
		file.CurrentSigningKID = kid
		return true, nil
	}
	for i := range retained {
		if retained[i].KID != kid {
			continue
		}
		storedKey, _, parseErr := accessTokenKey([]byte(retained[i].PrivateKeyPEM))
		if parseErr != nil {
			return false, parseErr
		}
		if retained[i].VerifyUntil.IsZero() && file.CurrentSigningKID == kid && samePublicKey(storedKey, configuredKey) {
			return changed, nil
		}
		// A retired KID is never reusable, and a KID never changes key material.
		return false, fmt.Errorf("oauth: signing key ID %q was already used", kid)
	}
	if len(retained) >= maxAccessTokenSigningKeys {
		return false, fmt.Errorf("oauth: signing key ring reached its %d-key limit", maxAccessTokenSigningKeys)
	}
	for i := range retained {
		if retained[i].KID == file.CurrentSigningKID && retained[i].VerifyUntil.IsZero() {
			retained[i].VerifyUntil = now.Add(ttl + tokenClockSkew)
		}
	}
	file.AccessTokenSigningKeys = append(file.AccessTokenSigningKeys, storedSigningKey{KID: kid, PrivateKeyPEM: keyPEM})
	file.CurrentSigningKID = kid
	return true, nil
}

func samePublicKey(a, b crypto.Signer) bool {
	aDER, aErr := x509.MarshalPKIXPublicKey(a.Public())
	bDER, bErr := x509.MarshalPKIXPublicKey(b.Public())
	return aErr == nil && bErr == nil && bytes.Equal(aDER, bDER)
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
	now := time.Now()
	jwks := make([]oauth.JWK, 0, len(s.keys))
	for kid, sk := range s.keys {
		if !sk.verifyUntil.IsZero() && !now.Before(sk.verifyUntil) {
			continue
		}
		switch pub := sk.key.Public().(type) {
		case *rsa.PublicKey:
			jwks = append(jwks, oauth.RSAJWK(kid, pub))
		case *ecdsa.PublicKey:
			jwks = append(jwks, oauth.ECJWK(kid, pub))
		}
	}
	slices.SortFunc(jwks, func(a, b oauth.JWK) int { return strings.Compare(a.Kid, b.Kid) })
	return jwks
}

// RotateKey rotates a standalone, in-memory service to a new ECDSA P-256 key.
// Server instances instead reconcile configured keys through the durable store.
func (s *AccessTokenService) RotateKey() (string, error) {
	return s.RotateKeyWithAlg("ES256")
}

// RotateKeyWithAlg rotates a standalone, in-memory service to a new key for
// alg. Previous keys remain available only for the access-token validation
// window; server instances use the durable store-backed lifecycle instead.
func (s *AccessTokenService) RotateKeyWithAlg(alg string) (string, error) {
	now := time.Now()
	for kid, key := range s.keys {
		if !key.verifyUntil.IsZero() && !now.Before(key.verifyUntil) {
			delete(s.keys, kid)
		}
	}
	if len(s.keys) >= maxAccessTokenSigningKeys {
		return "", fmt.Errorf("oauth: signing key ring reached its %d-key limit", maxAccessTokenSigningKeys)
	}
	var sk signingKey
	switch alg {
	case "RS256":
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", fmt.Errorf("generate oauth rotate rsa key: %w", err)
		}
		sk = signingKey{key: rsaKey, alg: "RS256"}
	case "ES256", "ES384", "ES512":
		// RFC 7518 §3.4 pairs each ES* alg with exactly one curve.
		curve := map[string]elliptic.Curve{
			"ES256": elliptic.P256(),
			"ES384": elliptic.P384(),
			"ES512": elliptic.P521(),
		}[alg]
		ecKey, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			return "", fmt.Errorf("generate oauth rotate ec key: %w", err)
		}
		sk = signingKey{key: ecKey, alg: alg}
	default:
		return "", fmt.Errorf("unsupported key algorithm: %q", alg)
	}
	kid, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate oauth rotate key id: %w", err)
	}
	s.keys[kid] = sk
	current := s.keys[s.currentKID]
	current.verifyUntil = now.Add(s.ttl + tokenClockSkew)
	s.keys[s.currentKID] = current
	s.currentKID = kid
	return kid, nil
}

// IssueAccessToken signs a JWT access token for user.
func (s *AccessTokenService) IssueAccessToken(issuer string, user oauth.User, audience, scope, grantID, clientID string) (string, error) {
	now := time.Now()
	jti, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate access token ID: %w", err)
	}
	return s.issueAccessTokenAt(&oauth.AccessTokenClaims{
		Issuer:   issuer,
		Subject:  user.ID,
		Audience: audience,
		ClientID: clientID,
		JWTID:    jti,
		Username: user.Username,
		Scope:    scope,
		GrantID:  grantID,
		Type:     accessTokenType,
	}, now, now.Add(s.ttl))
}

// IssueDPoPAccessToken signs a DPoP-bound JWT access token with cnf.jkt.
func (s *AccessTokenService) IssueDPoPAccessToken(issuer string, user oauth.User, audience, scope, grantID, dpopJKT, clientID string) (string, error) {
	now := time.Now()
	jti, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate access token ID: %w", err)
	}
	return s.issueAccessTokenAt(&oauth.AccessTokenClaims{
		Issuer:       issuer,
		Subject:      user.ID,
		Audience:     audience,
		ClientID:     clientID,
		JWTID:        jti,
		Username:     user.Username,
		Scope:        scope,
		GrantID:      grantID,
		Type:         accessTokenType,
		Confirmation: &oauth.TokenConfirmation{JKT: dpopJKT},
	}, now, now.Add(s.ttl))
}

// IssueRegistrationAccessToken issues a JWT for client registration management (RFC 7592).
// It remains valid for the durable registration's practical lifetime; deleting
// the registration or retiring the signing key ends its authority.
func (s *AccessTokenService) IssueRegistrationAccessToken(issuer, clientID string) (string, error) {
	now := time.Now()
	return s.issueTokenAt(&oauth.AccessTokenClaims{
		Issuer:   issuer,
		Subject:  clientID,
		Audience: issuer + "/oauth/register",
		Scope:    "client:manage",
		Type:     "registration_access_token",
	}, now, time.Unix(1<<62, 0), "JWT")
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
		if cid != claims.ClientID {
			return nil, errors.New("token client does not match its grant")
		}
		clientID = cid
	} else {
		clientID = claims.ClientID
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
	return s.issueTokenAt(claims, issuedAt, expiresAt, accessTokenHeaderTyp)
}

func (s *AccessTokenService) issueTokenAt(claims *oauth.AccessTokenClaims, issuedAt, expiresAt time.Time, headerTyp string) (string, error) {
	alg := s.keys[s.currentKID].alg
	headerJSON, err := json.Marshal(oauth.JWTHeader{Alg: alg, Typ: headerTyp, KID: s.currentKID})
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
	sk := s.keys[s.currentKID]
	signature, err := signJWS(sk.alg, sk.key, []byte(signingInput))
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseAndVerifyJWT splits a JWT, decodes header and payload, verifies the
// signature against the key identified by KID in the JWT header, and returns
// the raw header and payload JSON.
func (s *AccessTokenService) parseAndVerifyJWT(raw string, now time.Time) (parsedToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return parsedToken{}, errors.New("invalid bearer token format")
	}
	headerJSON, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token header: %w", err)
	}
	if _, err := decodeUniqueJSONObject(headerJSON); err != nil {
		return parsedToken{}, fmt.Errorf("parse token header: %w", err)
	}
	var header oauth.JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return parsedToken{}, fmt.Errorf("parse token header: %w", err)
	}
	keyInfo, ok := s.keys[header.KID]
	if !ok || header.Alg != keyInfo.alg || (!keyInfo.verifyUntil.IsZero() && !now.Before(keyInfo.verifyUntil)) {
		return parsedToken{}, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	if err := verifyJWS(keyInfo.key.Public(), keyInfo.alg, []byte(signingInput), signature); err != nil {
		return parsedToken{}, errors.New("invalid token signature")
	}
	payloadJSON, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return parsedToken{}, fmt.Errorf("decode token payload: %w", err)
	}
	if _, err := decodeUniqueJSONObject(payloadJSON); err != nil {
		return parsedToken{}, fmt.Errorf("parse token claims: %w", err)
	}
	return parsedToken{header: headerJSON, payload: payloadJSON}, nil
}

func (s *AccessTokenService) verifyClaims(token, issuer, audience string, now time.Time) (*oauth.AccessTokenClaims, error) {
	parsed, err := s.parseAndVerifyJWT(token, now)
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
	if claims.Subject == "" || claims.ClientID == "" || claims.JWTID == "" || claims.IssuedAt == 0 || claims.Expiry <= claims.IssuedAt {
		return nil, errors.New("token is missing required claims")
	}
	var header oauth.JWTHeader
	if err := json.Unmarshal(parsed.header, &header); err != nil || (!strings.EqualFold(header.Typ, "at+jwt") && !strings.EqualFold(header.Typ, "application/at+jwt")) {
		return nil, errors.New("invalid token header type")
	}
	nowUnix := now.Unix()
	clockSkew := int64(tokenClockSkew / time.Second)
	if claims.IssuedAt > nowUnix+clockSkew || claims.NotBefore > nowUnix+clockSkew || claims.Expiry <= nowUnix-clockSkew {
		return nil, errors.New("token is not valid now")
	}
	return &claims, nil
}

func (s *AccessTokenService) verifyRegistrationClaims(token, issuer, audience string, now time.Time) (*oauth.AccessTokenClaims, error) {
	parsed, err := s.parseAndVerifyJWT(token, now)
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
		alg, err := keyAlg(key)
		if err != nil {
			return nil, "", err
		}
		return signer, alg, nil
	}
	// Fall back to PKCS1 RSA.
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse oauth signing key: %w", err)
	}
	return key, "RS256", nil
}

// keyAlg returns the JWS algorithm name for a key.
//
// EC keys take the alg their curve mandates (RFC 7518 §3.4), so a P-384 key is
// ES384 rather than the ES256 a curve-blind mapping would report.
func keyAlg(key any) (string, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey, *rsa.PublicKey:
		return "RS256", nil
	case *ecdsa.PrivateKey:
		return ecdsaAlg(k.Curve)
	case *ecdsa.PublicKey:
		return ecdsaAlg(k.Curve)
	default:
		return "", fmt.Errorf("unsupported signing key type: %T", key)
	}
}
