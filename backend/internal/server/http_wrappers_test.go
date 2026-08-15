// Tests for generic HTTP handler wrappers, including error mapping.

package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

func testHTTPContext(t *testing.T) context.Context {
	return context.WithValue(t.Context(), httpLoggerKey{}, slog.New(slog.DiscardHandler))
}

type deadlineResponseWriter struct {
	*httptest.ResponseRecorder

	deadlines []time.Time
	writeErr  error
	flushErr  error
}

func (w *deadlineResponseWriter) Write(b []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(b)
}

func (w *deadlineResponseWriter) Flush() {}

func (w *deadlineResponseWriter) FlushError() error {
	return w.flushErr
}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

type pipeResponseWriter struct {
	conn net.Conn

	header       http.Header
	deadlineSet  chan time.Time
	deadlines    []time.Time
	writeStarted chan struct{}
}

func (w *pipeResponseWriter) Header() http.Header {
	return w.header
}

func (w *pipeResponseWriter) Write(b []byte) (int, error) {
	select {
	case w.writeStarted <- struct{}{}:
	default:
	}
	return w.conn.Write(b)
}

func (w *pipeResponseWriter) WriteHeader(int) {}

func (w *pipeResponseWriter) Flush() {}

func (w *pipeResponseWriter) SetWriteDeadline(deadline time.Time) error {
	err := w.conn.SetWriteDeadline(deadline)
	w.deadlines = append(w.deadlines, deadline)
	if !deadline.IsZero() {
		w.deadlineSet <- deadline
	}
	return err
}

func TestEmitTaskListEvent(t *testing.T) { //nolint:tparallel // UnsupportedDeadline mutates the global slog default.
	t.Run("PipeDeadlineStopsBlockedWrite", func(t *testing.T) {
		t.Parallel()
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		t.Cleanup(func() { _ = client.Close() })
		w := &pipeResponseWriter{
			conn:         server,
			header:       make(http.Header),
			deadlineSet:  make(chan time.Time, 1),
			writeStarted: make(chan struct{}, 1),
		}
		before := time.Now()
		errs := make(chan error, 1)
		go func() {
			errs <- emitTaskListEvent(testHTTPContext(t), w, http.NewResponseController(w), &v1.TaskListEvent{Kind: "snapshot"})
		}()

		var deadline time.Time
		select {
		case deadline = <-w.deadlineSet:
		case <-time.After(time.Second):
			t.Fatal("emitTaskListEvent did not set a write deadline")
		}
		if got := deadline.Sub(before); got < 4*time.Second || got > 6*time.Second {
			t.Errorf("write deadline offset = %v, want approximately 5s", got)
		}
		select {
		case <-w.writeStarted:
		case <-time.After(time.Second):
			t.Fatal("emitTaskListEvent did not begin writing to the pipe")
		}
		if err := server.SetWriteDeadline(time.Now()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errs:
			if !strings.Contains(err.Error(), "write task-list event") {
				t.Errorf("emitTaskListEvent error = %v, want delivery context", err)
			}
			netErr, ok := errors.AsType[net.Error](err)
			if !ok || !netErr.Timeout() {
				t.Errorf("emitTaskListEvent error = %v, want timeout", err)
			}
		case <-time.After(time.Second):
			t.Fatal("blocked pipe write did not return promptly")
		}
		if len(w.deadlines) != 2 || !w.deadlines[1].IsZero() {
			t.Errorf("deadlines = %v, want set then clear", w.deadlines)
		}
	})

	t.Run("WriteError", func(t *testing.T) {
		t.Parallel()
		want := errors.New("client write failed")
		w := &deadlineResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
			writeErr:         want,
		}

		err := emitTaskListEvent(testHTTPContext(t), w, http.NewResponseController(w), &v1.TaskListEvent{Kind: "snapshot"})
		if !errors.Is(err, want) {
			t.Fatalf("emitTaskListEvent error = %v, want %v", err, want)
		}
		if !strings.Contains(err.Error(), "write task-list event") {
			t.Errorf("emitTaskListEvent error = %v, want delivery context", err)
		}
		if len(w.deadlines) != 2 || !w.deadlines[1].IsZero() {
			t.Errorf("deadlines = %v, want set then clear", w.deadlines)
		}
	})

	t.Run("FlushError", func(t *testing.T) {
		t.Parallel()
		want := errors.New("encoder flush failed")
		w := &deadlineResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
			flushErr:         want,
		}

		err := emitTaskListEvent(testHTTPContext(t), w, http.NewResponseController(w), &v1.TaskListEvent{Kind: "snapshot"})
		if !errors.Is(err, want) {
			t.Errorf("emitTaskListEvent error = %v, want %v", err, want)
		}
		if !strings.Contains(err.Error(), "write task-list event") {
			t.Errorf("emitTaskListEvent error = %v, want delivery context", err)
		}
		if len(w.deadlines) != 2 || !w.deadlines[1].IsZero() {
			t.Errorf("deadlines = %v, want set then clear", w.deadlines)
		}
	})

	t.Run("UnsupportedDeadline", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()

		if err := emitTaskListEvent(testHTTPContext(t), w, http.NewResponseController(w), &v1.TaskListEvent{Kind: "snapshot"}); err != nil {
			t.Fatalf("emitTaskListEvent error = %v, want nil", err)
		}
		if got := w.Body.String(); got == "" {
			t.Error("event body is empty")
		}
	})
}

func TestToDTO(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := toDTO(nil); got != nil {
			t.Errorf("toDTO(nil) = %v, want nil", got)
		}
	})

	t.Run("kinds", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name       string
			kind       taskmgr.ErrorKind
			wantStatus int
			wantCode   api.ErrorCode
		}{
			{"not_found", taskmgr.KindNotFound, http.StatusNotFound, api.CodeNotFound},
			{"conflict", taskmgr.KindConflict, http.StatusConflict, api.CodeConflict},
			{"bad_request", taskmgr.KindBadRequest, http.StatusBadRequest, api.CodeBadRequest},
			{"internal", taskmgr.KindInternal, http.StatusInternalServerError, api.CodeInternalError},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := toDTO(&taskmgr.Error{Kind: tc.kind, Msg: "boom"})
				ews, ok := errors.AsType[api.ErrorWithStatus](got)
				if !ok {
					t.Fatalf("toDTO returned %T, want api.ErrorWithStatus", got)
				}
				if ews.StatusCode() != tc.wantStatus {
					t.Errorf("StatusCode() = %d, want %d", ews.StatusCode(), tc.wantStatus)
				}
				if ews.Code() != tc.wantCode {
					t.Errorf("Code() = %q, want %q", ews.Code(), tc.wantCode)
				}
			})
		}
	})

	t.Run("not_found_trims_suffix", func(t *testing.T) {
		t.Parallel()
		got := toDTO(&taskmgr.Error{Kind: taskmgr.KindNotFound, Msg: "task 42 not found"})
		if got.Error() != "task 42 not found" {
			t.Errorf("Error() = %q, want %q", got.Error(), "task 42 not found")
		}
	})

	t.Run("internal_preserves_wrapped", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("connection refused")
		got := toDTO(&taskmgr.Error{Kind: taskmgr.KindInternal, Msg: "sync to default", Err: inner})
		if got.Error() != "sync to default: connection refused" {
			t.Errorf("Error() = %q, want it to include the wrapped error", got.Error())
		}
	})

	t.Run("already_api", func(t *testing.T) {
		t.Parallel()
		orig := api.BadRequest("invalid input")
		got := toDTO(orig)
		if !errors.Is(got, orig) {
			t.Errorf("toDTO should return the API error unchanged, got %v", got)
		}
	})

	t.Run("fallback_plain_error", func(t *testing.T) {
		t.Parallel()
		got := toDTO(errors.New("random failure"))
		ews, ok := errors.AsType[api.ErrorWithStatus](got)
		if !ok {
			t.Fatalf("toDTO returned %T, want api.ErrorWithStatus", got)
		}
		if ews.StatusCode() != http.StatusInternalServerError {
			t.Errorf("StatusCode() = %d, want 500", ews.StatusCode())
		}
		if got.Error() != "random failure" {
			t.Errorf("Error() = %q, want %q", got.Error(), "random failure")
		}
	})
}

func TestComputeTaskPatch(t *testing.T) {
	t.Parallel()
	t.Run("ChangedFields", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","state":"running","costUSD":0.0}`
		new_ := `{"id":"abc","state":"waiting","costUSD":1.5}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"abc"` {
			t.Errorf("id = %s, want \"abc\"", patch["id"])
		}
		if string(patch["state"]) != `"waiting"` {
			t.Errorf("state = %s, want \"waiting\"", patch["state"])
		}
		if string(patch["costUSD"]) != `1.5` {
			t.Errorf("costUSD = %s, want 1.5", patch["costUSD"])
		}
		// Unchanged field should not be in patch
		if _, ok := patch["costUSD"]; !ok {
			t.Error("costUSD should be in patch (changed from 0.0 to 1.5)")
		}
	})
	t.Run("UnchangedFieldsOmitted", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","state":"running","repo":"myrepo"}`
		new_ := `{"id":"abc","state":"waiting","repo":"myrepo"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := patch["repo"]; ok {
			t.Error("repo should not be in patch (unchanged)")
		}
		if _, ok := patch["state"]; !ok {
			t.Error("state should be in patch (changed)")
		}
	})
	t.Run("RemovedFieldSetToNull", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"abc","error":"boom"}`
		new_ := `{"id":"abc"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["error"]) != "null" {
			t.Errorf("removed field error = %s, want null", patch["error"])
		}
	})
	t.Run("AlwaysIncludesID", func(t *testing.T) {
		t.Parallel()
		old := `{"id":"xyz","state":"running"}`
		new_ := `{"id":"xyz","state":"purged"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"xyz"` {
			t.Errorf("id = %s, want \"xyz\"", patch["id"])
		}
	})
}
