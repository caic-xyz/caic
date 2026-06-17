// SDK generation specification for OAuth types.

package oauth

import (
	"reflect"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
)

// SDKAPI returns the SDK generation specification for OAuth types.
//
//nolint:gosec // G101 false positive on RegisterRequest string literals
func SDKAPI() apispec.Config {
	return apispec.Config{
		// No routes — the OAuth endpoints are RFC-defined and don't follow
		// the REST route convention. This SDK is purely for DTO type generation.
		SDKPackagePaths: map[string]struct{}{
			reflect.TypeFor[AuthorizationServerMetadata]().PkgPath(): {},
		},
		ExtraSeeds: []reflect.Type{
			// Discovery
			reflect.TypeFor[AuthorizationServerMetadata](),
			reflect.TypeFor[ProtectedResourceMetadata](),
			// Client registration
			reflect.TypeFor[RegisterRequest](),
			reflect.TypeFor[RegisterResponse](),
			reflect.TypeFor[UpdateClientRequest](),
			// Token
			reflect.TypeFor[TokenResponse](),
			// Introspection
			reflect.TypeFor[IntrospectionRequest](),
			reflect.TypeFor[IntrospectionResponse](),
			// Error
			reflect.TypeFor[ErrorResponse](),
			// Keys
			reflect.TypeFor[JWK](),
			reflect.TypeFor[JWKSet](),
			// PAR
			reflect.TypeFor[PARResponse](),
			// Device authorization
			reflect.TypeFor[DeviceAuthorizationRequest](),
			reflect.TypeFor[DeviceAuthorizationResponse](),
			// DPoP
			reflect.TypeFor[TokenConfirmation](),
		},
		ErrorModel: apispec.ClientErrorModel{
			TypeName:         "ErrorResponse",
			TSCodeExpr:       "err.error",
			TSMessageExpr:    "err.error_description ?? \"\"",
			TSDetailsExpr:    "undefined",
			KTCodeExpr:       "err.error",
			KTMessageExpr:    "err.error_description ?: \"\"",
			KTDetailsExpr:    "null",
			SwiftCodeExpr:    "err.error",
			SwiftMessageExpr: "err.error_description ?? \"\"",
			SwiftDetailsExpr: "nil",
		},
		DocumentExtraSeeds: true,
		KotlinPackage:      "com.caic.oauth.sdk.v1",
		APIDocTitle:        "OAuth 2.0 Types Reference",
		APIDocIntro:        "DTO types for the OAuth 2.0 authorization server. Includes discovery metadata, client registration, token responses, introspection, and device authorization types.",
		SectionComments: map[string]string{
			"AuthorizationServerMetadata": "OAuth 2.0 Authorization Server Metadata (RFC 8414)",
			"ProtectedResourceMetadata":   "OAuth 2.0 Protected Resource Metadata (RFC 9728)",
			"RegisterRequest":             "OAuth 2.0 Dynamic Client Registration request (RFC 7591)",
			"RegisterResponse":            "OAuth 2.0 Dynamic Client Registration response (RFC 7591, 7592)",
			"UpdateClientRequest":         "OAuth 2.0 Dynamic Client Registration update (RFC 7592)",
			"TokenResponse":               "OAuth 2.0 token endpoint response (RFC 6749)",
			"IntrospectionRequest":        "OAuth 2.0 Token Introspection request (RFC 7662)",
			"IntrospectionResponse":       "OAuth 2.0 Token Introspection response (RFC 7662)",
			"ErrorResponse":               "OAuth 2.0 error response (RFC 6749 §5.2)",
			"JWK":                         "JSON Web Key (RFC 7517)",
			"JWKSet":                      "JSON Web Key Set (RFC 7517)",
			"PARResponse":                 "OAuth 2.0 Pushed Authorization Request response (RFC 9126)",
			"DeviceAuthorizationRequest":  "OAuth 2.0 Device Authorization request (RFC 8628)",
			"DeviceAuthorizationResponse": "OAuth 2.0 Device Authorization response (RFC 8628)",
			"TokenConfirmation":           "DPoP Token Confirmation (RFC 9449 §6.1)",
		},
	}
}
