// External URL state and host-check middleware for OAuth redirect URI resolution.

package auth

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// HostState holds the external URL used to build OAuth redirect URIs.
// In auto mode, the first FQDN request locks the URL. In static mode, the
// URL is set at construction time via NewHostState.
type HostState struct {
	mu          sync.Mutex
	lockedHost  string // lowercase authority (host or host:port), empty until locked
	externalURL string // e.g. "https://caic.example.com", empty until locked
}

// NewHostState returns a pre-locked HostState for a known external URL.
func NewHostState(externalURL string) *HostState {
	u, _ := url.Parse(externalURL)
	host := ""
	if u != nil {
		host = strings.ToLower(u.Host)
	}
	return &HostState{lockedHost: host, externalURL: externalURL}
}

// ExternalURL returns the locked external URL, or "" if not yet locked.
func (s *HostState) ExternalURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.externalURL
}

// lock locks the host authority from the request. The caller must ensure the
// host is a valid FQDN. If already locked, returns the existing value.
func (s *HostState) lock(authority, scheme string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockedHost != "" {
		return s.lockedHost
	}
	s.lockedHost = strings.ToLower(authority)
	hostname := extractHost(s.lockedHost)
	// Preserve non-default port in the external URL.
	hostport := hostname
	if _, port, err := net.SplitHostPort(s.lockedHost); err == nil {
		if (scheme == "https" && port != "443") || (scheme == "http" && port != "80") {
			hostport = net.JoinHostPort(hostname, port)
		}
	}
	s.externalURL = scheme + "://" + hostport
	slog.Info("auto-locked external URL", "url", s.externalURL)
	return s.lockedHost
}

// Middleware locks on the first FQDN request and rejects different FQDNs
// afterward. Non-FQDN hosts (bare IPs, localhost) pass through unchecked.
//
// Behind a reverse proxy, X-Forwarded-Host and X-Forwarded-Proto are used
// to determine the client-facing authority and scheme.
func (s *HostState) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, scheme := effectiveHostAndScheme(r)
		if !isFQDN(extractHost(authority)) {
			next.ServeHTTP(w, r)
			return
		}
		if locked := s.lock(authority, scheme); !strings.EqualFold(authority, locked) {
			http.Error(w, "forbidden: invalid host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// effectiveHostAndScheme returns the client-facing authority and scheme.
// It prefers X-Forwarded-Host and X-Forwarded-Proto over r.Host and r.TLS.
func effectiveHostAndScheme(r *http.Request) (authority, scheme string) {
	authority = r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		authority = fh
	}
	scheme = "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return authority, scheme
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
