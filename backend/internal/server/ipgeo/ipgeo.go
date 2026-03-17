// Package ipgeo provides IP geolocation and country-based allowlist enforcement
// using MaxMind MMDB files.
package ipgeo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// tailscalePrefix is the Tailscale CGNAT range 100.64.0.0/10.
var tailscalePrefix = netip.MustParsePrefix("100.64.0.0/10")

// GetClientIP extracts the real client IP from a request, checking
// X-Forwarded-For and X-Real-IP headers for proxied requests.
func GetClientIP(r *http.Request) string {
	// X-Forwarded-For may contain "client, proxy1, proxy2" — use the leftmost.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, found := strings.Cut(xff, ",")
		if found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	// RemoteAddr: strip port, handle IPv6 [::1]:port form.
	addr := r.RemoteAddr
	if strings.HasPrefix(addr, "[") {
		if host, _, found := strings.Cut(addr, "]:"); found {
			return host[1:]
		}
		return strings.Trim(addr, "[]")
	}
	if host, _, found := strings.Cut(addr, ":"); found {
		return host
	}
	return addr
}

// namedPrefix associates a name with an IP prefix for use in CountryCode.
type namedPrefix struct {
	name   string
	prefix netip.Prefix
}

// Checker resolves IP addresses to country codes or named origins using a
// MaxMind MMDB file and optional named CIDR groups, and enforces an allowlist.
type Checker struct {
	reader     *maxminddb.Reader
	namedCIDRs []namedPrefix
	allowlist  *allowlist
}

// Open opens an MMDB file for country lookups.
func Open(dbPath string) (*Checker, error) {
	r, err := maxminddb.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Checker{reader: r}, nil
}

// NewChecker builds a Checker from a comma-separated allowlist string and an
// optional geo DB path. Named origins that require network fetches are resolved
// automatically (e.g. "github" fetches IP ranges from api.github.com/meta).
// A fetch failure is logged as a warning and does not abort startup.
func NewChecker(ctx context.Context, allowlistStr, dbPath string) (*Checker, error) {
	al, err := parseAllowlist(allowlistStr)
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	if al.needsDB() && dbPath == "" {
		return nil, errors.New("CAIC_IPGEO_DB is required when CAIC_IPGEO_ALLOWLIST contains country codes")
	}
	c := &Checker{allowlist: al}
	if dbPath != "" {
		c, err = Open(dbPath)
		if err != nil {
			return nil, err
		}
		c.allowlist = al
	}
	if al.allowed("github") {
		if prefixes, err := fetchGitHubHookCIDRs(ctx); err != nil {
			slog.Warn("failed to fetch GitHub hook CIDRs; webhook IPs will not be auto-allowed", "err", err)
		} else {
			for _, p := range prefixes {
				c.namedCIDRs = append(c.namedCIDRs, namedPrefix{name: "github", prefix: p.Masked()})
			}
		}
	}
	return c, nil
}

// Close releases MMDB reader resources.
func (c *Checker) Close() error {
	if c == nil || c.reader == nil {
		return nil
	}
	return c.reader.Close()
}

// countryRecord is the minimal MMDB struct for country lookups.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// CountryCode returns the ISO 3166-1 alpha-2 country code or a named origin
// for the given IP string. Resolution order:
//  1. "local" for loopback, private, link-local, and unspecified IPs
//  2. "tailscale" for Tailscale CGNAT IPs (100.64.0.0/10)
//  3. Named CIDR groups (e.g. "github")
//  4. ISO 3166-1 alpha-2 country code from the MMDB geo database
//  5. "" on parse error, lookup error, or no DB with a public IP
func (c *Checker) CountryCode(ipStr string) string {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return ""
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() {
		return "local"
	}
	if tailscalePrefix.Contains(addr) {
		return "tailscale"
	}
	for _, nc := range c.namedCIDRs {
		if nc.prefix.Contains(addr) {
			return nc.name
		}
	}
	if c.reader != nil {
		var rec countryRecord
		if err := c.reader.Lookup(addr).Decode(&rec); err == nil {
			return rec.Country.ISOCode
		}
	}
	return ""
}

// IsAllowed reports whether the given IP is permitted by the checker's
// allowlist. Returns true when no allowlist is configured.
func (c *Checker) IsAllowed(clientIP string) bool {
	return c.allowlist.allowed(c.CountryCode(clientIP)) || c.allowlist.containsIP(clientIP)
}

// allowlist checks whether a country code or IP address is permitted.
// A nil *allowlist allows everything.
type allowlist struct {
	codes    map[string]struct{} // uppercase tokens: country codes and named origins
	prefixes []netip.Prefix      // CIDR entries from the allowlist
}

// parseAllowlist parses a comma-separated list of allowed values. Tokens
// containing "/" are parsed as CIDR prefixes (e.g. "34.74.90.64/28"); all
// others are uppercased and treated as ISO 3166-1 alpha-2 country codes or
// named origins (e.g. "local", "tailscale", "github"). An empty or
// whitespace-only string is an error; use "0.0.0.0/0,::/0" to allow all IPs.
func parseAllowlist(s string) (*allowlist, error) {
	a := &allowlist{codes: make(map[string]struct{})}
	for token := range strings.SplitSeq(s, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if strings.Contains(token, "/") {
			p, err := netip.ParsePrefix(token)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q in allowlist: %w", token, err)
			}
			a.prefixes = append(a.prefixes, p.Masked())
		} else {
			a.codes[strings.ToUpper(token)] = struct{}{}
		}
	}
	if len(a.codes) == 0 && len(a.prefixes) == 0 {
		return nil, errors.New("allowlist must not be empty; use 0.0.0.0/0,::/0 to allow all IPs")
	}
	return a, nil
}

// allowed reports whether the given country code or named origin (as returned
// by CountryCode) is on the allowlist.
func (a *allowlist) allowed(cc string) bool {
	_, ok := a.codes[strings.ToUpper(cc)]
	return ok
}

// containsIP reports whether the given IP string falls within any CIDR prefix
// in the allowlist. Returns false if the IP is invalid.
func (a *allowlist) containsIP(ipStr string) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(a.prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// needsDB reports whether the allowlist contains any country-code entry that
// requires a MaxMind MMDB to resolve. ISO 3166-1 alpha-2 country codes are
// always exactly two uppercase letters; longer tokens ("local", "tailscale",
// "github", etc.) and CIDR prefixes do not require a DB.
func (a *allowlist) needsDB() bool {
	for token := range a.codes {
		if len(token) == 2 && token[0] >= 'A' && token[0] <= 'Z' && token[1] >= 'A' && token[1] <= 'Z' {
			return true
		}
	}
	return false
}
