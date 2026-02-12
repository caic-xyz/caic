// Precompressed static file handler for embedded frontend assets.
//
// At build time, each file in dist/ is brotli-compressed at maximum quality
// and the original is deleted, so only .br files are embedded. This handler
// serves .br directly when the client accepts it, and lazily transcodes to
// gzip, zstd, or uncompressed for other clients, caching the result.
package server

import (
	"bytes"
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
)

// transcodeEntry holds a lazily-computed transcoded variant.
type transcodeEntry struct {
	once sync.Once
	data []byte
	err  error
}

// newStaticHandler returns an http.HandlerFunc that serves precompressed
// static files from dist with SPA fallback to index.html.
//
// Only .br files exist on disk. The handler serves brotli directly when
// accepted, and lazily transcodes to zstd/gzip/identity otherwise.
func newStaticHandler(dist fs.FS) http.HandlerFunc {
	// cache maps "path\x00encoding" → *transcodeEntry.
	var cache sync.Map

	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" {
			p = "/index.html"
		}
		clean := strings.TrimPrefix(path.Clean(p), "/")

		// SPA fallback: if the .br file doesn't exist, serve index.html.
		if _, err := fs.Stat(dist, clean+".br"); err != nil {
			clean = "index.html"
		}

		ct := mime.TypeByExtension(filepath.Ext(clean))
		if ct == "" {
			ct = "application/octet-stream"
		}

		accepted := parseAcceptEncoding(r.Header.Get("Accept-Encoding"))

		// Fast path: serve .br directly.
		if accepted["br"] {
			serveBrotli(w, r, dist, clean, ct)
			return
		}

		// Pick best accepted encoding, falling back to identity.
		enc := "identity"
		for _, candidate := range []string{"zstd", "gzip"} {
			if accepted[candidate] {
				enc = candidate
				break
			}
		}

		data, err := transcode(&cache, dist, clean, enc)
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
		_, _ = w.Write(data)
	}
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
	http.ServeContent(w, r, clean, stat.ModTime(), f.(io.ReadSeeker))
}

// transcode decompresses the .br file and re-compresses to the target
// encoding, caching the result for subsequent requests.
func transcode(cache *sync.Map, dist fs.FS, clean, enc string) ([]byte, error) {
	key := clean + "\x00" + enc
	val, _ := cache.LoadOrStore(key, &transcodeEntry{})
	entry := val.(*transcodeEntry)
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
func parseAcceptEncoding(header string) map[string]bool {
	accepted := make(map[string]bool)
	for part := range strings.SplitSeq(header, ",") {
		enc := strings.TrimSpace(part)
		// Strip quality parameter (e.g. "gzip;q=0.5").
		if i := strings.IndexByte(enc, ';'); i >= 0 {
			enc = enc[:i]
		}
		if enc != "" {
			accepted[enc] = true
		}
	}
	return accepted
}
