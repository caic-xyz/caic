// Tests for HTTP request body decompression middleware.

package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

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
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", bytes.NewReader([]byte("data")))
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
