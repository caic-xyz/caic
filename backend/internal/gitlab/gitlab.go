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

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Client is a minimal GitLab API client authenticated with a personal access token.
// It implements forge.Forge.
type Client struct {
	Token string
}

var _ forge.Forge = (*Client)(nil)

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
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"` // "pending", "running", "success", "failed", "canceled"
	AllowFailure bool   `json:"allow_failure"`
	TargetURL    string `json:"target_url"` // e.g. https://gitlab.com/owner/repo/-/jobs/{jobID}
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
		return forge.PR{}, fmt.Errorf("gitlab create MR: status %d: %s", resp.StatusCode, data)
	}
	var r createMRResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return forge.PR{}, err
	}
	return forge.PR{Number: r.IID, HeadSHA: r.SHA}, nil
}

// GetDefaultBranchSHA returns the HEAD commit SHA of branch in the given repo.
func (c *Client) GetDefaultBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/repository/branches/%s", apiBase, projectID(owner, repo), url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
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
		runs[i] = forge.CheckRun{
			JobID:      s.ID,
			RunID:      0, // GitLab job URLs don't require a separate run ID
			Name:       s.Name,
			Status:     gitLabStatus(s.Status),
			Conclusion: gitLabConclusion(s.Status, s.AllowFailure),
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

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	req.Header.Set("Content-Type", "application/json")
}

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

// GetJobLog fetches the log for a GitLab CI job and returns the last
// maxBytes bytes of its content. maxBytes <= 0 means no limit.
func (c *Client) GetJobLog(ctx context.Context, owner, repo string, jobID int64, maxBytes int) (string, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/jobs/%d/trace", apiBase, projectID(owner, repo), jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab get job log: status %d: %s", resp.StatusCode, data)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return string(data), nil
}
