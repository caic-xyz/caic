// Tests HTTP handler wrappers and bounded SSE response writes.

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/maruel/gomode/sse"
)

func TestWriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   api.ErrorCode
	}{
		{name: "bad request", err: &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "invalid request"}, wantStatus: http.StatusBadRequest, wantCode: api.CodeBadRequest},
		{name: "unknown repository", err: &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownRepository, Message: "unknown repo: mistyped"}, wantStatus: http.StatusBadRequest, wantCode: api.CodeUnknownRepository},
		{name: "unauthorized", err: &api.Error{Status: http.StatusUnauthorized, Code: api.CodeUnauthorized, Message: "authentication required"}, wantStatus: http.StatusUnauthorized, wantCode: api.CodeUnauthorized},
		{name: "forbidden", err: &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: "task" + " access denied"}, wantStatus: http.StatusForbidden, wantCode: api.CodeForbidden},
		{name: "not found", err: &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task" + " not found"}, wantStatus: http.StatusNotFound, wantCode: api.CodeNotFound},
		{name: "conflict", err: &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "task is not waiting"}, wantStatus: http.StatusConflict, wantCode: api.CodeConflict},
		{name: "internal", err: &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "backend unavailable"}, wantStatus: http.StatusInternalServerError, wantCode: api.CodeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			writeError(testHTTPContext(t), w, tt.err)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			var response api.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", response.Error.Code, tt.wantCode)
			}
		})
	}
}

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

func TestEmitTaskListEvent(t *testing.T) {
	t.Parallel()
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
			errs <- emitTaskListEvent(sse.New(w), &v1.TaskListEvent{Kind: "snapshot"})
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

		err := emitTaskListEvent(sse.New(w), &v1.TaskListEvent{Kind: "snapshot"})
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

		err := emitTaskListEvent(sse.New(w), &v1.TaskListEvent{Kind: "snapshot"})
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

		if err := emitTaskListEvent(sse.New(w), &v1.TaskListEvent{Kind: "snapshot"}); err != nil {
			t.Fatalf("emitTaskListEvent error = %v, want nil", err)
		}
		if got := w.Body.String(); got == "" {
			t.Error("event body is empty")
		}
	})
}

func TestTaskEventStreamWriteDeadline(t *testing.T) {
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
	stream := taskEventStream{
		w:      w,
		writer: sse.New(w),
	}
	errs := make(chan error, 1)
	go func() {
		errs <- stream.writeReady()
	}()

	select {
	case <-w.deadlineSet:
	case <-time.After(time.Second):
		t.Fatal("task SSE stream did not set a write deadline")
	}
	select {
	case <-w.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("task SSE stream did not begin writing to the pipe")
	}
	if err := server.SetWriteDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "write SSE ready event") {
			t.Errorf("writeReady error = %v, want write context", err)
		}
		netErr, ok := errors.AsType[net.Error](err)
		if !ok || !netErr.Timeout() {
			t.Errorf("writeReady error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked task SSE write did not return promptly")
	}
	if len(w.deadlines) != 2 || !w.deadlines[1].IsZero() {
		t.Errorf("deadlines = %v, want set then clear", w.deadlines)
	}
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
				apiErr, ok := errors.AsType[*api.Error](got)
				if !ok {
					t.Fatalf("toDTO returned %T, want *api.Error", got)
				}
				if apiErr.Status != tc.wantStatus {
					t.Errorf("Status = %d, want %d", apiErr.Status, tc.wantStatus)
				}
				if apiErr.Code != tc.wantCode {
					t.Errorf("Code = %q, want %q", apiErr.Code, tc.wantCode)
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

	t.Run("unknown_repository", func(t *testing.T) {
		t.Parallel()

		got := toDTO(&taskmgr.Error{Kind: taskmgr.KindBadRequest, Code: taskmgr.CodeUnknownRepository, Msg: "unknown repo: mistyped"})
		apiErr, ok := errors.AsType[*api.Error](got)
		if !ok {
			t.Fatalf("toDTO returned %T, want *api.Error", got)
		}
		if apiErr.Code != api.CodeUnknownRepository {
			t.Errorf("Code = %q, want %q", apiErr.Code, api.CodeUnknownRepository)
		}
		if apiErr.Status != http.StatusBadRequest {
			t.Errorf("Status = %d, want %d", apiErr.Status, http.StatusBadRequest)
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
		orig := &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "invalid input"}
		got := toDTO(orig)
		if !errors.Is(got, orig) {
			t.Errorf("toDTO should return the API error unchanged, got %v", got)
		}
	})

	t.Run("fallback_plain_error", func(t *testing.T) {
		t.Parallel()
		got := toDTO(errors.New("random failure"))
		apiErr, ok := errors.AsType[*api.Error](got)
		if !ok {
			t.Fatalf("toDTO returned %T, want *api.Error", got)
		}
		if apiErr.Status != http.StatusInternalServerError {
			t.Errorf("Status = %d, want 500", apiErr.Status)
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

func TestReadAndDecodeBody(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{"", `{}`, `{"instructions":"hello"}`, `{"instructions":"hello"}garbage`, `{"instructions":"hello"}{"ignored":true}`} {
			t.Run(body, func(t *testing.T) {
				t.Parallel()
				req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", strings.NewReader(body))
				var in v1.CompactReq
				if !readAndDecodeBody(httptest.NewRecorder(), req, &in) {
					t.Fatal("decode failed")
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{" ", `{`, `{"unknown":true}`} {
			t.Run(body, func(t *testing.T) {
				t.Parallel()
				req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", strings.NewReader(body))
				w := httptest.NewRecorder()
				var in v1.CompactReq
				if readAndDecodeBody(w, req, &in) || w.Code != 400 {
					t.Fatalf("accepted invalid body: %d", w.Code)
				}
			})
		}
	})
	t.Run("safeDecodeDiagnostics", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		ctx := context.WithValue(t.Context(), httpLoggerKey{}, slog.New(slog.NewJSONHandler(&logs, nil)))
		fieldName := "private-request-field"
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(`{"`+fieldName+`":true}`))
		var in v1.CompactReq
		w := httptest.NewRecorder()
		if readAndDecodeBody(w, req, &in) {
			t.Fatal("unknown field accepted")
		}
		if strings.Contains(logs.String(), fieldName) || strings.Contains(w.Body.String(), fieldName) {
			t.Fatal("raw input leaked in diagnostics")
		}
		if !strings.Contains(logs.String(), "offset") {
			t.Fatal("missing safe decode diagnostic")
		}
	})
	t.Run("oversizedValue", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", io.MultiReader(strings.NewReader(`{"instructions":"`), io.LimitReader(zeroSpaceReader{}, 33554432), strings.NewReader(`"}`)))
		var in v1.CompactReq
		w := httptest.NewRecorder()
		if readAndDecodeBody(w, req, &in) || w.Code != 413 {
			t.Fatalf("oversized JSON value accepted: %d", w.Code)
		}
	})

	t.Run("boundary", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{33554431, 33554432, 33554433} {
			for _, known := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/known=%t", n, known), func(t *testing.T) {
					t.Parallel()
					req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", io.MultiReader(strings.NewReader(`{}`), io.LimitReader(zeroSpaceReader{}, int64(n-2))))
					if known {
						req.ContentLength = int64(n)
					}
					w := httptest.NewRecorder()
					var in v1.CompactReq
					ok := readAndDecodeBody(w, req, &in)
					if n > 33554432 {
						if ok || w.Code != 413 {
							t.Fatalf("oversized body accepted: %d", w.Code)
						}
						var response api.ErrorResponse
						if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
							t.Fatal(err)
						}
						if response.Error.Code != api.CodeBadRequest {
							t.Fatalf("error code: %s", response.Error.Code)
						}
					} else if !ok {
						t.Fatalf("boundary rejected: %d", w.Code)
					}
				})
			}
		}
	})
	t.Run("nearMaximumImages", func(t *testing.T) {
		t.Parallel()
		// Two canonical base64 payloads total just under the existing 20 MiB decoded allowance.
		data := strings.Repeat("A", 13981012)
		body := `{"initialPrompt":{"images":[{"mediaType":"image/png","data":"` + data + `"},{"mediaType":"image/png","data":"` + data + `"}]},"harness":"claude"}`
		var in v1.CreateTaskReq
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", strings.NewReader(body))
		if !readAndDecodeBody(httptest.NewRecorder(), req, &in) {
			t.Fatal("maximum canonical images rejected")
		}
		if err := in.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bodyFailures", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			reader   io.Reader
			closeErr error
			canceled bool
		}{
			{name: "lateReadError", reader: &bodyErrorReader{data: []byte(`{}`), err: io.ErrUnexpectedEOF}},
			{name: "readAfterObject", reader: io.MultiReader(strings.NewReader(`{}`), &bodyErrorReader{err: io.ErrUnexpectedEOF})},
			{name: "closeError", reader: strings.NewReader(`{}`), closeErr: io.ErrClosedPipe},
			{name: "readBeforeDecode", reader: &bodyErrorReader{data: []byte(`invalid`), err: io.ErrUnexpectedEOF}},
			{name: "canceled", reader: strings.NewReader(`{}`), canceled: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(testHTTPContext(t))
				t.Cleanup(cancel)
				if tc.canceled {
					cancel()
				}
				body := &requestTestBody{Reader: tc.reader, closeErr: tc.closeErr}
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", body)
				w := httptest.NewRecorder()
				var in v1.CompactReq
				if readAndDecodeBody(w, req, &in) || w.Code != 400 {
					t.Fatalf("failure accepted: %d", w.Code)
				}
				if response := decodeError(t, w); response.Message != "failed to read request body" {
					t.Fatalf("message: %s", response.Message)
				}
			})
		}
	})
	t.Run("emptyReqIgnoresBody", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", &bodyErrorReader{err: io.ErrUnexpectedEOF})
		if !readAndDecodeBody(httptest.NewRecorder(), req, &api.EmptyReq{}) {
			t.Fatal("EmptyReq behavior changed")
		}
	})
	t.Run("compressed", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			size    int64
			corrupt bool
			status  int
		}{
			{name: "valid", size: 2, status: 200},
			{name: "decompressionBomb", size: 33554433, status: 413},
			{name: "lateChecksumError", size: 65536, corrupt: true, status: 400},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				if _, err := gz.Write([]byte(`{}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(gz, io.LimitReader(zeroSpaceReader{}, tc.size-2)); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				if tc.corrupt {
					compressed.Bytes()[compressed.Len()-8] ^= 1
				}
				req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", &compressed)
				req.Header.Set("Content-Encoding", "gzip")
				w := httptest.NewRecorder()
				h := decompressMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var in v1.CompactReq
					_ = readAndDecodeBody(w, r, &in)
				}))
				h.ServeHTTP(w, req)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d", w.Code, tc.status)
				}
			})
		}
	})
}

type zeroSpaceReader struct{}

func (zeroSpaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type bodyErrorReader struct {
	data []byte
	err  error
}

func (r *bodyErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

type requestTestBody struct {
	io.Reader

	closeErr error
}

func (r *requestTestBody) Close() error { return r.closeErr }
