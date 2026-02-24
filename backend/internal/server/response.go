// JSON response writers for success and structured error responses.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/caic-xyz/caic/backend/internal/server/dto"
)

// writeError writes a structured JSON error response. If err implements
// dto.ErrorWithStatus, the HTTP status, error code and details are taken from
// it; otherwise 500 is used.
func writeError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	code := dto.CodeInternalError
	var details map[string]any

	var ews dto.ErrorWithStatus
	if errors.As(err, &ews) {
		statusCode = ews.StatusCode()
		code = ews.Code()
		details = ews.Details()
	}

	slog.Error("handler error", "err", err, "statusCode", statusCode, "code", code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := dto.ErrorResponse{
		Error:   dto.ErrorDetails{Code: code, Message: err.Error()},
		Details: details,
	}
	if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
		slog.Warn("failed to encode error response", "err", encErr)
	}
}

// writeJSONResponse writes a JSON success response or a structured error
// response, unifying both paths into a single call.
func writeJSONResponse[Out any](w http.ResponseWriter, output *Out, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if encErr := json.NewEncoder(w).Encode(output); encErr != nil {
		slog.Warn("failed to encode JSON response", "err", encErr)
	}
}
