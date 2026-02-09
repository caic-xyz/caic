// Structured API error types and JSON error response writer.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type errorCode string

const (
	codeBadRequest    errorCode = "BAD_REQUEST"
	codeNotFound      errorCode = "NOT_FOUND"
	codeConflict      errorCode = "CONFLICT"
	codeInternalError errorCode = "INTERNAL_ERROR"
)

type apiError struct {
	statusCode int
	code       errorCode
	message    string
}

func (e *apiError) Error() string {
	return e.message
}

func badRequest(msg string) *apiError {
	return &apiError{statusCode: http.StatusBadRequest, code: codeBadRequest, message: msg}
}

func notFound(resource string) *apiError {
	return &apiError{statusCode: http.StatusNotFound, code: codeNotFound, message: resource + " not found"}
}

func conflict(msg string) *apiError {
	return &apiError{statusCode: http.StatusConflict, code: codeConflict, message: msg}
}

func internalError(msg string) *apiError {
	return &apiError{statusCode: http.StatusInternalServerError, code: codeInternalError, message: msg}
}

// errorResponse is the JSON envelope for error responses.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    errorCode `json:"code"`
	Message string    `json:"message"`
}

// writeError writes a structured JSON error response. If err is an *apiError,
// the status code and code are taken from it; otherwise 500 is used.
func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = internalError(err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.statusCode)
	if encErr := json.NewEncoder(w).Encode(errorResponse{Error: errorBody{Code: ae.code, Message: ae.message}}); encErr != nil {
		slog.Warn("failed to encode error response", "err", encErr)
	}
}

// writeJSON writes a JSON response with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to encode JSON response", "err", err)
	}
}
