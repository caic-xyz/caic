// Package github implements forge.Forge for github.com using the GitHub REST API.
// Uses net/http directly; no extra dependencies.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maruel/roundtrippers"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Client is a minimal GitHub API client authenticated with a personal access token.
// It implements forge.Forge.
type Client struct {
	HTTPClient *http.Client
	// baseURL overrides the GitHub API base URL. Empty means https://api.github.com.
	// Used by tests to point at a fake server.
	baseURL string
}

var _ forge.Forge = (*Client)(nil)

// apiBase returns the GitHub API base URL, using the override if set.
func (c *Client) apiBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return "https://api.github.com"
}

// NewClient returns a Client that authenticates with token and throttles/retries
// via throttle. The transport chain is: Header → Retry → throttle.
func NewClient(token string, throttle http.RoundTripper) *Client {
	return &Client{
		HTTPClient: &http.Client{
			Transport: &roundtrippers.Header{
				Transport: &roundtrippers.Retry{Transport: throttle},
				Header: http.Header{
					"Authorization":        {"Bearer " + token},
					"Accept":               {"application/vnd.github+json"},
					"X-GitHub-Api-Version": {"2026-03-10"},
					"Content-Type":         {"application/json"},
				},
			},
		},
	}
}

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
		ID          int64                    `json:"id"`
		Name        string                   `json:"name"`
		Status      forge.CheckRunStatus     `json:"status"`
		Conclusion  forge.CheckRunConclusion `json:"conclusion"`
		HTMLURL     string                   `json:"html_url"` // e.g. https://github.com/owner/repo/actions/runs/{runID}/job/{jobID}
		CreatedAt   *time.Time               `json:"created_at"`
		StartedAt   *time.Time               `json:"started_at"`
		CompletedAt *time.Time               `json:"completed_at"`
	} `json:"check_runs"`
}

// jobResponse is the relevant subset of the GitHub Actions job detail response.
type jobResponse struct {
	Labels []string `json:"labels"`
}

// searchPRsResponse is the relevant subset of the GitHub search PRs response.
type searchPRsResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"items"`
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
	resp, err := c.HTTPClient.Do(req)
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

// FindPRByBranch returns the PR for the given head branch, or ErrNotFound
// if no PR exists for that branch.
func (c *Client) FindPRByBranch(ctx context.Context, owner, repo, headBranch string) (forge.PR, error) {
	url := fmt.Sprintf("https://api.github.com/search/issues?q=repo:%s/%s+head:%s+is:pr", owner, repo, headBranch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return forge.PR{}, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return forge.PR{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return forge.PR{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return forge.PR{}, fmt.Errorf("github search PRs: status %d: %s", resp.StatusCode, data)
	}
	var r searchPRsResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return forge.PR{}, err
	}
	if len(r.Items) == 0 {
		return forge.PR{}, fmt.Errorf("no PR found for branch %q: %w", headBranch, forge.ErrNotFound)
	}
	// Return the first matching PR (most recent).
	pr := r.Items[0]
	return forge.PR{Number: pr.Number, HeadSHA: pr.Head.SHA}, nil
}

// GetDefaultBranchSHA returns the HEAD commit SHA of branch in the given repo.
// Uses the lightweight git refs API — no full commit data is fetched.
func (c *Client) GetDefaultBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", owner, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
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
	resp, err := c.HTTPClient.Do(req)
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
		var queuedAt, startedAt, completedAt time.Time
		if cr.CreatedAt != nil {
			queuedAt = *cr.CreatedAt
		}
		if cr.StartedAt != nil {
			startedAt = *cr.StartedAt
		}
		if cr.CompletedAt != nil {
			completedAt = *cr.CompletedAt
		}
		runs[i] = forge.CheckRun{
			JobID:       cr.ID,
			RunID:       runID,
			Name:        cr.Name,
			Status:      cr.Status,
			Conclusion:  cr.Conclusion,
			QueuedAt:    queuedAt,
			StartedAt:   startedAt,
			CompletedAt: completedAt,
		}
	}
	return runs, nil
}

// GetJobLog fetches the log for a GitHub Actions job, capped at 100 MB. When
// failingOnly is true, the log is trimmed to only the steps that contain
// ##[error] markers; if no such steps are found the full log is returned.
func (c *Client) GetJobLog(ctx context.Context, owner, repo string, jobID int64, failingOnly bool) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", c.apiBase(), owner, repo, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return "", err
	}
	// Don't auto-follow the redirect: the GitHub API returns 302 to a pre-signed
	// blob storage URL, and our transport injects Authorization on every request.
	// Sending the GitHub token to Azure's pre-signed URL causes a 403.
	noFollow := *c.HTTPClient
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("github get job log: expected redirect, got status %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", errors.New("github get job log: redirect has no Location header")
	}
	// Fetch the pre-signed URL without any auth headers.
	blobReq, err := http.NewRequestWithContext(ctx, http.MethodGet, location, http.NoBody)
	if err != nil {
		return "", err
	}
	blobResp, err := http.DefaultClient.Do(blobReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = blobResp.Body.Close() }()
	if blobResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(blobResp.Body)
		return "", fmt.Errorf("github get job log: status %d: %s", blobResp.StatusCode, data)
	}
	log, err := forge.ReadLog(blobResp.Body)
	if err != nil {
		return "", err
	}
	if failingOnly {
		log = extractGitHubSteps(log)
	}
	return log, nil
}

// GetJobLabels returns the runner labels for a GitHub Actions job by fetching
// the job details endpoint. Labels typically include the runner OS (e.g.
// "ubuntu-latest").
func (c *Client) GetJobLabels(ctx context.Context, owner, repo string, jobID int64) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/jobs/%d", owner, repo, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github get job: status %d: %s", resp.StatusCode, data)
	}
	var r jobResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Labels, nil
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

// mergePRRequest is the JSON body for PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge.
type mergePRRequest struct {
	CommitTitle   string `json:"commit_title"`
	CommitMessage string `json:"commit_message"`
	MergeMethod   string `json:"merge_method"`
}

// MergePR squash-merges a pull request on GitHub.
func (c *Client) MergePR(ctx context.Context, owner, repo string, prNumber int, commitTitle, commitMessage string) error {
	payload, err := json.Marshal(mergePRRequest{
		CommitTitle:   commitTitle,
		CommitMessage: commitMessage,
		MergeMethod:   "squash",
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/merge", owner, repo, prNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github merge PR: status %d: %s", resp.StatusCode, data)
	}
	return nil
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
	resp, err := c.HTTPClient.Do(req)
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

// extractGitHubSteps returns the content of ##[group]…##[endgroup] sections
// that contain at least one ##[error] line. Falls back to rawLog when no
// groups or no errors are found.
func extractGitHubSteps(rawLog string) string {
	type section struct {
		header string
		body   string
		hasErr bool
	}
	var sections []section
	var cur *section
	for line := range strings.SplitSeq(rawLog, "\n") {
		stripped := stripTimestamp(line)
		switch {
		case strings.HasPrefix(stripped, "##[group]"):
			sections = append(sections, section{header: strings.TrimPrefix(stripped, "##[group]")})
			cur = &sections[len(sections)-1]
		case strings.HasPrefix(stripped, "##[endgroup]"):
			cur = nil
		default:
			if cur != nil {
				cur.body += line + "\n"
				if strings.HasPrefix(stripped, "##[error]") {
					cur.hasErr = true
				}
			}
		}
	}
	// Collect sections that have errors.
	var sb strings.Builder
	for i := range sections {
		if !sections[i].hasErr {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "Step: %s\n%s", sections[i].header, sections[i].body)
	}
	if sb.Len() == 0 {
		return rawLog
	}
	return strings.TrimRight(sb.String(), "\n")
}

// stripTimestamp removes the leading ISO-8601 timestamp that GitHub Actions
// prepends to each log line (e.g. "2024-01-01T00:00:00.1234567Z ").
func stripTimestamp(line string) string {
	// Timestamps are exactly 28 chars: "YYYY-MM-DDTHH:MM:SS.fffffffZ "
	if len(line) > 29 && line[4] == '-' && line[10] == 'T' && line[27] == 'Z' && line[28] == ' ' {
		return line[29:]
	}
	return line
}
