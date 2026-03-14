// Package gitlab implements forge.Forge for gitlab.com using the GitLab REST API.
// Uses net/http directly; no extra dependencies.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/maruel/roundtrippers"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Client is a minimal GitLab API client authenticated with a personal access token.
// It implements forge.Forge.
type Client struct {
	HTTPClient *http.Client
}

var _ forge.Forge = (*Client)(nil)

// NewClient returns a Client that authenticates with token and throttles/retries
// via throttle. The transport chain is: Header → Retry → throttle.
func NewClient(token string, throttle http.RoundTripper) *Client {
	return &Client{
		HTTPClient: &http.Client{
			Transport: &roundtrippers.Header{
				Transport: &roundtrippers.Retry{Transport: throttle},
				Header: http.Header{
					"PRIVATE-TOKEN": {token},
					"Content-Type":  {"application/json"},
				},
			},
		},
	}
}

const apiBase = "https://gitlab.com/api/v4"

// projectID returns the URL-encoded "namespace/repo" project identifier used in GitLab API paths.
func projectID(owner, repo string) string {
	return url.PathEscape(owner + "/" + repo)
}

// createMRRequest is the JSON body for POST /projects/{id}/merge_requests.
type createMRRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

// createMRResponse is the relevant subset of the GitLab MR creation response.
type createMRResponse struct {
	IID int    `json:"iid"` // Internal project MR number (shown in UI).
	SHA string `json:"sha"` // HEAD commit SHA of the source branch.
}

// branchResponse is the relevant subset of the GitLab branch response.
type branchResponse struct {
	Commit struct {
		ID string `json:"id"` // Commit SHA.
	} `json:"commit"`
}

// commitStatus is one entry from the GitLab commit statuses API.
type commitStatus struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"` // "pending", "running", "success", "failed", "canceled"
	AllowFailure bool       `json:"allow_failure"`
	TargetURL    string     `json:"target_url"` // e.g. https://gitlab.com/owner/repo/-/jobs/{jobID}
	CreatedAt    *time.Time `json:"created_at"`
}

// CreatePR creates a merge request on GitLab and returns its metadata.
func (c *Client) CreatePR(ctx context.Context, owner, repo, head, base, title, body string) (forge.PR, error) {
	payload, err := json.Marshal(createMRRequest{
		SourceBranch: head,
		TargetBranch: base,
		Title:        title,
		Description:  body,
	})
	if err != nil {
		return forge.PR{}, err
	}
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests", apiBase, projectID(owner, repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
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
		return forge.PR{}, fmt.Errorf("gitlab create MR: status %d: %s", resp.StatusCode, data)
	}
	var r createMRResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return forge.PR{}, err
	}
	return forge.PR{Number: r.IID, HeadSHA: r.SHA}, nil
}

// FindPRByBranch returns the MR for the given source branch, or ErrNotFound
// if no MR exists for that branch.
func (c *Client) FindPRByBranch(ctx context.Context, owner, repo, sourceBranch string) (forge.PR, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests?source_branch=%s&state=opened", apiBase, projectID(owner, repo), url.QueryEscape(sourceBranch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
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
		return forge.PR{}, fmt.Errorf("gitlab search MRs: status %d: %s", resp.StatusCode, data)
	}
	var mrs []struct {
		IID   int    `json:"iid"`
		SHA   string `json:"sha"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &mrs); err != nil {
		return forge.PR{}, err
	}
	if len(mrs) == 0 {
		return forge.PR{}, fmt.Errorf("no MR found for branch %q: %w", sourceBranch, forge.ErrNotFound)
	}
	// Return the first matching MR.
	mr := mrs[0]
	return forge.PR{Number: mr.IID, HeadSHA: mr.SHA}, nil
}

// GetDefaultBranchSHA returns the HEAD commit SHA of branch in the given repo.
func (c *Client) GetDefaultBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/repository/branches/%s", apiBase, projectID(owner, repo), url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
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
		return "", fmt.Errorf("gitlab get branch: %w", forge.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab get branch: status %d: %s", resp.StatusCode, data)
	}
	var r branchResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", err
	}
	return r.Commit.ID, nil
}

// GetCheckRuns returns all CI pipeline job statuses for the given commit SHA.
func (c *Client) GetCheckRuns(ctx context.Context, owner, repo, sha string) ([]forge.CheckRun, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/repository/commits/%s/statuses", apiBase, projectID(owner, repo), url.PathEscape(sha))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
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
		return nil, fmt.Errorf("gitlab get statuses: %w", forge.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitlab get statuses: status %d: %s", resp.StatusCode, data)
	}
	var statuses []commitStatus
	if err := json.Unmarshal(data, &statuses); err != nil {
		return nil, err
	}
	runs := make([]forge.CheckRun, len(statuses))
	for i, s := range statuses {
		var queuedAt time.Time
		if s.CreatedAt != nil {
			queuedAt = *s.CreatedAt
		}
		runs[i] = forge.CheckRun{
			JobID:      s.ID,
			RunID:      0, // GitLab job URLs don't require a separate run ID
			Name:       s.Name,
			Status:     gitLabStatus(s.Status),
			Conclusion: gitLabConclusion(s.Status, s.AllowFailure),
			QueuedAt:   queuedAt,
		}
	}
	return runs, nil
}

// PRURL returns the GitLab merge request URL.
func (c *Client) PRURL(owner, repo string, prNumber int) string {
	return fmt.Sprintf("https://gitlab.com/%s/%s/-/merge_requests/%d", owner, repo, prNumber)
}

// PRLabel returns a GitLab-style MR label.
func (c *Client) PRLabel(prNumber int) string {
	return fmt.Sprintf("MR #%d", prNumber)
}

// CIJobURL returns the GitLab job URL.
func (c *Client) CIJobURL(owner, repo string, _, jobID int64) string {
	if jobID > 0 {
		return fmt.Sprintf("https://gitlab.com/%s/%s/-/jobs/%d", owner, repo, jobID)
	}
	return ""
}

// CIHomeURL returns the GitLab pipelines overview URL for a repo.
func (c *Client) CIHomeURL(remoteURL string) string {
	return remoteURL + "/-/pipelines"
}

// BranchCompareURL returns the GitLab compare URL for a branch.
func (c *Client) BranchCompareURL(remoteURL, branch string) string {
	return remoteURL + "/-/compare/" + branch + "?expand=1"
}

// Name returns "GitLab".
func (c *Client) Name() string { return "GitLab" }

// gitLabStatus maps GitLab pipeline status strings to forge.CheckRunStatus.
func gitLabStatus(status string) forge.CheckRunStatus {
	switch status {
	case "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		return forge.CheckRunStatusQueued
	case "running":
		return forge.CheckRunStatusInProgress
	default: // "success", "failed", "canceled", "skipped", "manual"
		return forge.CheckRunStatusCompleted
	}
}

// gitLabConclusion maps GitLab terminal status strings to forge.CheckRunConclusion.
// Returns an empty conclusion for non-terminal statuses.
func gitLabConclusion(status string, allowFailure bool) forge.CheckRunConclusion {
	switch status {
	case "success":
		return forge.CheckRunConclusionSuccess
	case "failed":
		if allowFailure {
			return forge.CheckRunConclusionNeutral
		}
		return forge.CheckRunConclusionFailure
	case "canceled":
		return forge.CheckRunConclusionCancelled
	case "skipped", "manual":
		return forge.CheckRunConclusionSkipped
	default:
		return ""
	}
}

// mergeMRRequest is the JSON body for PUT /projects/{id}/merge_requests/{mr_iid}/merge.
type mergeMRRequest struct {
	Squash              bool   `json:"squash"`
	SquashCommitMessage string `json:"squash_commit_message"`
}

// MergePR squash-merges a merge request on GitLab.
func (c *Client) MergePR(ctx context.Context, owner, repo string, prNumber int, commitTitle, commitMessage string) error {
	msg := commitTitle
	if commitMessage != "" {
		msg += "\n\n" + commitMessage
	}
	payload, err := json.Marshal(mergeMRRequest{
		Squash:              true,
		SquashCommitMessage: msg,
	})
	if err != nil {
		return err
	}
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d/merge", apiBase, projectID(owner, repo), prNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, apiURL, bytes.NewReader(payload))
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
		return fmt.Errorf("gitlab merge MR: status %d: %s", resp.StatusCode, data)
	}
	return nil
}

// GetJobLabels returns nil for GitLab; the commit statuses API does not
// expose runner labels.
func (c *Client) GetJobLabels(_ context.Context, _, _ string, _ int64) ([]string, error) {
	return nil, nil
}

// GetJobLog fetches the log for a GitLab CI job, capped at 100 MB.
// failingOnly is accepted but has no effect: GitLab's plain-text trace API
// does not include reliable step-level markers.
func (c *Client) GetJobLog(ctx context.Context, owner, repo string, jobID int64, _ bool) (string, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/jobs/%d/trace", apiBase, projectID(owner, repo), jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gitlab get job log: status %d: %s", resp.StatusCode, data)
	}
	return forge.ReadLog(resp.Body)
}
