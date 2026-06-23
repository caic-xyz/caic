// Tests for static file serving middleware.

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// brCompress returns data brotli-compressed at max quality.
func brCompress(t *testing.T, data []byte) []byte {
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
	indexContent = []byte("<html><head></head><body>hello</body></html>")
	appContent   = []byte("console.log('hi')")
	cssContent   = []byte("body{}")
	iconContent  = []byte("icon")
)

// testFS returns a brotli-only FS matching what compress_dist.py produces.
func testFS(t *testing.T) fstest.MapFS {
	return fstest.MapFS{
		"index.html.br":       {Data: brCompress(t, indexContent)},
		"favicon.svg.br":      {Data: brCompress(t, iconContent)},
		"assets/app.js.br":    {Data: brCompress(t, appContent)},
		"assets/style.css.br": {Data: brCompress(t, cssContent)},
	}
}

func TestAssetHandler(t *testing.T) {
	t.Parallel()
	h := &assetHandler{dist: testFS(t)}

	t.Run("BrotliDirect", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "br, gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

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
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

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
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

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

	t.Run("RootFileIdentity", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/favicon.svg", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

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

	t.Run("MissingAssetNotFound", func(t *testing.T) {
		t.Parallel()
		// A missing asset is a 404; it must not fall back to the SPA document.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/missing.js", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("VaryHeader", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/favicon.svg", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
	})

	t.Run("CacheControlAssets", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q, want immutable", got)
		}
	})

	t.Run("BrotliPreferredOverZstd", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/app.js", http.NoBody)
		req.Header.Set("Accept-Encoding", "zstd, br, gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// Brotli is always preferred since it's the native format.
		if got := w.Header().Get("Content-Encoding"); got != "br" {
			t.Errorf("Content-Encoding = %q, want %q", got, "br")
		}
	})
}

func TestRootAssetNames(t *testing.T) {
	t.Parallel()
	names, err := rootAssetNames(testFS(t))
	if err != nil {
		t.Fatalf("rootAssetNames: %v", err)
	}
	// Root-level .br files only, with index.html (SPA-owned) and the assets/
	// subtree excluded.
	want := map[string]struct{}{"favicon.svg": {}}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, n := range names {
		if _, ok := want[n]; !ok {
			t.Errorf("unexpected root asset %q", n)
		}
	}
}

func TestSpaHandler(t *testing.T) {
	t.Parallel()
	h := &spaHandler{dist: testFS(t)}

	t.Run("DeepRoute", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/some/deep/route", http.NoBody)
		req.Header.Set("Accept-Encoding", "br")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		// The SPA document is served identity-encoded (not brotli) so the
		// bootstrap can be injected; compressMiddleware compresses it downstream.
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (identity)", got)
		}
		body := w.Body.Bytes()
		if !bytes.Contains(body, []byte("window.__CAIC_BOOTSTRAP__")) {
			t.Errorf("body = %q, want injected bootstrap", body)
		}
		if !bytes.Contains(body, []byte("<body>hello</body>")) {
			t.Errorf("body = %q, want index.html content", body)
		}
	})

	t.Run("CacheControlNoCache", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
		}
	})

	t.Run("RootServesIndex", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		// The root document is the personalized SPA index with the bootstrap
		// injected before </head>.
		want := []byte(`<html><head><script>window.__CAIC_BOOTSTRAP__=` +
			`{"authProviders":[]};</script></head><body>hello</body></html>`)
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("body = %q, want %q", w.Body.Bytes(), want)
		}
	})

	t.Run("BootstrapInjection", func(t *testing.T) {
		t.Parallel()
		hb := &spaHandler{dist: testFS(t), authProviders: []string{"github"}}
		// Even with brotli accepted, the personalized document is served
		// identity-encoded so compressMiddleware can compress it downstream.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Header.Set("Accept-Encoding", "br")
		w := httptest.NewRecorder()
		hb.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (identity)", got)
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("Content-Type = %q, want text/html", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", got)
		}
		want := []byte(`<html><head><script>window.__CAIC_BOOTSTRAP__=` +
			`{"authProviders":["github"]};</script></head><body>hello</body></html>`)
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("body = %q, want %q", w.Body.Bytes(), want)
		}
	})
}

func TestSpaHandlerBootstrapSnippet(t *testing.T) {
	t.Parallel()
	const prefix = "<script>window.__CAIC_BOOTSTRAP__="
	const suffix = ";</script>"
	parse := func(t *testing.T, b []byte) v1.AuthBootstrapResp {
		str := string(b)
		if !strings.HasPrefix(str, prefix) || !strings.HasSuffix(str, suffix) {
			t.Fatalf("snippet = %q, missing script wrapper", str)
		}
		var data v1.AuthBootstrapResp
		if err := json.Unmarshal(b[len(prefix):len(b)-len(suffix)], &data); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return data
	}

	t.Run("anonymous", func(t *testing.T) {
		t.Parallel()
		// nil authProviders must still marshal to [] so the frontend never sees null.
		h := &spaHandler{}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		data := parse(t, h.bootstrapSnippet(req))
		if data.User != nil {
			t.Errorf("user = %+v, want nil", data.User)
		}
		if data.AuthProviders == nil || len(data.AuthProviders) != 0 {
			t.Errorf("authProviders = %v, want empty non-nil slice", data.AuthProviders)
		}
	})

	t.Run("user", func(t *testing.T) {
		t.Parallel()
		h := &spaHandler{authProviders: []string{"github"}}
		u := &auth.User{ID: "u1", Provider: auth.ProviderGitHub, Username: "alice", AvatarURL: "https://x/a.png"}
		req := httptest.NewRequestWithContext(auth.NewContext(t.Context(), u), http.MethodGet, "/", http.NoBody)
		data := parse(t, h.bootstrapSnippet(req))
		if data.User == nil {
			t.Fatalf("user = nil, want alice")
		}
		if data.User.Username != "alice" || data.User.Provider != "github" || data.User.ID != "u1" {
			t.Errorf("user = %+v, want alice/github/u1", data.User)
		}
	})

	t.Run("escapes script terminator", func(t *testing.T) {
		t.Parallel()
		// A username containing </script> must not break out of the element.
		h := &spaHandler{}
		u := &auth.User{ID: "u1", Provider: auth.ProviderGitHub, Username: "</script><evil>"}
		req := httptest.NewRequestWithContext(auth.NewContext(t.Context(), u), http.MethodGet, "/", http.NoBody)
		snippet := h.bootstrapSnippet(req)
		if n := bytes.Count(snippet, []byte("</script>")); n != 1 {
			t.Errorf("</script> count = %d, want 1 (payload not escaped)", n)
		}
	})
}

func TestInjectIntoHead(t *testing.T) {
	t.Parallel()
	snippet := []byte("<X>")
	tests := []struct {
		name string
		html string
		want string
	}{
		{"head", "<html><head></head><body></body></html>", "<html><head><X></head><body></body></html>"},
		{"nohead", "<html><body></body></html>", "<X><html><body></body></html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := injectIntoHead([]byte(tt.html), snippet); string(got) != tt.want {
				t.Errorf("injectIntoHead = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		html := []byte("<html></html>")
		if got := injectIntoHead(html, nil); !bytes.Equal(got, html) {
			t.Errorf("injectIntoHead with empty snippet = %q, want unchanged", got)
		}
	})
}

func TestParseAcceptEncoding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header string
		want   map[string]struct{}
	}{
		{"gzip, br", map[string]struct{}{"gzip": {}, "br": {}}},
		{"zstd;q=1.0, gzip;q=0.5", map[string]struct{}{"zstd": {}, "gzip": {}}},
		{"", map[string]struct{}{}},
		{"identity", map[string]struct{}{"identity": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			t.Parallel()
			got := parseAcceptEncoding(tt.header)
			for k := range tt.want {
				if _, ok := got[k]; !ok {
					t.Errorf("parseAcceptEncoding(%q) missing key %q", tt.header, k)
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
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("brotli decompress: %v", err)
	}
	return out
}

func decompressZstd(t *testing.T, data []byte) []byte {
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
	r, err := gzip.NewReader(bytes.NewReader(data))
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
