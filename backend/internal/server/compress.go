// Response compression middleware for API endpoints.
//
// Compresses responses using zstd, brotli, or gzip at fast compression
// levels. SSE streams are compressed with per-event flushing to preserve
// real-time delivery. Skips responses that already have a Content-Encoding
// (precompressed static files).
package server

import (
	"io"
	"net/http"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// compressMiddleware returns a handler that compresses responses based on
// the client's Accept-Encoding header.
func compressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted := parseAcceptEncoding(r.Header.Get("Accept-Encoding"))
		enc := negotiateEncoding(accepted)
		if enc == "" {
			next.ServeHTTP(w, r)
			return
		}

		cw := &compressWriter{
			ResponseWriter: w,
			encoding:       enc,
		}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

// negotiateEncoding picks the best encoding the client accepts.
func negotiateEncoding(accepted map[string]struct{}) string {
	for _, enc := range []string{"zstd", "br", "gzip"} {
		if _, ok := accepted[enc]; ok {
			return enc
		}
	}
	return ""
}

// compressWriter wraps http.ResponseWriter to compress the response body.
type compressWriter struct {
	http.ResponseWriter

	encoding     string
	writer       io.WriteCloser
	headerSent   bool
	skipCompress bool
}

func (cw *compressWriter) WriteHeader(code int) {
	// WebSocket upgrades hijack the connection; compressing the 101
	// response adds a bogus Content-Encoding header and causes a
	// "response.Write on hijacked connection" log on cleanup.
	if code == http.StatusSwitchingProtocols {
		cw.skipCompress = true
		cw.headerSent = true
	}
	cw.initOnce()
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *compressWriter) Write(b []byte) (int, error) {
	cw.initOnce()
	if cw.skipCompress {
		return cw.ResponseWriter.Write(b)
	}
	return cw.writer.Write(b)
}

// Flush flushes compressed data to the wire. Calls initOnce so that
// Content-Encoding is set before the first flush sends headers.
func (cw *compressWriter) Flush() {
	cw.initOnce()
	if cw.writer != nil {
		if f, ok := cw.writer.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController.
func (cw *compressWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// initOnce inspects response headers to decide whether to compress.
// Called once before the first Write, WriteHeader, or Flush.
func (cw *compressWriter) initOnce() {
	if cw.headerSent {
		return
	}
	cw.headerSent = true

	h := cw.Header()

	// Skip if the handler already set Content-Encoding (precompressed static).
	if h.Get("Content-Encoding") != "" {
		cw.skipCompress = true
		return
	}

	// Compressed size differs from original; remove Content-Length.
	h.Del("Content-Length")
	h.Set("Content-Encoding", cw.encoding)
	h.Add("Vary", "Accept-Encoding")

	switch cw.encoding {
	case "zstd":
		enc, _ := zstd.NewWriter(cw.ResponseWriter, zstd.WithEncoderLevel(zstd.SpeedFastest))
		cw.writer = enc
	case "br":
		cw.writer = brotli.NewWriterLevel(cw.ResponseWriter, 1)
	case "gzip":
		gz, _ := gzip.NewWriterLevel(cw.ResponseWriter, gzip.BestSpeed)
		cw.writer = gz
	}
}

// finish flushes and closes the compressor.
func (cw *compressWriter) finish() {
	if cw.writer == nil {
		return
	}
	_ = cw.writer.Close()
}
