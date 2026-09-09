// Structured API error types and HTTP response envelope shared across all API versions.

package api

import (
	"fmt"
)

// ErrorCode is a machine-readable error identifier.
type ErrorCode string

// Error codes.
const (
	CodeBadRequest               ErrorCode = "BAD_REQUEST"
	CodeUnknownRepository        ErrorCode = "UNKNOWN_REPOSITORY"
	CodeInvalidOAuthState        ErrorCode = "INVALID_OAUTH_STATE"
	CodeOAuthGrantNotFound       ErrorCode = "OAUTH_GRANT_NOT_FOUND"
	CodeOAuthProviderUnavailable ErrorCode = "OAUTH_PROVIDER_UNAVAILABLE"
	CodeRepositoryPathConflict   ErrorCode = "REPOSITORY_PATH_CONFLICT"
	CodeUnknownCache             ErrorCode = "UNKNOWN_CACHE"
	CodeUnknownRuntime           ErrorCode = "UNKNOWN_RUNTIME"
	CodeUpdateCheckFailed        ErrorCode = "UPDATE_CHECK_FAILED"
	CodeUpdateUnavailable        ErrorCode = "UPDATE_UNAVAILABLE"
	CodeUnauthorized             ErrorCode = "UNAUTHORIZED"
	CodeForbidden                ErrorCode = "FORBIDDEN"
	CodeNotFound                 ErrorCode = "NOT_FOUND"
	CodeConflict                 ErrorCode = "CONFLICT"
	CodeInternalError            ErrorCode = "INTERNAL_ERROR"
)

// Error is a concrete error type with status code, error code, optional
// details, and optional wrapped error.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Code is the machine-readable error code.
	Code ErrorCode
	// Message is the client-facing error message.
	Message string
	// Details holds optional unstructured diagnostics.
	Details map[string]any
	// Cause is the optional underlying error.
	Cause error
}

// Error returns the error message, including any wrapped error.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap returns the wrapped error.
func (e *Error) Unwrap() error {
	return e.Cause
}

// ErrorResponse is the JSON envelope for error responses.
type ErrorResponse struct {
	Error   ErrorDetails   `json:"error"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorDetails holds the code and message within an error response.
type ErrorDetails struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}
