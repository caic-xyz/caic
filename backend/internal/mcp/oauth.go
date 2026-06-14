// MCP OAuth wire DTOs and protocol helpers without caic authorization policy.

package mcp

import (
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

const (
	// OAuthGrantAuthorizationCode is the OAuth authorization-code grant type.
	OAuthGrantAuthorizationCode = "authorization_code"
	// OAuthGrantRefreshToken is the OAuth refresh-token grant type.
	OAuthGrantRefreshToken = "refresh_token"
	// OAuthCodeChallengeS256 is the PKCE S256 code challenge method.
	OAuthCodeChallengeS256 = "S256"
	// OAuthResponseTypeCode is the authorization-code response type.
	OAuthResponseTypeCode = "code"
	// OAuthTokenTypeBearer is the bearer access token type.
	OAuthTokenTypeBearer = "Bearer"
	// OAuthTokenEndpointAuthNone is the public-client token endpoint auth method.
	OAuthTokenEndpointAuthNone = "none"
	// JWTAlgRS256 is the RSASSA-PKCS1-v1_5 SHA-256 JWT algorithm.
	JWTAlgRS256 = "RS256"
)

// ProtectedResourceMetadata is OAuth 2.0 Protected Resource Metadata for an MCP resource.
type ProtectedResourceMetadata struct {
	Resource              string   `json:"resource"`
	AuthorizationServers  []string `json:"authorization_servers"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
	ResourceDocumentation string   `json:"resource_documentation,omitempty"`
}

// OAuthAuthorizationServerMetadata is OAuth/OIDC discovery metadata used by MCP clients.
type OAuthAuthorizationServerMetadata struct {
	Issuer                                        string   `json:"issuer"`
	AuthorizationEndpoint                         string   `json:"authorization_endpoint"`
	TokenEndpoint                                 string   `json:"token_endpoint"`
	JWKSURI                                       string   `json:"jwks_uri"`
	RegistrationEndpoint                          string   `json:"registration_endpoint"`
	RevocationEndpoint                            string   `json:"revocation_endpoint,omitempty"`
	ResponseTypesSupported                        []string `json:"response_types_supported"`
	GrantTypesSupported                           []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported                 []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported             []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported        []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	ScopesSupported                               []string `json:"scopes_supported,omitempty"`
	AuthorizationResponseIssuerParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// OAuthRegisterRequest is a dynamic client registration request.
type OAuthRegisterRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// OAuthRegisterResponse is a dynamic client registration response.
type OAuthRegisterResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// OAuthTokenResponse is an OAuth token endpoint response.
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// OAuthErrorResponse is an OAuth error response body.
type OAuthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// JWKSet is a JSON Web Key Set response.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// JWK is an RSA JSON Web Key used for JWT signature verification.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWTHeader is the JOSE header for a JWT access token.
type JWTHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

// AccessTokenClaims are JWT access-token claims used by the MCP resource server.
type AccessTokenClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	Username  string `json:"username"`
	Scope     string `json:"scope"`
	GrantID   string `json:"grant_id,omitempty"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	Expiry    int64  `json:"exp"`
	Type      string `json:"typ"`
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

// WriteOAuthError writes an OAuth JSON error response.
func WriteOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(OAuthErrorResponse{Error: code, ErrorDescription: description}); err != nil {
		slog.Warn("failed to encode OAuth error", "err", err)
	}
}
