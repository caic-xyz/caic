// Server-side OAuth 2.0 DTOs: types consumed by the authorization server,
// token management, DPoP, and protected resource metadata.

package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

// TokenConfirmation holds key confirmation claims per RFC 9449 §6.1.
type TokenConfirmation struct {
	JKT string `json:"jkt"`
}

// User is the package-neutral OAuth view of an authenticated resource owner.
type User struct {
	ID       string
	Username string
	Provider string
}

// BearerClaims holds verified bearer-token identity and authorization claims.
type BearerClaims struct {
	User         User
	Subject      string
	Username     string
	Issuer       string
	Audience     string
	Scopes       []string
	Iat          int64
	Exp          int64
	ClientID     string
	Confirmation *TokenConfirmation
}

// ProtectedResourceMetadata is OAuth 2.0 Protected Resource Metadata.
type ProtectedResourceMetadata struct {
	Resource              string   `json:"resource"`
	AuthorizationServers  []string `json:"authorization_servers"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
	ResourceDocumentation string   `json:"resource_documentation,omitempty"`
}

// AuthorizationServerMetadata is OAuth/OIDC discovery metadata.
type AuthorizationServerMetadata struct {
	Issuer                                        string   `json:"issuer"`
	AuthorizationEndpoint                         string   `json:"authorization_endpoint"`
	TokenEndpoint                                 string   `json:"token_endpoint"`
	IntrospectionEndpoint                         string   `json:"introspection_endpoint,omitempty"`
	JWKSURI                                       string   `json:"jwks_uri"`
	RegistrationEndpoint                          string   `json:"registration_endpoint"`
	RevocationEndpoint                            string   `json:"revocation_endpoint,omitempty"`
	EndSessionEndpoint                            string   `json:"end_session_endpoint,omitempty"`
	ResponseTypesSupported                        []string `json:"response_types_supported"`
	GrantTypesSupported                           []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported                 []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported             []string `json:"token_endpoint_auth_methods_supported"`
	IntrospectionEndpointAuthMethodsSupported     []string `json:"introspection_endpoint_auth_methods_supported,omitempty"`
	RevocationEndpointAuthMethodsSupported        []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	ScopesSupported                               []string `json:"scopes_supported,omitempty"`
	DPoPSigningAlgValuesSupported                 []string `json:"dpop_signing_alg_values_supported,omitempty"`
	AuthorizationResponseIssuerParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
	PushedAuthorizationRequestEndpoint            string   `json:"pushed_authorization_request_endpoint,omitempty"`
	RequirePushedAuthorizationRequests            bool     `json:"require_pushed_authorization_requests"`
}

// RegisterRequest is a dynamic client registration request.
type RegisterRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// RegisterResponse is a dynamic client registration response (RFC 7592).
type RegisterResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	RegistrationAccessToken string   `json:"registration_access_token,omitempty"`
	RegistrationClientURI   string   `json:"registration_client_uri,omitempty"`
}

// PARResponse is an RFC 9126 pushed authorization request response.
type PARResponse struct {
	RequestURI string `json:"request_uri"`
	ExpiresIn  int64  `json:"expires_in"`
}

// DeviceAuthorizationRequest is an RFC 8628 device authorization request body.
type DeviceAuthorizationRequest struct {
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
}

// DeviceAuthorizationResponse is an RFC 8628 device authorization response body.
type DeviceAuthorizationResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval,omitempty"`
}

// UpdateClientRequest is an RFC 7592 client update request body.
// Nil pointer fields leave the corresponding client field unchanged.
type UpdateClientRequest struct {
	ClientName              *string   `json:"client_name,omitempty"`
	RedirectURIs            *[]string `json:"redirect_uris,omitempty"`
	TokenEndpointAuthMethod *string   `json:"token_endpoint_auth_method,omitempty"`
}

// TokenResponse is an OAuth token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// IntrospectionRequest is an RFC 7662 token introspection request body.
type IntrospectionRequest struct {
	Token         string `json:"token"`
	TokenTypeHint string `json:"token_type_hint,omitempty"`
}

// IntrospectionResponse is an RFC 7662 token introspection response body.
type IntrospectionResponse struct {
	Active       bool               `json:"active"`
	Scope        string             `json:"scope,omitempty"`
	ClientID     string             `json:"client_id,omitempty"`
	TokenType    string             `json:"token_type,omitempty"`
	Exp          int64              `json:"exp,omitempty"`
	Sub          string             `json:"sub,omitempty"`
	Username     string             `json:"username,omitempty"`
	Iss          string             `json:"iss,omitempty"`
	Aud          string             `json:"aud,omitempty"`
	Iat          int64              `json:"iat,omitempty"`
	Confirmation *TokenConfirmation `json:"cnf,omitempty"`
}

// ErrorResponse is an OAuth error response body.
type ErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// JWKSet is a JSON Web Key Set response.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// JWK is a JSON Web Key.  For server keys (RSA), only kty/n/e/kid/use/alg
// are populated.  For client DPoP keys, crv/x/y may also be set.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JWTHeader is the JOSE header for a JWT access token.
type JWTHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

// AccessTokenClaims are JWT access-token claims.
type AccessTokenClaims struct {
	Issuer       string             `json:"iss"`
	Subject      string             `json:"sub"`
	Audience     string             `json:"aud"`
	Username     string             `json:"username"`
	Scope        string             `json:"scope"`
	GrantID      string             `json:"grant_id,omitempty"`
	IssuedAt     int64              `json:"iat"`
	NotBefore    int64              `json:"nbf"`
	Expiry       int64              `json:"exp"`
	Type         string             `json:"typ"`
	Confirmation *TokenConfirmation `json:"cnf,omitempty"`
}

// BearerToken extracts an OAuth bearer token from the Authorization header.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

// BearerChallenge formats a WWW-Authenticate Bearer challenge.
func BearerChallenge(resourceMetadataURL, scope string) string {
	return "Bearer resource_metadata=" + strconv.Quote(resourceMetadataURL) + ", scope=" + strconv.Quote(scope)
}

// BearerScopeChallenge formats a WWW-Authenticate Bearer scope challenge.
func BearerScopeChallenge(scope string) string {
	return "Bearer scope=" + strconv.Quote(scope)
}

// VerifyPKCES256 verifies a PKCE S256 code verifier against its challenge.
func VerifyPKCES256(challenge, verifier string) bool {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:]) == challenge
}

// RefreshTokenKey returns the storage key for an opaque refresh token.
func RefreshTokenKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// RSAJWK returns an RSA signing key in JWK form.
func RSAJWK(kid string, pub *rsa.PublicKey) JWK {
	return JWK{
		Kty: "RSA",
		Use: "sig",
		Alg: JWTAlgRS256,
		Kid: kid,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// ECJWK returns an ECDSA signing key in JWK form.
//
// crv and alg follow the key's curve (RFC 7518 §3.4, §6.2.1.1). Reporting a
// fixed P-256/ES256 would mislabel a P-384 or P-521 key and make every
// published coordinate unparseable to a conforming client.
func ECJWK(kid string, pub *ecdsa.PublicKey) JWK {
	raw, _ := pub.Bytes()
	keySize := (len(raw) - 1) / 2
	var crv, alg string
	switch pub.Curve {
	case elliptic.P384():
		crv, alg = "P-384", "ES384"
	case elliptic.P521():
		crv, alg = "P-521", "ES512"
	default:
		crv, alg = "P-256", "ES256"
	}
	return JWK{
		Kty: "EC",
		Use: "sig",
		Alg: alg,
		Kid: kid,
		Crv: crv,
		X:   base64.RawURLEncoding.EncodeToString(raw[1 : 1+keySize]),
		Y:   base64.RawURLEncoding.EncodeToString(raw[1+keySize:]),
	}
}

// WriteError writes an OAuth JSON error response.
func WriteError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorResponse{Error: code, ErrorDescription: description}); err != nil {
		slog.Warn("failed to encode OAuth error", "err", err)
	}
}
