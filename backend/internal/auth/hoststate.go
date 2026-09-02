// External URL state and host-check middleware for OAuth redirect URI resolution.

package auth

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// HostState holds the external URL used to build OAuth redirect URIs.
// In auto mode, the first FQDN request locks the URL. In static mode, the
// URL is set at construction time via NewHostState.
type HostState struct {
	trustedProxies []netip.Prefix

	mu          sync.Mutex
	lockedHost  string // lowercase authority (host or host:port), empty until locked
	externalURL string // e.g. "https://caic.example.com", empty until locked
}

// NewHostState returns host state that accepts forwarding headers only when
// the direct network peer belongs to trustedProxies. A non-empty externalURL
// pre-locks the state; an empty value enables first-FQDN auto-locking.
func NewHostState(externalURL string, trustedProxies []netip.Prefix) *HostState {
	u, _ := url.Parse(externalURL)
	host := ""
	if u != nil {
		host = strings.ToLower(u.Host)
	}
	return &HostState{trustedProxies: slices.Clone(trustedProxies), lockedHost: host, externalURL: externalURL}
}

// ExternalURL returns the external URL from a request for use in OAuth redirect
// URI construction. On the first FQDN request it auto-locks the host authority;
// subsequent calls return the locked URL regardless of the request.
func (s *HostState) ExternalURL(r *http.Request) string {
	authority, scheme, ok := s.effectiveHostAndScheme(r)
	if !ok {
		return ""
	}
	if !isFQDN(extractHost(authority)) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.externalURL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockedHost != "" {
		return s.externalURL
	}
	s.lockedHost = strings.ToLower(authority)
	hostname := extractHost(s.lockedHost)
	hostport := hostname
	if _, port, err := net.SplitHostPort(s.lockedHost); err == nil {
		if (scheme == "https" && port != "443") || (scheme == "http" && port != "80") {
			hostport = net.JoinHostPort(hostname, port)
		}
	}
	s.externalURL = scheme + "://" + hostport
	slog.Info("auto-locked external URL", "url", s.externalURL)
	return s.externalURL
}

// Middleware rejects requests with a different FQDN after the host is locked.
// Non-FQDN hosts (bare IPs, localhost) pass through unchecked.
//
// Forwarding headers are used only when the direct peer is trusted.
func (s *HostState) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, _, ok := s.effectiveHostAndScheme(r)
		if !ok {
			http.Error(w, "forbidden: invalid forwarded origin", http.StatusForbidden)
			return
		}
		if !isFQDN(extractHost(authority)) {
			next.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		locked := s.lockedHost
		s.mu.Unlock()
		if locked != "" && !strings.EqualFold(authority, locked) {
			http.Error(w, "forbidden: invalid host", http.StatusForbidden)
			return
		}
		// Auto-lock via ExternalURL side-effect on first FQDN.
		_ = s.ExternalURL(r)
		next.ServeHTTP(w, r)
	})
}

func (s *HostState) effectiveHostAndScheme(r *http.Request) (authority, scheme string, ok bool) {
	authority = r.Host
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !s.trustsPeer(r.RemoteAddr) {
		return authority, scheme, true
	}
	host, proto, present, valid := forwardedOrigin(r.Header)
	if !present {
		return authority, scheme, true
	}
	if !valid {
		return "", "", false
	}
	return host, proto, true
}

func (s *HostState) trustsPeer(remoteAddr string) bool {
	if len(s.trustedProxies) == 0 {
		return false
	}
	peer, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	addr := peer.Addr()
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func forwardedOrigin(header http.Header) (host, proto string, present, valid bool) {
	forwardedValues := header.Values("Forwarded")
	xHostValues := header.Values("X-Forwarded-Host")
	xProtoValues := header.Values("X-Forwarded-Proto")
	forwardedPresent := len(forwardedValues) > 0
	xForwardedPresent := len(xHostValues) > 0 || len(xProtoValues) > 0
	if !forwardedPresent && !xForwardedPresent {
		return "", "", false, false
	}

	var forwardedHost, forwardedProto string
	if forwardedPresent {
		var ok bool
		forwardedHost, forwardedProto, ok = parseForwarded(forwardedValues)
		if !ok {
			return "", "", true, false
		}
	}
	var xHost, xProto string
	if xForwardedPresent {
		var hostOK, protoOK bool
		xHost, hostOK = finalHeaderElement(xHostValues)
		xProto, protoOK = finalHeaderElement(xProtoValues)
		xProto = strings.ToLower(xProto)
		if !hostOK || !protoOK || !validForwardedOrigin(xHost, xProto) {
			return "", "", true, false
		}
	}
	if forwardedPresent && xForwardedPresent && (!strings.EqualFold(forwardedHost, xHost) || forwardedProto != xProto) {
		return "", "", true, false
	}
	if forwardedPresent {
		return forwardedHost, forwardedProto, true, true
	}
	return xHost, xProto, true, true
}

func parseForwarded(values []string) (host, proto string, ok bool) {
	entry, ok := finalForwardedElement(values)
	if !ok {
		return "", "", false
	}
	hostPresent := false
	protoPresent := false
	for part := range strings.SplitSeq(entry, ";") {
		key, raw, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return "", "", false
		}
		decoded := strings.TrimSpace(raw)
		if strings.HasPrefix(decoded, `"`) {
			var err error
			decoded, err = strconv.Unquote(decoded)
			if err != nil {
				return "", "", false
			}
		}
		switch strings.ToLower(key) {
		case "host":
			if hostPresent {
				return "", "", false
			}
			hostPresent = true
			host = decoded
		case "proto":
			if protoPresent {
				return "", "", false
			}
			protoPresent = true
			proto = strings.ToLower(decoded)
		}
	}
	return host, proto, validForwardedOrigin(host, proto)
}

func finalForwardedElement(values []string) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	value := values[len(values)-1]
	lastComma := -1
	quoted := false
	escaped := false
	for i, char := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if char == ',' && !quoted {
			lastComma = i
		}
	}
	if quoted || escaped {
		return "", false
	}
	value = strings.TrimSpace(value[lastComma+1:])
	return value, value != ""
}

func finalHeaderElement(values []string) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	value := values[len(values)-1]
	if i := strings.LastIndexByte(value, ','); i >= 0 {
		value = value[i+1:]
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func validForwardedOrigin(host, proto string) bool {
	if host == "" || (proto != "http" && proto != "https") || strings.ContainsAny(host, "\r\n/\\") {
		return false
	}
	u, err := url.Parse(proto + "://" + host)
	return err == nil && u.Host == host && u.User == nil
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
