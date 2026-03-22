// GitHub Releases API: fetch latest release and download assets.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Release is the subset of the GitHub Release API response needed for updates.
type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is a single release asset.
type ReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// LatestRelease fetches the latest non-prerelease for the given owner/repo.
func (c *Client) LatestRelease(ctx context.Context, owner, repo string) (*Release, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase(), owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, body)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// DownloadAsset fetches a release asset URL and returns the response body as a
// stream. The caller must close the returned ReadCloser.
func (c *Client) DownloadAsset(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return resp.Body, nil
}
