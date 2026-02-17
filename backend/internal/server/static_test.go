package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// brCompress returns data brotli-compressed at max quality.
func brCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	indexContent = []byte("<html>hello</html>")
	appContent   = []byte("console.log('hi')")
	cssContent   = []byte("body{}")
	iconContent  = []byte("icon")
)

// testFS returns a brotli-only FS matching what compress_dist.py produces.
func testFS(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"index.html.br":       {Data: brCompress(t, indexContent)},
		"favicon.svg.br":      {Data: brCompress(t, iconContent)},
		"assets/app.js.br":    {Data: brCompress(t, appContent)},
		"assets/style.css.br": {Data: brCompress(t, cssContent)},
	}
}

func TestStaticHandler(t *testing.T) {
	h := newStaticHandler(testFS(t))

	t.Run("BrotliDirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "br, gzip")
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("Content-Encoding = %q, want %q", got, "br")
		}
		if got := w.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
			t.Errorf("Content-Type = %q, want %q", got, "text/javascript; charset=utf-8")
		}
		body := decompressBrotli(t, w.Body.Bytes())
		if !bytes.Equal(body, appContent) {
			t.Errorf("body = %q, want %q", body, appContent)
		}
	})

	t.Run("TranscodeZstd", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd")
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "zstd" {
			t.Errorf("Content-Encoding = %q, want %q", got, "zstd")
		}
		body := decompressZstd(t, w.Body.Bytes())
		if !bytes.Equal(body, appContent) {
			t.Errorf("body = %q, want %q", body, appContent)
		}
	})

	t.Run("TranscodeGzip", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		body := decompressGzip(t, w.Body.Bytes())
		if !bytes.Equal(body, appContent) {
			t.Errorf("body = %q, want %q", body, appContent)
		}
	})

	t.Run("FallbackIdentity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/favicon.svg", http.NoBody)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty", got)
		}
		if !bytes.Equal(w.Body.Bytes(), iconContent) {
			t.Errorf("body = %q, want %q", w.Body.Bytes(), iconContent)
		}
	})

	t.Run("SPAFallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/some/deep/route", http.NoBody)
		req.Header.Set("Accept-Encoding", "br")
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("Content-Encoding = %q, want %q", got, "br")
		}
		body := decompressBrotli(t, w.Body.Bytes())
		if !bytes.Equal(body, indexContent) {
			t.Errorf("body = %q, want %q", body, indexContent)
		}
	})

	t.Run("VaryHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/favicon.svg", http.NoBody)
		w := httptest.NewRecorder()
		h(w, req)

		if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
	})

	t.Run("CacheControlAssets", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", http.NoBody)
		w := httptest.NewRecorder()
		h(w, req)

		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q, want immutable", got)
		}
	})

	t.Run("CacheControlRoot", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		w := httptest.NewRecorder()
		h(w, req)

		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
		}
	})

	t.Run("RootServesIndex", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		// No Accept-Encoding → identity transcoding.
		if !bytes.Equal(w.Body.Bytes(), indexContent) {
			t.Errorf("body = %q, want index.html content", w.Body.Bytes())
		}
	})

	t.Run("BrotliPreferredOverZstd", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd, br, gzip")
		w := httptest.NewRecorder()
		h(w, req)

		// Brotli is always preferred since it's the native format.
		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("Content-Encoding = %q, want %q", got, "br")
		}
	})
}

func TestParseAcceptEncoding(t *testing.T) {
	tests := []struct {
		header string
		want   map[string]bool
	}{
		{"gzip, br", map[string]bool{"gzip": true, "br": true}},
		{"zstd;q=1.0, gzip;q=0.5", map[string]bool{"zstd": true, "gzip": true}},
		{"", map[string]bool{}},
		{"identity", map[string]bool{"identity": true}},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			got := parseAcceptEncoding(tt.header)
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseAcceptEncoding(%q)[%q] = %v, want %v", tt.header, k, got[k], v)
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseAcceptEncoding(%q) has %d entries, want %d", tt.header, len(got), len(tt.want))
			}
		})
	}
}

// Decompression helpers for roundtrip verification.

func decompressBrotli(t *testing.T, data []byte) []byte {
	t.Helper()
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("brotli decompress: %v", err)
	}
	return out
}

func decompressZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("zstd decompress: %v", err)
	}
	return out
}

func decompressGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := kgzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gzip decompress: %v", err)
	}
	return out
}
