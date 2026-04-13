// GitHub webhook IP ranges fetched from the GitHub meta API.
package ipgeo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"time"

	"github.com/maruel/roundtrippers"
)

// githubMetaResponse is the minimal shape of https://api.github.com/meta.
type githubMetaResponse struct {
	Hooks []string `json:"hooks"`
}

// retryPolicy403 retries on 403, 429, and 5xx errors with exponential backoff.
type retryPolicy403 struct{}

func (p *retryPolicy403) ShouldRetry(ctx context.Context, start time.Time, try int, err error, resp *http.Response) bool {
	if err != nil {
		return try < 3
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

func (p *retryPolicy403) Backoff(start time.Time, try int) time.Duration {
	return time.Duration(math.Pow(2, float64(try))) * time.Second
}

// githubMetaURL is the URL of the GitHub meta API. It is a variable so tests
// can override it without making network calls.
var githubMetaURL = "https://api.github.com/meta"

// fetchGitHubHookCIDRs fetches the GitHub meta API and returns the IP prefixes
// used for webhook delivery.
func fetchGitHubHookCIDRs(ctx context.Context) ([]netip.Prefix, error) {
	return fetchGitHubHookCIDRsFrom(ctx, githubMetaURL)
}

func fetchGitHubHookCIDRsFrom(ctx context.Context, url string) ([]netip.Prefix, error) {
	client := &http.Client{
		Transport: &roundtrippers.Retry{
			Transport: http.DefaultTransport,
			Policy:    &retryPolicy403{},
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
