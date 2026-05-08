// GitHub webhook IP ranges fetched from the GitHub meta API.

package ipgeo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/maruel/roundtrippers"
)

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
