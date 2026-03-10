// Package github implements forge.Forge for github.com using the GitHub REST API.
// Uses net/http directly; no extra dependencies.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Client is a minimal GitHub API client authenticated with a personal access token.
// It implements forge.Forge.
type Client struct {
	Token string
}

var _ forge.Forge = (*Client)(nil)

// createPRRequest is the JSON body for POST /repos/{owner}/{repo}/pulls.
type createPRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// createPRResponse is the relevant subset of the GitHub PR creation response.
type createPRResponse struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// refResponse is the relevant subset of the GitHub git ref response.
type refResponse struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// checkRunsResponse is the relevant subset of the GitHub check-runs list response.
type checkRunsResponse struct {
	CheckRuns []struct {
		ID         int64                    `json:"id"`
		Name       string                   `json:"name"`
		Status     forge.CheckRunStatus     `json:"status"`
		Conclusion forge.CheckRunConclusion `json:"conclusion"`
		HTMLURL    string                   `json:"html_url"` // e.g. https://github.com/owner/repo/actions/runs/{runID}/job/{jobID}
	} `json:"check_runs"`
}

// actionsRunRe extracts the workflow run ID from a GitHub Actions job URL.
var actionsRunRe = regexp.MustCompile(`/actions/runs/(\d+)/job/\d+`)

// CreatePR creates a pull request on GitHub and returns its metadata.
func (c *Client) CreatePR(ctx context.Context, owner, repo, head, base, title, body string) (forge.PR, error) {
	payload, err := json.Marshal(createPRRequest{Title: title, Body: body, Head: head, Base: base})
	if err != nil {
		return forge.PR{}, err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return forge.PR{}, err
	}
	c.setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return forge.PR{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return forge.PR{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		return forge.PR{}, fmt.Errorf("github create PR: status %d: %s", resp.StatusCode, data)
	}
	var r createPRResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return forge.PR{}, err
	}
	return forge.PR{Number: r.Number, HeadSHA: r.Head.SHA}, nil
}

// GetDefaultBranchSHA returns the HEAD commit SHA of branch in the given repo.
// Uses the lightweight git refs API — no full commit data is fetched.
func (c *Client) GetDefaultBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", owner, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	c.setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("github get ref: %w", forge.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github get ref: status %d: %s", resp.StatusCode, data)
	}
	var r refResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", err
	}
	return r.Object.SHA, nil
}

// GetCheckRuns returns all check runs for the given commit SHA.
func (c *Client) GetCheckRuns(ctx context.Context, owner, repo, sha string) ([]forge.CheckRun, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/check-runs", owner, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("github get check-runs: %w", forge.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github get check-runs: status %d: %s", resp.StatusCode, data)
	}
	var r checkRunsResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	runs := make([]forge.CheckRun, len(r.CheckRuns))
	for i, cr := range r.CheckRuns {
		var runID int64
		if m := actionsRunRe.FindStringSubmatch(cr.HTMLURL); m != nil {
			runID, _ = strconv.ParseInt(m[1], 10, 64)
		}
		runs[i] = forge.CheckRun{
			JobID:      cr.ID,
			RunID:      runID,
			Name:       cr.Name,
			Status:     cr.Status,
			Conclusion: cr.Conclusion,
		}
	}
	return runs, nil
}

// GetJobLog fetches the log for a GitHub Actions job and returns the last
// maxBytes bytes of its content. maxBytes <= 0 means no limit.
func (c *Client) GetJobLog(ctx context.Context, owner, repo string, jobID int64, maxBytes int) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	c.setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github get job log: status %d: %s", resp.StatusCode, data)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		// Trim to next line boundary to avoid splitting a log line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return string(data), nil
}

// PRURL returns the GitHub pull request URL.
func (c *Client) PRURL(owner, repo string, prNumber int) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, prNumber)
}

// PRLabel returns a GitHub-style PR label.
func (c *Client) PRLabel(prNumber int) string {
	return fmt.Sprintf("PR #%d", prNumber)
}

// CIJobURL returns the GitHub Actions job URL.
func (c *Client) CIJobURL(owner, repo string, runID, jobID int64) string {
	if runID > 0 && jobID > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/job/%d", owner, repo, runID, jobID)
	}
	return ""
}

// CIHomeURL returns the GitHub Actions overview URL for a repo.
func (c *Client) CIHomeURL(remoteURL string) string {
	return remoteURL + "/actions"
}

// BranchCompareURL returns the GitHub compare URL for a branch.
func (c *Client) BranchCompareURL(remoteURL, branch string) string {
	return remoteURL + "/compare/" + branch + "?expand=1"
}

// PostComment posts a comment on the given issue or pull request.
func (c *Client) PostComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github post comment: status %d: %s", resp.StatusCode, data)
	}
	return nil
}

// Name returns "GitHub".
func (c *Client) Name() string { return "GitHub" }

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
}
