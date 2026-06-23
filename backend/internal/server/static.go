// Precompressed static file handler for embedded frontend assets.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// transcodeEntry holds a lazily-computed transcoded variant.
type transcodeEntry struct {
	once sync.Once
	data []byte
	err  error
}

// assetHandler serves the embedded frontend's precompressed static assets:
// hashed bundles under /assets/ and the root-level files (favicon, manifest,
// service worker, icons).
//
// Files exist on disk only as .br; the handler serves brotli directly when
// accepted and lazily transcodes to zstd/gzip/identity otherwise. A request for
// a file that does not exist is a 404 — unlike the SPA document, assets never
// fall back to index.html.
type assetHandler struct {
	dist fs.FS

	// cache maps "path\x00encoding" → *transcodeEntry for transcoded assets.
	cache sync.Map
}

func (s *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if _, err := fs.Stat(s.dist, clean+".br"); err != nil {
		http.NotFound(w, r)
		return
	}

	ct := mime.TypeByExtension(filepath.Ext(clean))
	if ct == "" {
		ct = "application/octet-stream"
	}

	accepted := parseAcceptEncoding(r.Header.Get("Accept-Encoding"))

	// Fast path: serve .br directly.
	if _, ok := accepted["br"]; ok {
		serveBrotli(w, r, s.dist, clean, ct)
		return
	}

	// Pick best accepted encoding, falling back to identity.
	enc := "identity"
	for _, candidate := range []string{"zstd", "gzip"} {
		if _, ok := accepted[candidate]; ok {
			enc = candidate
			break
		}
	}

	data, err := transcode(&s.cache, s.dist, clean, enc)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", ct)
	if enc != "identity" {
		w.Header().Set("Content-Encoding", enc)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Vary", "Accept-Encoding")
	setStaticCacheControl(w, clean)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) //nolint:gosec // data from embedded FS, not user input
}

// rootAssetNames returns the names of the precompressed root-level files in
// dist (favicon, manifest, service worker, icons), with the .br suffix
// stripped. index.html is excluded: it is owned by spaHandler. The caller
// registers each name as an exact route so ServeMux dispatches it to the asset
// handler, leaving "/" purely for the SPA document.
func rootAssetNames(dist fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(dist, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".br")
		if e.IsDir() || !ok || name == "index.html" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// spaHandler serves the single-page-app document. Every route it receives
// resolves to index.html, served decompressed with the auth bootstrap injected
// into <head> so the frontend hydrates the logged-in user without a round-trip
// and never flashes the login page. Because the document varies per user it
// bypasses the precompressed fast path and stays no-cache.
type spaHandler struct {
	dist fs.FS

	// authProviders is the configured OAuth provider list, fixed at startup. It
	// is the only auth-config input to the bootstrap; the user varies per
	// request and comes from the request context.
	authProviders []string

	// cache holds the decompressed index document.
	cache sync.Map
}

func (s *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The response is identity-encoded; compressMiddleware compresses it on the
	// way out.
	serveIndexWithBootstrap(w, &s.cache, s.dist, s.bootstrapSnippet(r))
}

// bootstrapSnippet builds the <script> element that seeds
// window.__CAIC_BOOTSTRAP__ for the current request. The user, if any, is read
// from the request context populated by the auth middleware. json.Marshal
// escapes <, > and & so the payload cannot terminate the script element early.
func (s *spaHandler) bootstrapSnippet(r *http.Request) []byte {
	data := v1.AuthBootstrapResp{AuthProviders: s.authProviders}
	if data.AuthProviders == nil {
		data.AuthProviders = []string{}
	}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		data.User = &v1.UserResp{
			ID:        u.ID,
			Provider:  string(u.Provider),
			Username:  u.Username,
			AvatarURL: u.AvatarURL,
		}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	snippet := make([]byte, 0, len(payload)+40)
	snippet = append(snippet, "<script>window.__CAIC_BOOTSTRAP__="...)
	snippet = append(snippet, payload...)
	snippet = append(snippet, ";</script>"...)
	return snippet
}

// serveBrotli serves a .br file directly from the embedded FS.
func serveBrotli(w http.ResponseWriter, r *http.Request, dist fs.FS, clean, ct string) {
	f, err := dist.Open(clean + ".br")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Encoding", "br")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Vary", "Accept-Encoding")
	setStaticCacheControl(w, clean)
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, clean, stat.ModTime(), rs)
}

// serveIndexWithBootstrap serves index.html decompressed with snippet injected
// into <head>. It omits Content-Encoding so compressMiddleware compresses the
// personalized body, and keeps the no-cache header so it is never shared.
func serveIndexWithBootstrap(w http.ResponseWriter, cache *sync.Map, dist fs.FS, snippet []byte) {
	html, err := transcode(cache, dist, "index.html", "identity")
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	body := injectIntoHead(html, snippet)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	setStaticCacheControl(w, "index.html")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// injectIntoHead returns html with snippet inserted immediately before the
// closing </head> tag, falling back to a prefix when no head is present. The
// returned slice never aliases html, which transcode caches and shares.
func injectIntoHead(html, snippet []byte) []byte {
	if len(snippet) == 0 {
		return html
	}
	idx := max(bytes.Index(html, []byte("</head>")), 0)
	out := make([]byte, 0, len(html)+len(snippet))
	out = append(out, html[:idx]...)
	out = append(out, snippet...)
	out = append(out, html[idx:]...)
	return out
}

// transcode decompresses the .br file and re-compresses to the target
// encoding, caching the result for subsequent requests.
func transcode(cache *sync.Map, dist fs.FS, clean, enc string) ([]byte, error) {
	key := clean + "\x00" + enc
	val, _ := cache.LoadOrStore(key, &transcodeEntry{})
	entry, ok := val.(*transcodeEntry)
	if !ok {
		return nil, fmt.Errorf("transcode cache stored unexpected type %T", val)
	}
	entry.once.Do(func() {
		entry.data, entry.err = doTranscode(dist, clean, enc)
	})
	return entry.data, entry.err
}

// doTranscode performs the actual decompress-then-recompress.
func doTranscode(dist fs.FS, clean, enc string) ([]byte, error) {
	f, err := dist.Open(clean + ".br")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(brotli.NewReader(f))
	if err != nil {
		return nil, err
	}

	if enc == "identity" {
		return raw, nil
	}

	var buf bytes.Buffer
	switch enc {
	case "zstd":
		w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(raw); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case "gzip":
		w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(raw); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// setStaticCacheControl sets Cache-Control for static assets. Hashed
// filenames under assets/ are immutable; everything else (index.html,
// favicon) must not be cached so deploys take effect immediately.
func setStaticCacheControl(w http.ResponseWriter, clean string) {
	if strings.HasPrefix(clean, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
}

// parseAcceptEncoding returns the set of encodings the client accepts.
func parseAcceptEncoding(header string) map[string]struct{} {
	accepted := make(map[string]struct{})
	for part := range strings.SplitSeq(header, ",") {
		enc := strings.TrimSpace(part)
		// Strip quality parameter (e.g. "gzip;q=0.5").
		if i := strings.IndexByte(enc, ';'); i >= 0 {
			enc = enc[:i]
		}
		if enc != "" {
			accepted[enc] = struct{}{}
		}
	}
	return accepted
}
