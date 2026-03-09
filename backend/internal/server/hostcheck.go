// Host header validation middleware that rejects requests not matching ExternalURL.
package server

import (
	"net"
	"net/http"
	"strings"
)

// hostCheckMiddleware rejects requests whose Host header does not match the
// configured ExternalURL hostname. This prevents host header injection and
// ensures the server only responds to requests addressed to the expected host.
func hostCheckMiddleware(allowed string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Strip port if present (e.g. "example.com:8080" → "example.com").
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !strings.EqualFold(host, allowed) {
			http.Error(w, "forbidden: invalid host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
