// Named IP origin sources for allowlist matching.

package ipgeo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/maruel/roundtrippers"
)

// OriginSource provides CIDR prefixes for a named IP origin.
type OriginSource interface {
	Name() string
	Prefixes(ctx context.Context) ([]netip.Prefix, error)
}

type fetchPrefixesFromURL func(ctx context.Context, url string) ([]netip.Prefix, error)

type staticOriginSource struct {
	name     string
	prefixes []netip.Prefix
}

func (s *staticOriginSource) Name() string { return s.name }

func (s *staticOriginSource) Prefixes(context.Context) ([]netip.Prefix, error) {
	return s.prefixes, nil
}

type urlOriginSource struct {
	name  string
	urls  []string
	fetch fetchPrefixesFromURL
}

func (s *urlOriginSource) Name() string { return s.name }

func (s *urlOriginSource) Prefixes(ctx context.Context) ([]netip.Prefix, error) {
	var errs []error
	var prefixes []netip.Prefix
	for _, url := range s.urls {
		fetched, err := s.fetch(ctx, url)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		prefixes = append(prefixes, fetched...)
	}
	if len(errs) > 0 {
		return prefixes, errors.Join(errs...)
	}
	return prefixes, nil
}

func defaultOriginSources(githubURL string) []OriginSource {
	return []OriginSource{
		&staticOriginSource{name: "anthropic", prefixes: []netip.Prefix{anthropicOutboundPrefix}},
		&urlOriginSource{name: "github", urls: []string{githubURL}, fetch: fetchGitHubHookCIDRsFrom},
		&urlOriginSource{name: "openai", urls: openAIIPRangeURLs, fetch: fetchOpenAICIDRsFrom},
	}
}

// Anthropic

// anthropicOutboundPrefix is listed at https://platform.claude.com/docs/en/api/ip-addresses
var anthropicOutboundPrefix = netip.MustParsePrefix("160.79.104.0/21")

// GitHub

// githubMetaResponse is the minimal shape of https://api.github.com/meta.
type githubMetaResponse struct {
	Hooks []string `json:"hooks"`
}

// retryPolicy403 extends the default retry policy to also retry on 403.
type retryPolicy403 struct {
	*roundtrippers.ExponentialBackoff
}

func (p *retryPolicy403) ShouldRetry(ctx context.Context, start time.Time, try int, err error, resp *http.Response) bool {
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		return p.ExponentialBackoff.ShouldRetry(ctx, start, try, err, nil)
	}
	return p.ExponentialBackoff.ShouldRetry(ctx, start, try, err, resp)
}

// defaultGitHubMetaURL is the canonical GitHub meta API endpoint.
const defaultGitHubMetaURL = "https://api.github.com/meta"

// fetchGitHubHookCIDRsFrom fetches GitHub webhook IP ranges from a URL.
//
// It is described at https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/about-githubs-ip-addresses
//
// The URL parameter allows tests to substitute a local server.
func fetchGitHubHookCIDRsFrom(ctx context.Context, url string) ([]netip.Prefix, error) {
	client := &http.Client{
		Transport: &roundtrippers.Retry{
			Transport: http.DefaultTransport,
			Policy: &retryPolicy403{
				ExponentialBackoff: &roundtrippers.DefaultRetryPolicy,
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub meta API returned %d", resp.StatusCode)
	}
	var meta githubMetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode GitHub meta: %w", err)
	}
	prefixes := make([]netip.Prefix, 0, len(meta.Hooks))
	for _, cidr := range meta.Hooks {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in GitHub meta: %w", cidr, err)
		}
		prefixes = append(prefixes, p.Masked())
	}
	return prefixes, nil
}

// OpenAI

var openAIIPRangeURLs = []string{
	"https://openai.com/chatgpt-connectors.json",
	"https://openai.com/chatgpt-agents.json",
}

type openAIIPRangeResponse struct {
	Prefixes []openAIIPRangePrefix `json:"prefixes"`
}

type openAIIPRangePrefix struct {
	IPv4Prefix string `json:"ipv4Prefix"`
	IPv6Prefix string `json:"ipv6Prefix"`
}

// fetchOpenAICIDRsFrom fetches OpenAI published IP ranges from a URL.
//
// It is described at https://developers.openai.com/api/docs/guides/ip-addresses
//
// The URL parameter allows tests to substitute a local server.
func fetchOpenAICIDRsFrom(ctx context.Context, url string) ([]netip.Prefix, error) {
	client := &http.Client{
		Transport: &roundtrippers.Retry{
			Transport: http.DefaultTransport,
			Policy:    &retryPolicy403{ExponentialBackoff: &roundtrippers.DefaultRetryPolicy},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI IP range URL %q returned %d", url, resp.StatusCode)
	}
	var ranges openAIIPRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&ranges); err != nil {
		return nil, fmt.Errorf("decode OpenAI IP ranges %q: %w", url, err)
	}
	prefixes := make([]netip.Prefix, 0, len(ranges.Prefixes))
	for _, prefix := range ranges.Prefixes {
		for _, cidr := range []string{prefix.IPv4Prefix, prefix.IPv6Prefix} {
			if cidr == "" {
				continue
			}
			p, err := netip.ParsePrefix(cidr)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q in OpenAI IP ranges %q: %w", cidr, url, err)
			}
			prefixes = append(prefixes, p.Masked())
		}
	}
	return prefixes, nil
}

// Tailscale

// tailscalePrefix is the Tailscale CGNAT range 100.64.0.0/10.
//
// TODO: Move to staticOriginSource.
var tailscalePrefix = netip.MustParsePrefix("100.64.0.0/10")
