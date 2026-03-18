// Host header validation middleware that rejects requests not matching ExternalURL.
package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
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

// autoHostState holds the auto-lock state shared between the middleware and
// OAuth handlers. The first FQDN request locks the external base URL.
type autoHostState struct {
	mu          sync.Mutex
	lockedHost  string // lowercase hostname, empty until locked
	externalURL string // e.g. "https://caic.example.com", empty until locked
}

// ExternalURL returns the locked external URL, or "" if not yet locked.
func (s *autoHostState) ExternalURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.externalURL
}

// lock attempts to lock the host from the HTTP request. Returns the locked
// hostname (lowercase). If already locked, returns the existing value.
func (s *autoHostState) lock(r *http.Request) string {
	host := extractHost(r.Host)
	if !isFQDN(host) {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockedHost != "" {
		return s.lockedHost
	}
	s.lockedHost = strings.ToLower(host)
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	// Preserve non-default port in the external URL.
	hostport := s.lockedHost
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		if (scheme == "https" && port != "443") || (scheme == "http" && port != "80") {
			hostport = net.JoinHostPort(s.lockedHost, port)
		}
	}
	s.externalURL = scheme + "://" + hostport
	slog.Info("auto-locked external URL", "url", s.externalURL)
	return s.lockedHost
}

// autoHostCheckMiddleware uses an autoHostState to lock on the first FQDN
// request and reject different FQDNs afterward. Non-FQDN hosts (bare IPs,
// localhost) pass through unchecked.
func autoHostCheckMiddleware(state *autoHostState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := extractHost(r.Host)
		if !isFQDN(host) {
			next.ServeHTTP(w, r)
			return
		}
		locked := state.lock(r)
		if !strings.EqualFold(host, locked) {
			http.Error(w, "forbidden: invalid host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractHost strips the port from a Host header value.
func extractHost(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// isFQDN reports whether host looks like a fully qualified domain name:
// contains at least one dot and is not a numeric IP address.
func isFQDN(host string) bool {
	return strings.Contains(host, ".") && net.ParseIP(host) == nil
}
