// Client IP extraction for HTTP origin checks and request logging.

package server

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
)

// clientIP returns the client address for r. It accepts client-IP forwarding
// headers only when r's direct peer is a configured trusted proxy. For an
// X-Forwarded-For chain, it returns the rightmost untrusted address; malformed
// forwarding values and all-trusted chains use the direct peer instead.
func clientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	direct := directClientIP(r.RemoteAddr)
	if !auth.TrustsPeer(r.RemoteAddr, trustedProxies) {
		return direct
	}

	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		return clientIPFromXForwardedFor(xff, trustedProxies, direct)
	}
	if xri := r.Header.Values("X-Real-IP"); len(xri) > 0 {
		return clientIPFromRealIP(xri, direct)
	}
	return direct
}

func clientIPFromXForwardedFor(values []string, trustedProxies []netip.Prefix, direct string) string {
	var chain []netip.Addr
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			addr, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return direct
			}
			chain = append(chain, addr)
		}
	}
	for _, c := range slices.Backward(chain) {
		if !auth.TrustsAddress(c, trustedProxies) {
			return c.String()
		}
	}
	return direct
}

func clientIPFromRealIP(values []string, direct string) string {
	if len(values) != 1 {
		return direct
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil {
		return direct
	}
	return addr.String()
}

func directClientIP(remoteAddr string) string {
	if addr, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addr.Addr().String()
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.String()
	}
	return ""
}
