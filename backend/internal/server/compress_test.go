// Tests for HTTP response compression middleware.

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

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
	t.Run("Zstd", func(t *testing.T) {
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip, br, zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Content-Encoding"); got != "zstd" {
			t.Errorf("Content-Encoding = %q, want %q (zstd preferred)", got, "zstd")
		}
	})

	t.Run("CompressesSSE", func(t *testing.T) {
		h := compressMiddleware(sseHandler(t))
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(precompressedHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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
		h := compressMiddleware(jsonHandler())
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
	})
}
