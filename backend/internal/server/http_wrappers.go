// Generic HTTP handler wrappers: decode JSON bodies, validate typed inputs, and encode JSON responses or structured errors.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	voiceapi "github.com/caic-xyz/caic/gomode/voicegateway/api"
)

type validatable interface {
	Validate() error
}

// handle wraps a typed handler function into an http.HandlerFunc. It reads the
// JSON body (with DisallowUnknownFields), populates path parameters via struct
// tags, validates, calls fn, and writes the JSON response or structured error.
func handle[In any, PtrIn interface {
	*In
	validatable
}, Out any](fn func(context.Context, PtrIn) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := PtrIn(new(In))
		if !readAndDecodeBody(w, r, in) {
			return
		}
		populatePathParams(r, in)
		if err := in.Validate(); err != nil {
			writeError(w, err)
			return
		}
		out, err := fn(r.Context(), in)
		writeJSONResponse(w, out, err)
	}
}

type taskEntryResolver interface {
	getTask(r *http.Request) (*tasks.Entry, error)
	notifyTaskChange()
}

// handleWithTask wraps a typed handler that also needs the resolved *tasks.Entry.
// It parses {id}, looks up the task via resolver.getTask, then proceeds like handle.
func handleWithTask[In any, PtrIn interface {
	*In
	validatable
}, Out any](resolver taskEntryResolver, fn func(context.Context, *tasks.Entry, PtrIn) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, err := resolver.getTask(r)
		if err != nil {
			writeError(w, err)
			return
		}
		in := PtrIn(new(In))
		if !readAndDecodeBody(w, r, in) {
			return
		}
		populatePathParams(r, in)
		if err := in.Validate(); err != nil {
			writeError(w, err)
			return
		}
		out, err := fn(r.Context(), entry, in)
		if err == nil {
			resolver.notifyTaskChange()
		}
		writeJSONResponse(w, out, err)
	}
}

// toDTO maps a tasks.Error to the matching API error so the HTTP layer can
// emit the correct status code. A nil error returns nil. An error that is
// already an API error (ErrorWithStatus) is returned unchanged. Any other error
// falls back to a 500.
func toDTO(err error) error {
	if err == nil {
		return nil
	}
	var te *tasks.Error
	if errors.As(err, &te) {
		// Map every kind via te.Error() so a wrapped cause (e.g. the plan-read
		// failure on restart, or the compact no-session error) is preserved in
		// the message. Error() == Msg when there is no wrapped error.
		switch te.Kind {
		case tasks.KindNotFound:
			// api.NotFound appends " not found"; trim it from the manager's
			// message (e.g. "task X not found") to avoid a doubled suffix.
			return api.NotFound(strings.TrimSuffix(te.Error(), " not found"))
		case tasks.KindConflict:
			return api.Conflict(te.Error())
		case tasks.KindBadRequest:
			return api.BadRequest(te.Error())
		case tasks.KindInternal:
			return api.InternalError(te.Error())
		default:
			return api.InternalError(te.Error())
		}
	}
	var ews api.ErrorWithStatus
	if errors.As(err, &ews) {
		return err
	}
	return api.InternalError(err.Error())
}

// readAndDecodeBody reads the request body and decodes JSON into input. It
// skips decoding for EmptyReq. Unknown JSON fields are rejected. Returns false
// if an error was written to the response.
func readAndDecodeBody[In any](w http.ResponseWriter, r *http.Request, input *In) bool {
	if _, isEmpty := any(input).(*api.EmptyReq); isEmpty {
		return true
	}
	body, err := io.ReadAll(r.Body)
	if err2 := r.Body.Close(); err == nil {
		err = err2
	}
	if err != nil {
		writeError(w, api.BadRequest("failed to read request body"))
		return false
	}
	if len(body) == 0 {
		return true
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(input); err != nil {
		slog.ErrorContext(r.Context(), "failed to decode request body", "err", err)
		writeError(w, api.BadRequest("invalid request body"))
		return false
	}
	return true
}

// populatePathParams extracts path parameters from the request and populates
// struct fields tagged with `path:"paramName"`.
func populatePathParams(r *http.Request, input any) {
	val := reflect.ValueOf(input)
	if val.Kind() != reflect.Pointer {
		return
	}
	elem := val.Elem()
	if elem.Kind() != reflect.Struct {
		return
	}
	typ := elem.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("path")
		if tag == "" {
			continue
		}
		paramValue := r.PathValue(tag)
		if paramValue == "" {
			continue
		}
		//exhaustive:ignore
		switch field.Type.Kind() {
		case reflect.String:
			elem.Field(i).SetString(paramValue)
		case reflect.Int:
			if v, err := strconv.Atoi(paramValue); err == nil {
				elem.Field(i).SetInt(int64(v))
			}
		}
	}
}

// writeError writes a structured JSON error response. If err implements
// api.ErrorWithStatus, the HTTP status, error code and details are taken from
// it; otherwise 500 is used.
func writeError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	code := api.CodeInternalError
	var details map[string]any

	var ews api.ErrorWithStatus
	if errors.As(err, &ews) {
		statusCode = ews.StatusCode()
		code = ews.Code()
		details = ews.Details()
	}
	var voiceEWS voiceapi.ErrorWithStatus
	if errors.As(err, &voiceEWS) {
		statusCode = voiceEWS.StatusCode()
		code = api.ErrorCode(voiceEWS.Code())
		details = voiceEWS.Details()
	}

	slog.Error("handler error", "err", err, "statusCode", statusCode, "code", code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := api.ErrorResponse{
		Error:   api.ErrorDetails{Code: code, Message: err.Error()},
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

// userIDFromCtx returns the authenticated user's ID, or "default" in no-auth mode.
func userIDFromCtx(ctx context.Context) string {
	if u, ok := auth.UserFromContext(ctx); ok {
		return u.ID
	}
	return "default"
}

// computeTaskPatch returns a sparse map containing only the fields that differ
// between oldJSON and newJSON, always including "id". Fields present in oldJSON
// but absent in newJSON are set to null so clients can clear them.
func computeTaskPatch(oldJSON, newJSON []byte) (map[string]json.RawMessage, error) {
	var oldMap, newMap map[string]json.RawMessage
	if err := json.Unmarshal(oldJSON, &oldMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(newJSON, &newMap); err != nil {
		return nil, err
	}
	patch := map[string]json.RawMessage{"id": newMap["id"]}
	for k, newVal := range newMap {
		if oldVal, ok := oldMap[k]; !ok || !bytes.Equal(oldVal, newVal) {
			patch[k] = newVal
		}
	}
	for k := range oldMap {
		if _, ok := newMap[k]; !ok {
			patch[k] = json.RawMessage("null")
		}
	}
	return patch, nil
}

// emitTaskListEvent marshals ev and writes it as an SSE message event.
func emitTaskListEvent(w http.ResponseWriter, flusher http.Flusher, ev v1.TaskListEvent) error { //nolint:gocritic // struct size grew with Repos field; refactor not worth it
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	flusher.Flush()
	return nil
}
