// Package oauth is an OAuth 2.0 authorization server and client library.
//
// This package will be split into a standalone Go module (stdlib only).
// No caic-internal types or backend dependencies may leak in.
//
// Subpackages:
//
//	oauth/oauthclient - Authorization-code client helpers and provider
//	    (GitHub/GitLab) configurations.
//	oauth/oauthserver - OAuth authorization server HTTP handlers and
//	    token management.
//
// This root package holds shared constants and protocol-level DTOs used
// by both subpackages. Server-side types live in dto_server.go;
// client-side types live in dto_client.go.
//
// Implemented RFCs: 6749 (Authorization Framework), 6750 (Bearer Token
// Usage), 7636 (PKCE), 7009 (Token Revocation), 7662 (Token
// Introspection), 9068 (JWT Profile for Access Tokens), 8414
// (Authorization Server Metadata), 7591 (Dynamic Client Registration),
// 7592 (Client Registration Management), 8628 (Device Authorization
// Grant), 9126 (Pushed Authorization Requests), 9207 (Issuer
// Identification), 9449 (DPoP), and 9700 (OAuth Security BCP).
//
// Not implemented: 8693 (Token Exchange). Issue audience-scoped tokens at
// the authorization endpoint via the resource parameter (RFC 8707) instead.
package oauth

const (
	// GrantAuthorizationCode is the OAuth authorization-code grant type.
	GrantAuthorizationCode = "authorization_code"
	// GrantRefreshToken is the OAuth refresh-token grant type.
	GrantRefreshToken = "refresh_token"
	// GrantDeviceCode is the OAuth device authorization grant type.
	GrantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"
	// CodeChallengeS256 is the PKCE S256 code challenge method.
	CodeChallengeS256 = "S256"
	// ResponseTypeCode is the authorization-code response type.
	ResponseTypeCode = "code"
	// TokenTypeBearer is the bearer access token type.
	TokenTypeBearer = "Bearer"
	// TokenEndpointAuthNone is the public-client token endpoint auth method.
	TokenEndpointAuthNone = "none"
	// JWTAlgRS256 is the RSASSA-PKCS1-v1_5 SHA-256 JWT algorithm.
	JWTAlgRS256 = "RS256"
)
