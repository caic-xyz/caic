// Generic HTTP handler wrappers that decode requests, validate, call a typed
// handler function, and encode JSON responses or structured errors.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"

	"github.com/maruel/caic/backend/internal/server/dto"
)

// handle wraps a typed handler function into an http.HandlerFunc. It reads the
// JSON body (with DisallowUnknownFields), populates path parameters via struct
// tags, validates, calls fn, and writes the JSON response or structured error.
func handle[In any, PtrIn interface {
	*In
	dto.Validatable
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

// handleWithTask wraps a typed handler that also needs the resolved *taskEntry.
// It parses {id}, looks up the task via s.getTask, then proceeds like handle.
func handleWithTask[In any, PtrIn interface {
	*In
	dto.Validatable
}, Out any](s *Server, fn func(context.Context, *taskEntry, PtrIn) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, err := s.getTask(r)
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
			s.notifyTaskChange()
		}
		writeJSONResponse(w, out, err)
	}
}

// readAndDecodeBody reads the request body and decodes JSON into input. It
// skips decoding for EmptyReq. Unknown JSON fields are rejected. Returns false
// if an error was written to the response.
func readAndDecodeBody[In any](w http.ResponseWriter, r *http.Request, input *In) bool {
	if _, isEmpty := any(input).(*dto.EmptyReq); isEmpty {
		return true
	}
	body, err := io.ReadAll(r.Body)
	if err2 := r.Body.Close(); err == nil {
		err = err2
	}
	if err != nil {
		writeError(w, dto.BadRequest("failed to read request body"))
		return false
	}
	if len(body) == 0 {
		return true
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(input); err != nil {
		slog.Error("failed to decode request body", "err", err)
		writeError(w, dto.BadRequest("invalid request body"))
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
