// Request body decompression based on Content-Encoding. Supports zstd, brotli, and gzip.

package server

import (
	"io"
	"net/http"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/server/api"
)

// decompressMiddleware returns a handler that decompresses request bodies
// based on the Content-Encoding header.
func decompressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ce := r.Header.Get("Content-Encoding")
		if ce == "" {
			next.ServeHTTP(w, r)
			return
		}

		var reader io.ReadCloser
		switch ce {
		case "zstd":
			dec, err := zstd.NewReader(r.Body, zstd.WithDecoderMaxMemory(10<<20))
			if err != nil {
				writeError(w, api.BadRequest("invalid zstd body"))
				return
			}
			reader = dec.IOReadCloser()
		case "br":
			reader = io.NopCloser(brotli.NewReader(r.Body))
		case "gzip":
			gr, err := gzip.NewReader(r.Body)
			if err != nil {
				writeError(w, api.BadRequest("invalid gzip body"))
				return
			}
			reader = gr
		default:
			writeError(w, api.BadRequest("unsupported Content-Encoding: "+ce))
			return
		}

		r.Body = reader
		r.Header.Del("Content-Encoding")
		r.ContentLength = -1
		next.ServeHTTP(w, r)
	})
}
