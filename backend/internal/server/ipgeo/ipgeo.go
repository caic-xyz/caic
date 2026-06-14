// Package ipgeo provides IP origin and country allowlist enforcement.
package ipgeo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

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

type originResolver interface {
	Resolve(addr netip.Addr) string
}

// namedPrefix resolves addresses contained by a CIDR prefix to a named origin.
type namedPrefix struct {
	name   string
	prefix netip.Prefix
}

func (p namedPrefix) Resolve(addr netip.Addr) string {
	if p.prefix.Contains(addr) {
		return p.name
	}
	return ""
}

// Checker resolves IP addresses to country codes or named origins using a
// MaxMind MMDB file and optional named CIDR groups, and enforces an allowlist.
type Checker struct {
	// resolvers are first-match; named origins must precede country lookups.
	resolvers []originResolver
	allowlist *allowlist
}

// Open opens an MMDB file for country lookups.
func Open(dbPath string) (*Checker, error) {
	r, err := maxminddb.Open(dbPath)
	if err != nil {
		return nil, err
	}
	c := &Checker{}
	for _, source := range defaultOriginSources(defaultGitHubMetaURL) {
		if source.Cacheable() {
			continue
		}
		prefixes, err := source.Prefixes(context.Background())
		if err != nil {
			return nil, fmt.Errorf("origin %s: %w", source.Name(), err)
		}
		for _, p := range prefixes {
			c.resolvers = append(c.resolvers, namedPrefix{name: source.Name(), prefix: p.Masked()})
		}
	}
	c.resolvers = append(c.resolvers, maxMindResolver{reader: r})
	return c, nil
}

// NewChecker builds a Checker from a comma-separated allowlist string and an
// optional geo DB path. Named origins that require network fetches are resolved
// automatically. Results are cached under cacheDir for 24 hours; stale cache
// entries are used when refresh fails. Fetch failures are logged as warnings and
// do not abort startup.
func NewChecker(ctx context.Context, allowlistStr, dbPath, cacheDir string) (*Checker, error) {
	al, err := parseAllowlist(allowlistStr)
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	if al.needsDB() && dbPath == "" {
		return nil, errors.New("CAIC_IPGEO_DB is required when CAIC_IPGEO_ALLOWLIST contains country codes")
	}
	c := &Checker{allowlist: al}
	var countryReader *maxminddb.Reader
	if dbPath != "" {
		countryReader, err = maxminddb.Open(dbPath)
		if err != nil {
			return nil, err
		}
	}
	cache := loadOriginCache(ctx, cacheDir)
	for _, source := range defaultOriginSources(defaultGitHubMetaURL) {
		if source.Cacheable() && !al.allowed(source.Name()) {
			continue
		}
		prefixes := resolveOriginPrefixes(ctx, cache, source)
		for _, p := range prefixes {
			c.resolvers = append(c.resolvers, namedPrefix{name: source.Name(), prefix: p.Masked()})
		}
	}
	if countryReader != nil {
		c.resolvers = append(c.resolvers, maxMindResolver{reader: countryReader})
	}
	return c, nil
}

// Close releases MMDB reader resources.
func (c *Checker) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, r := range c.resolvers {
		closer, ok := r.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// countryRecord is the minimal MMDB struct for country lookups.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

type maxMindResolver struct {
	reader *maxminddb.Reader
}

func (r maxMindResolver) Resolve(addr netip.Addr) string {
	var rec countryRecord
	if err := r.reader.Lookup(addr).Decode(&rec); err == nil {
		return rec.Country.ISOCode
	}
	return ""
}

func (r maxMindResolver) Close() error {
	return r.reader.Close()
}

// CheckOrigin resolves the IP origin and reports whether it is allowlisted.
//
// It returns the resolved origin even when the allowlist blocks the IP, so
// callers can use the same lookup result for logging and error messages. It
// returns "", false for invalid IPs unless no allowlist is configured.
func (c *Checker) CheckOrigin(clientIP string) (string, bool) {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return "", c.allowlist == nil
	}
	origin := c.resolve(addr)
	if c.allowlist == nil {
		return origin, true
	}
	return origin, c.allowlist.allowed(origin) || c.allowlist.contains(addr)
}

func (c *Checker) resolve(addr netip.Addr) string {
	for _, r := range c.resolvers {
		if name := r.Resolve(addr); name != "" {
			return name
		}
	}
	return ""
}

// allowlist checks whether a country code or IP address is permitted.
type allowlist struct {
	codes    map[string]struct{} // uppercase tokens: country codes and named origins
	prefixes []netip.Prefix      // CIDR entries from the allowlist
}

// parseAllowlist parses a comma-separated list of allowed values. Tokens
// containing "/" are parsed as CIDR prefixes (e.g. "34.74.90.64/28"); all
// others are uppercased and treated as ISO 3166-1 alpha-2 country codes or
// named origins (e.g. "local", "tailscale", "anthropic", "github", "openai"). An empty or
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

// allowed reports whether the given country code or named origin is on the
// allowlist.
func (a *allowlist) allowed(cc string) bool {
	_, ok := a.codes[strings.ToUpper(cc)]
	return ok
}

// contains reports whether the given IP falls within any CIDR prefix in the
// allowlist.
func (a *allowlist) contains(addr netip.Addr) bool {
	return slices.ContainsFunc(a.prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// needsDB reports whether the allowlist contains any country-code entry that
// requires a MaxMind MMDB to resolve. ISO 3166-1 alpha-2 country codes are
// always exactly two uppercase letters; longer tokens ("local", "tailscale",
// "anthropic", "github", "openai", etc.) and CIDR prefixes do not require a DB.
func (a *allowlist) needsDB() bool {
	for token := range a.codes {
		if len(token) == 2 && token[0] >= 'A' && token[0] <= 'Z' && token[1] >= 'A' && token[1] <= 'Z' {
			return true
		}
	}
	return false
}

func loadOriginCache(ctx context.Context, cacheDir string) *originCache {
	if cacheDir == "" {
		return nil
	}
	cache, err := openOriginCache(filepath.Join(cacheDir, "ip-origins.json"))
	if err != nil {
		slog.WarnContext(ctx, "failed to load IP origin cache", "err", err)
		return nil
	}
	return cache
}

func resolveOriginPrefixes(ctx context.Context, cache *originCache, source OriginSource) []netip.Prefix {
	if !source.Cacheable() {
		prefixes, err := source.Prefixes(ctx)
		if err != nil {
			slog.WarnContext(ctx, "failed to resolve static IP origin", "origin", source.Name(), "err", err)
			return nil
		}
		return prefixes
	}
	if cache != nil {
		if prefixes, ok := cache.fresh(source.Name()); ok {
			return prefixes
		}
	}
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 10*time.Second)
	prefixes, err := source.Prefixes(fetchCtx)
	fetchCancel()
	if err == nil {
		if cache != nil {
			if err := cache.set(source.Name(), prefixes); err != nil {
				slog.WarnContext(ctx, "failed to update IP origin cache", "origin", source.Name(), "err", err)
			}
		}
		return prefixes
	}
	if cache != nil {
		if prefixes, ok := cache.stale(source.Name()); ok {
			slog.WarnContext(ctx, "failed to refresh IP origin; using stale cache", "origin", source.Name(), "err", err)
			return prefixes
		}
	}
	if len(prefixes) > 0 {
		slog.WarnContext(ctx, "partially fetched IP origin; using uncached partial prefixes", "origin", source.Name(), "err", err)
		return prefixes
	}
	slog.WarnContext(ctx, "failed to fetch IP origin; origin IPs will not be auto-allowed", "origin", source.Name(), "err", err)
	return nil
}
