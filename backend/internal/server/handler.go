// Generic HTTP handler wrappers that decode requests, validate, call a typed
// handler function, and encode JSON responses or structured errors.
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

type validatable interface {
	validate() error
}

// emptyReq is used for endpoints that take no request body.
type emptyReq struct{}

func (emptyReq) validate() error { return nil }

// inputReq is the request body for POST /api/v1/tasks/{id}/input.
type inputReq struct {
	Prompt string `json:"prompt"`
}

func (r *inputReq) validate() error {
	if r.Prompt == "" {
		return badRequest("prompt is required")
	}
	return nil
}

// createTaskReq is the request body for POST /api/v1/tasks.
type createTaskReq struct {
	Prompt string `json:"prompt"`
	Repo   string `json:"repo"`
}

func (r *createTaskReq) validate() error {
	if r.Prompt == "" {
		return badRequest("prompt is required")
	}
	if r.Repo == "" {
		return badRequest("repo is required")
	}
	return nil
}

// statusResp is a common response for mutation endpoints.
type statusResp struct {
	Status string `json:"status"`
}

// handle wraps a typed handler function into an http.HandlerFunc. It decodes
// the JSON request body (skipping decode for emptyReq), validates, calls fn,
// and encodes the JSON response or writes a structured error.
func handle[In any, PtrIn interface {
	*In
	validatable
}, Out any](fn func(context.Context, PtrIn) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := PtrIn(new(In))
		if _, isEmpty := any(in).(*emptyReq); !isEmpty {
			if err := json.NewDecoder(r.Body).Decode(in); err != nil && err != io.EOF {
				writeError(w, badRequest(err.Error()))
				return
			}
		}
		if err := in.validate(); err != nil {
			writeError(w, err)
			return
		}
		out, err := fn(r.Context(), in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, out)
	}
}

// handleWithTask wraps a typed handler that also needs the resolved *taskEntry.
// It parses {id}, looks up the task via s.getTask, then proceeds like handle.
func handleWithTask[In any, PtrIn interface {
	*In
	validatable
}, Out any](s *Server, fn func(context.Context, *taskEntry, PtrIn) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, err := s.getTask(r)
		if err != nil {
			writeError(w, err)
			return
		}
		in := PtrIn(new(In))
		if _, isEmpty := any(in).(*emptyReq); !isEmpty {
			if err := json.NewDecoder(r.Body).Decode(in); err != nil && err != io.EOF {
				writeError(w, badRequest(err.Error()))
				return
			}
		}
		if err := in.validate(); err != nil {
			writeError(w, err)
			return
		}
		out, err := fn(r.Context(), entry, in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, out)
	}
}
