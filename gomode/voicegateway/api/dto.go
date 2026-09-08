// Package api provides shared voice gateway API infrastructure.
package api

import (
	"fmt"
)

// Validatable is implemented by request types that can validate their fields.
type Validatable interface {
	Validate() error
}

// EmptyReq is used for endpoints that take no request body.
type EmptyReq struct{}

// Validate is a no-op for empty requests.
func (EmptyReq) Validate() error { return nil }

// ErrorCode is a machine-readable error identifier.
type ErrorCode string

// Standard error codes.
const (
	CodeBadRequest    ErrorCode = "BAD_REQUEST"
	CodeUnauthorized  ErrorCode = "UNAUTHORIZED"
	CodeForbidden     ErrorCode = "FORBIDDEN"
	CodeNotFound      ErrorCode = "NOT_FOUND"
	CodeConflict      ErrorCode = "CONFLICT"
	CodeInternalError ErrorCode = "INTERNAL_ERROR"
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
	// Error contains the machine-readable code and human-readable message.
	Error ErrorDetails `json:"error"`
	// Details contains optional endpoint-specific structured error context.
	Details map[string]any `json:"details,omitempty"`
}

// ErrorDetails holds the code and message within an error response.
type ErrorDetails struct {
	// Code is the machine-readable error code.
	Code ErrorCode `json:"code"`
	// Message is the human-readable error message.
	Message string `json:"message"`
}
