// Generic HTTP handler wrappers that decode requests, validate, call a typed handler function, and encode JSON responses or structured errors.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/tasks"
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
		slog.Error("failed to decode request body", "err", err)
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
