// Tests for HTTP transport middleware: response compression and request decompression.

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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/caic-xyz/caic/backend/internal/httplog"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

type flushErrorWriteCloser struct {
	err      error
	closeErr error
}

func (w *flushErrorWriteCloser) Write(b []byte) (int, error) {
	return len(b), nil
}

func (w *flushErrorWriteCloser) Close() error {
	return w.closeErr
}

func (w *flushErrorWriteCloser) Flush() error {
	return w.err
}

// finishFailResponseWriter starts failing after the handler's buffered write.
type finishFailResponseWriter struct {
	*httptest.ResponseRecorder

	err          error
	failing      bool
	failedWrites int
}

func (w *finishFailResponseWriter) Write(p []byte) (int, error) {
	if w.failing {
		w.failedWrites++
		return 0, w.err
	}
	return w.ResponseRecorder.Write(p)
}

func jsonHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// sseHandler mirrors the real SSE handlers: set headers, flush (sends
// headers to the wire), then write event data and flush again.
func sseHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter %T does not implement http.Flusher", w)
			return
		}
		flusher.Flush()
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
		flusher.Flush()
	}
}

func precompressedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("already-compressed"))
	}
}

func TestCompressMiddleware(t *testing.T) {
	t.Parallel()
	t.Run("FinalizationFailure", func(t *testing.T) {
		t.Parallel()
		for _, enc := range []string{"br", "gzip", "zstd"} {
			t.Run(enc, func(t *testing.T) {
				t.Parallel()
				const querySecret = "private-query-canary"
				const payload = "private-payload-canary"
				want := errors.New("transport write failed")
				var logs bytes.Buffer
				log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
				ctx := context.WithValue(t.Context(), httpLoggerKey{}, log)
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/download?token="+querySecret, http.NoBody)
				req.Header.Set("Accept-Encoding", enc)
				wire := &finishFailResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: want}
				var beforeClose []byte
				handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					if n, err := w.Write([]byte(payload)); err != nil || n != len(payload) {
						t.Fatalf("buffered Write = %d,%v", n, err)
					}
					beforeClose = bytes.Clone(wire.Body.Bytes())
					wire.failing = true
				})
				h := httplog.Handler{Handler: compressMiddleware(handler), Logger: log}
				h.ServeHTTP(wire, req)
				if wire.failedWrites == 0 {
					t.Fatal("compressor finalization did not exercise transport failure")
				}
				if wire.Code != http.StatusOK || !bytes.Equal(wire.Body.Bytes(), beforeClose) {
					t.Fatal("finalization rewrote the committed response")
				}
				if strings.Contains(logs.String(), querySecret) || strings.Contains(logs.String(), payload) {
					t.Fatal("query or payload leaked into logs")
				}
				var records []map[string]any
				for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					records = append(records, record)
				}
				if len(records) != 2 {
					t.Fatalf("got %d log records; need finalization failure then HTTP access", len(records))
				}
				failure := records[0]
				if failure["level"] != "ERROR" || failure["m"] != "GET" || failure["p"] != "/download" || failure["encoding"] != enc || failure["err"] != want.Error() {
					t.Fatalf("finalization record=%v", failure)
				}
				access := records[1]
				if access["msg"] != "http" || access["s"] != float64(http.StatusOK) {
					t.Fatalf("access record lost committed status: %v", access)
				}
			})
		}
	})
	t.Run("FinalizationSuccess", func(t *testing.T) {
		t.Parallel()
		for _, enc := range []string{"br", "gzip", "zstd"} {
			t.Run(enc, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				log := slog.New(slog.NewJSONHandler(&logs, nil))
				ctx := context.WithValue(t.Context(), httpLoggerKey{}, log)
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", http.NoBody)
				req.Header.Set("Accept-Encoding", enc)
				compressMiddleware(jsonHandler()).ServeHTTP(httptest.NewRecorder(), req)
				if logs.Len() != 0 {
					t.Fatal("successful finalization emitted an error log")
				}
			})
		}
	})
	t.Run("WebSocketUpgrade", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&logs, nil))
		ctx := context.WithValue(t.Context(), httpLoggerKey{}, log)
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/ws", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		h := compressMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) }))
		wire := httptest.NewRecorder()
		h.ServeHTTP(wire, req)
		if wire.Code != http.StatusSwitchingProtocols || wire.Header().Get("Content-Encoding") != "" || wire.Body.Len() != 0 || logs.Len() != 0 {
			t.Fatal("compression interfered with upgrade")
		}
	})

	t.Run("FinishPreservesCause", func(t *testing.T) {
		t.Parallel()
		want := errors.New("transport close failed")
		cw := &compressWriter{
			ResponseWriter: httptest.NewRecorder(),
			encoding:       "gzip",
			writer:         &flushErrorWriteCloser{closeErr: fmt.Errorf("codec close: %w", want)},
			headerSent:     true,
		}
		if err := cw.finish(); !errors.Is(err, want) {
			t.Fatalf("finish error=%v; wrapped cause lost", err)
		}
	})

	t.Run("FlushError", func(t *testing.T) {
		t.Parallel()
		want := errors.New("encoder flush failed")
		cw := &compressWriter{
			ResponseWriter: httptest.NewRecorder(),
			writer:         &flushErrorWriteCloser{err: want},
			headerSent:     true,
		}

		err := http.NewResponseController(cw).Flush()
		if !errors.Is(err, want) {
			t.Errorf("Flush error = %v, want %v", err, want)
		}
	})

	t.Run("Zstd", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "zstd" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "zstd")
		}

		dec, err := zstd.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		body, err := io.ReadAll(dec)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"status":"ok"}` {
			t.Errorf("body = %q, want %q", string(body), `{"status":"ok"}`)
		}
	})

	t.Run("Brotli", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "br")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "br")
		}

		body, err := io.ReadAll(brotli.NewReader(w.Body))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"status":"ok"}` {
			t.Errorf("body = %q, want %q", string(body), `{"status":"ok"}`)
		}
	})

	t.Run("Gzip", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "gzip")
		}

		gr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(gr)
		_ = gr.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"status":"ok"}` {
			t.Errorf("body = %q, want %q", string(body), `{"status":"ok"}`)
		}
	})

	t.Run("Preference", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip, br, zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "zstd" {
			t.Errorf("Content-Encoding = %q, want %q (zstd preferred)", got, "zstd")
		}
	})

	t.Run("CompressesSSE", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(sseHandler(t))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "zstd" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "zstd")
		}

		dec, err := zstd.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		body, err := io.ReadAll(dec)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "event: ping\ndata: {}\n\n" {
			t.Errorf("body = %q, want SSE payload", string(body))
		}
	})

	t.Run("SkipsPrecompressed", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(precompressedHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd, br, gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("Content-Encoding = %q, want %q (original)", got, "br")
		}
		if got := w.Body.String(); got != "already-compressed" {
			t.Errorf("body = %q, want %q", got, "already-compressed")
		}
	})

	t.Run("NoAcceptEncoding", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty", got)
		}
		if got := w.Body.String(); got != `{"status":"ok"}` {
			t.Errorf("body = %q, want uncompressed JSON", got)
		}
	})

	t.Run("VaryHeader", func(t *testing.T) {
		t.Parallel()
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
	})
}

// echoHandler reads the request body and echoes it back.
func echoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(body)
	}
}

func TestDecompressMiddleware(t *testing.T) {
	t.Parallel()
	t.Run("Gzip", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		_, _ = gz.Write([]byte("hello gzip"))
		_ = gz.Close()

		h := decompressMiddleware(echoHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", &buf)
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Body.String(); got != "hello gzip" {
			t.Errorf("body = %q, want %q", got, "hello gzip")
		}
	})

	t.Run("Zstd", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		enc, _ := zstd.NewWriter(&buf)
		_, _ = enc.Write([]byte("hello zstd"))
		_ = enc.Close()

		h := decompressMiddleware(echoHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", &buf)
		req.Header.Set("Content-Encoding", "zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Body.String(); got != "hello zstd" {
			t.Errorf("body = %q, want %q", got, "hello zstd")
		}
	})

	t.Run("Brotli", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		bw := brotli.NewWriter(&buf)
		_, _ = bw.Write([]byte("hello brotli"))
		_ = bw.Close()

		h := decompressMiddleware(echoHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", &buf)
		req.Header.Set("Content-Encoding", "br")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Body.String(); got != "hello brotli" {
			t.Errorf("body = %q, want %q", got, "hello brotli")
		}
	})

	t.Run("Unsupported", func(t *testing.T) {
		t.Parallel()
		h := decompressMiddleware(echoHandler())
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/", bytes.NewReader([]byte("data")))
		req.Header.Set("Content-Encoding", "deflate")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("None", func(t *testing.T) {
		t.Parallel()
		h := decompressMiddleware(echoHandler())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", bytes.NewReader([]byte("plain body")))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Body.String(); got != "plain body" {
			t.Errorf("body = %q, want %q", got, "plain body")
		}
	})
}
