// GitHub webhook IP ranges fetched from the GitHub meta API.
package ipgeo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

// githubMetaResponse is the minimal shape of https://api.github.com/meta.
type githubMetaResponse struct {
	Hooks []string `json:"hooks"`
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
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
