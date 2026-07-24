// Structured API error types and constructors shared across all API versions.

package api

import (
	"fmt"
	"net/http"
)

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

// ErrorWithStatus is an error that carries an HTTP status code, error code,
// and optional details map.
type ErrorWithStatus interface {
	error
	StatusCode() int
	Code() ErrorCode
	Details() map[string]any
}

// Error is a concrete error type with status code, error code, optional
// details, and optional wrapped error.
type Error struct {
	statusCode int
	code       ErrorCode
	message    string
	details    map[string]any
	wrappedErr error
}

// Error returns the error message, including any wrapped error.
func (e *Error) Error() string {
	if e.wrappedErr != nil {
		return fmt.Sprintf("%s: %v", e.message, e.wrappedErr)
	}
	return e.message
}

// StatusCode returns the HTTP status code.
func (e *Error) StatusCode() int {
	return e.statusCode
}

// Code returns the machine-readable error code.
func (e *Error) Code() ErrorCode {
	return e.code
}

// Details returns the optional details map.
func (e *Error) Details() map[string]any {
	return e.details
}

// Unwrap returns the wrapped error.
func (e *Error) Unwrap() error {
	return e.wrappedErr
}

// WithDetail adds a single key/value to the error details.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.details == nil {
		e.details = make(map[string]any)
	}
	e.details[key] = value
	return e
}

// Wrap wraps an underlying error.
func (e *Error) Wrap(err error) *Error {
	e.wrappedErr = err
	return e
}

// Constructors.

// BadRequest creates a 400 error.
func BadRequest(msg string) *Error {
	return &Error{statusCode: http.StatusBadRequest, code: CodeBadRequest, message: msg}
}

// NotFound creates a 404 error.
func NotFound(resource string) *Error {
	return &Error{statusCode: http.StatusNotFound, code: CodeNotFound, message: resource + " not found"}
}

// Forbidden creates a 403 error.
func Forbidden(resource string) *Error {
	return &Error{statusCode: http.StatusForbidden, code: CodeForbidden, message: resource + " access denied"}
}

// Conflict creates a 409 error.
func Conflict(msg string) *Error {
	return &Error{statusCode: http.StatusConflict, code: CodeConflict, message: msg}
}

// InternalError creates a 500 error.
func InternalError(msg string) *Error {
	return &Error{statusCode: http.StatusInternalServerError, code: CodeInternalError, message: msg}
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
