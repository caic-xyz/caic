// Package forge defines the interface for interacting with code hosting forges
// (GitHub, GitLab, etc.) and provides URL parsing and a factory function for
// selecting the right implementation based on a remote URL.
package forge

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ErrNotFound is returned when the forge API responds with 404, typically
// because the token lacks access to the resource.
var ErrNotFound = errors.New("not found")

// Kind identifies the code hosting forge.
type Kind string

// Supported forge kinds.
const (
	KindGitHub Kind = "github"
	KindGitLab Kind = "gitlab"
)

// PR holds the fields of a pull/merge request returned after creation.
type PR struct {
	Number  int
	HeadSHA string
}

// CheckRunStatus is the status of a CI check run.
type CheckRunStatus string

// Check run status values.
const (
	CheckRunStatusQueued     CheckRunStatus = "queued"
	CheckRunStatusInProgress CheckRunStatus = "in_progress"
	CheckRunStatusCompleted  CheckRunStatus = "completed"
)

// CheckRunConclusion is the conclusion of a completed CI check run.
// Empty when not yet completed.
type CheckRunConclusion string

// Check run conclusion values.
const (
	CheckRunConclusionSuccess        CheckRunConclusion = "success"
	CheckRunConclusionFailure        CheckRunConclusion = "failure"
	CheckRunConclusionNeutral        CheckRunConclusion = "neutral"
	CheckRunConclusionSkipped        CheckRunConclusion = "skipped"
	CheckRunConclusionCancelled      CheckRunConclusion = "cancelled"
	CheckRunConclusionTimedOut       CheckRunConclusion = "timed_out"
	CheckRunConclusionActionRequired CheckRunConclusion = "action_required"
	CheckRunConclusionStale          CheckRunConclusion = "stale"
)

// CheckRun describes a single CI check run.
type CheckRun struct {
	JobID      int64 // Job ID.
	RunID      int64 // Pipeline/workflow run ID; 0 if not available.
	Name       string
	Status     CheckRunStatus
	Conclusion CheckRunConclusion
}

// Forge is the interface for interacting with a code hosting forge.
type Forge interface {
	// CreatePR creates a pull/merge request and returns its metadata.
	CreatePR(ctx context.Context, owner, repo, head, base, title, body string) (PR, error)
	// GetCheckRuns returns all CI check runs for a commit SHA.
	GetCheckRuns(ctx context.Context, owner, repo, sha string) ([]CheckRun, error)
	// GetDefaultBranchSHA returns the HEAD commit SHA of the given branch.
	GetDefaultBranchSHA(ctx context.Context, owner, repo, branch string) (string, error)
	// PRURL returns the web URL for a pull/merge request.
	PRURL(owner, repo string, prNumber int) string
	// PRLabel returns the human-readable label for a pull/merge request number,
	// e.g. "PR #42" for GitHub or "MR #42" for GitLab.
	PRLabel(prNumber int) string
	// CIJobURL returns the web URL for a specific CI job.
	CIJobURL(owner, repo string, runID, jobID int64) string
	// CIHomeURL returns the CI overview web URL for a repo.
	CIHomeURL(remoteURL string) string
	// BranchCompareURL returns the URL to compare branch against base in the forge UI.
	BranchCompareURL(remoteURL, branch string) string
	// Name returns the forge name for display (e.g. "GitHub", "GitLab").
	Name() string
	// GetJobLog fetches the log for a CI job and returns the last maxBytes bytes.
	// maxBytes <= 0 means no limit.
	GetJobLog(ctx context.Context, owner, repo string, jobID int64, maxBytes int) (string, error)
}

// Remote URL regex patterns for supported forges.
var (
	ghHTTPS = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/?#]+?)(?:\.git)?$`)
	ghSSH   = regexp.MustCompile(`^git@github\.com:([^/]+)/([^/?#]+?)(?:\.git)?$`)
	glHTTPS = regexp.MustCompile(`^https?://gitlab\.com/([^/]+)/([^/?#]+?)(?:\.git)?$`)
	glSSH   = regexp.MustCompile(`^git@gitlab\.com:([^/]+)/([^/?#]+?)(?:\.git)?$`)
)

// ParseRemoteURL extracts the forge kind, owner, and repo name from a remote URL.
// Supports both HTTPS and SSH formats for github.com and gitlab.com.
func ParseRemoteURL(rawURL string) (kind Kind, owner, repo string, err error) {
	rawURL = strings.TrimSpace(rawURL)
	if m := ghHTTPS.FindStringSubmatch(rawURL); m != nil {
		return KindGitHub, m[1], m[2], nil
	}
	if m := ghSSH.FindStringSubmatch(rawURL); m != nil {
		return KindGitHub, m[1], m[2], nil
	}
	if m := glHTTPS.FindStringSubmatch(rawURL); m != nil {
		return KindGitLab, m[1], m[2], nil
	}
	if m := glSSH.FindStringSubmatch(rawURL); m != nil {
		return KindGitLab, m[1], m[2], nil
	}
	return "", "", "", fmt.Errorf("unrecognized forge remote URL: %q", rawURL)
}

// RemoteURL returns the URL of the "origin" remote for the git repository at dir.
func RemoteURL(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output() //nolint:gosec // dir is a trusted repo path
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
