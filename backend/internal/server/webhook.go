// Webhook event handlers for GitHub webhook delivery.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/cicache"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/github"
	"github.com/caic-xyz/caic/backend/internal/task"
)

const maxWebhookBodyBytes = 10 << 20 // 10 MB

// handleGitHubWebhook handles POST /webhooks/github.
// It verifies the HMAC signature and dispatches on X-GitHub-Event.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if len(s.githubWebhookSecret) == 0 {
		http.Error(w, "webhooks not configured", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20)) // 25 MB limit
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if err := github.VerifySignature(s.githubWebhookSecret, body, sig); err != nil {
		slog.Warn("webhook signature mismatch", "err", err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	slog.Info("github webhook", "event", event) //nolint:gosec // G706: event name is from GitHub header, not user input
	switch event {
	case "issues":
		var ev github.IssuesEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		s.handleIssuesEvent(r.Context(), &ev)
	case "pull_request":
		var ev github.PullRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		s.handlePullRequestEvent(r.Context(), &ev)
	case "issue_comment":
		var ev github.IssueCommentEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		s.handleIssueCommentEvent(r.Context(), &ev)
	case "installation":
		var ev github.InstallationEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		s.handleInstallationEvent(r.Context(), &ev)
	case "check_suite":
		var ev github.CheckSuiteEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		s.handleCheckSuiteEvent(r.Context(), &ev)
	case "check_run":
		var ev githubCheckRunEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if ev.CheckRun.Status == "completed" {
			owner, repo, _ := strings.Cut(ev.Repository.FullName, "/")
			if owner != "" && repo != "" && ev.CheckRun.HeadSHA != "" {
				go s.webhookOnCI(s.ctx, forge.KindGitHub, owner, repo, ev.CheckRun.HeadSHA) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
			}
		}
	case "workflow_run":
		var ev githubWorkflowRunEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if ev.WorkflowRun.Status == "completed" {
			owner, repo, _ := strings.Cut(ev.Repository.FullName, "/")
			if owner != "" && repo != "" && ev.WorkflowRun.HeadSHA != "" {
				go s.webhookOnCI(s.ctx, forge.KindGitHub, owner, repo, ev.WorkflowRun.HeadSHA) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
			}
		}
	default:
		// Unknown event — silently ignore, return 200.
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGitLabWebhook verifies the X-Gitlab-Token header and dispatches
// Pipeline Hook events.
func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	if len(s.gitlabWebhookSecret) == 0 {
		http.Error(w, "webhooks not configured", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Verify secret token using constant-time compare.
	token := r.Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(token), s.gitlabWebhookSecret) != 1 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	if r.Header.Get("X-Gitlab-Event") != "Pipeline Hook" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var ev gitlabPipelineEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Only dispatch on terminal pipeline statuses.
	switch ev.ObjectAttributes.Status {
	case "success", "failed", "canceled", "skipped":
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	sha := ev.ObjectAttributes.SHA
	owner, repo, _ := strings.Cut(ev.Project.PathWithNamespace, "/")
	if owner != "" && repo != "" && sha != "" {
		go s.webhookOnCI(s.ctx, forge.KindGitLab, owner, repo, sha) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhookOnCI handles a CI completion event from a forge webhook by fetching
// the current check-run state and updating affected tasks and repos.
func (s *Server) webhookOnCI(ctx context.Context, kind forge.Kind, owner, repo, sha string) {
	f := s.forgeFor(ctx, kind)
	if f == nil {
		return
	}

	// Collect affected task entries and repo paths under the lock.
	s.mu.Lock()
	var affected []*taskEntry
	for _, e := range s.tasks {
		if e.monitorBranch != "" {
			snap := e.task.Snapshot()
			if snap.ForgeOwner == owner && snap.ForgeRepo == repo {
				if p := e.task.Primary(); p != nil && p.Branch == e.monitorBranch {
					affected = append(affected, e)
				}
			}
		}
	}
	var affectedRepoPaths []string
	for i := range s.repos { // s.repos is immutable after construction
		info := &s.repos[i]
		if info.ForgeOwner == owner && info.ForgeRepo == repo {
			if s.repoCIStatus[info.RelPath].HeadSHA == sha {
				affectedRepoPaths = append(affectedRepoPaths, info.RelPath)
			}
		}
	}
	s.mu.Unlock()

	if len(affected) == 0 && len(affectedRepoPaths) == 0 {
		return
	}

	runs, err := f.GetCheckRuns(ctx, owner, repo, sha)
	if err != nil {
		slog.Warn("webhookOnCI: get check-runs", "owner", owner, "repo", repo, "sha", sha[:min(7, len(sha))], "err", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	result, done := bot.EvaluateCheckRuns(owner, repo, runs)

	// Pre-compute interim status once for all affected tasks/repos.
	var interimStatus task.CIStatus
	var interimChecks []task.CICheck
	if !done {
		interimStatus, interimChecks = bot.InterimCIStatus(runs, result.Checks)
	}

	for _, e := range affected {
		if !done {
			e.task.SetCIStatus(interimStatus, interimChecks)
			s.notifyTaskChange()
			continue
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("webhookOnCI: cache put", "err", err)
		}
		s.applyMonitorCIResult(ctx, e, f, owner, repo, sha, result)
	}

	for _, relPath := range affectedRepoPaths {
		if !done {
			repoStatus := cicache.StatusPending
			if interimStatus == task.CIStatusFailure {
				repoStatus = cicache.StatusFailure
			}
			s.setRepoCIStatus(relPath, sha, cicache.Result{Status: repoStatus, Checks: result.Checks})
			continue
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("webhookOnCI: cache put", "err", err)
		}
		s.setRepoCIStatus(relPath, sha, result)
	}
}

// handleIssuesEvent creates a task when a labeled issue is opened.
// Trigger: action=="opened" AND label "caic" present.
func (s *Server) handleIssuesEvent(ctx context.Context, ev *github.IssuesEvent) {
	if ev.Action != "opened" {
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	labels := make([]string, len(ev.Issue.Labels))
	for i, l := range ev.Issue.Labels {
		labels[i] = l.Name
	}
	s.bot.OnIssueOpened(ctx, &bot.IssueEvent{
		ForgeFullName: ev.Repository.FullName,
		Number:        ev.Issue.Number,
		Title:         ev.Issue.Title,
		Body:          ev.Issue.Body,
		HTMLURL:       ev.Issue.HTMLURL,
		Labels:        labels,
	}, s.commenterFor(ev.Installation.ID))
}

// handlePullRequestEvent creates a task when a PR is opened or reopened,
// or updates an existing task if a PR is opened for a branch that already
// has a container/task but no PR yet.
// Trigger: action=="opened" OR action=="reopened".
func (s *Server) handlePullRequestEvent(ctx context.Context, ev *github.PullRequestEvent) {
	// First, try to create a new task (existing behavior).
	if ev.Action == "opened" || ev.Action == "reopened" {
		s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
		s.bot.OnPROpened(ctx, &bot.PREvent{
			ForgeFullName: ev.Repository.FullName,
			Number:        ev.PullRequest.Number,
			Title:         ev.PullRequest.Title,
			Body:          ev.PullRequest.Body,
			HTMLURL:       ev.PullRequest.HTMLURL,
			HeadRef:       ev.PullRequest.Head.Ref,
			BaseRef:       ev.PullRequest.Base.Ref,
		})
	}

	// Also check if this PR is for an existing task that doesn't have a PR yet.
	// This handles the case where a user creates a PR outside of caic.
	s.handlePRForExistingTask(ctx, ev)
}

// handlePRForExistingTask updates an existing task with PR info if the PR head
// branch matches a task's branch but the task doesn't have a PR yet.
func (s *Server) handlePRForExistingTask(ctx context.Context, ev *github.PullRequestEvent) {
	// Only handle PR open, reopen, or synchronize actions.
	if ev.Action != "opened" && ev.Action != "reopened" && ev.Action != "synchronize" {
		return
	}
	owner, repo, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repo == "" {
		return
	}
	branch := ev.PullRequest.Head.Ref
	if branch == "" {
		return
	}
	prNumber := ev.PullRequest.Number
	sha := ev.PullRequest.Head.SHA

	// Find tasks matching this repo and branch.
	s.mu.Lock()
	var matchingEntries []*taskEntry
	for _, e := range s.tasks {
		snap := e.task.Snapshot()
		if snap.ForgeOwner == owner && snap.ForgeRepo == repo {
			// Check if the primary repo branch matches.
			if p := e.task.Primary(); p != nil && p.Branch == branch {
				matchingEntries = append(matchingEntries, e)
			}
		}
	}
	s.mu.Unlock()

	for _, entry := range matchingEntries {
		snap := entry.task.Snapshot()
		if snap.ForgePR == 0 {
			// Task has no PR yet — set the PR info.
			slog.Info("webhook: associating external PR with existing task",
				"task", entry.task.ID, "repo", owner+"/"+repo, "br", branch, "pr", prNumber)
			entry.task.SetPR(owner, repo, prNumber)
			// Start CI monitoring.
			ri := s.repoByForge(owner + "/" + repo)
			if ri != nil {
				f := s.forgeFor(ctx, ri.ForgeKind)
				if f != nil {
					s.mu.Lock()
					entry.monitorBranch = branch
					s.mu.Unlock()
					go s.monitorCI(ctx, entry, f, owner, repo, sha)
				}
			}
		} else if snap.ForgePR == prNumber && ev.Action == "synchronize" {
			// PR already exists, but new commits were pushed — restart CI monitoring.
			slog.Info("webhook: restarting CI monitor for PR",
				"task", entry.task.ID, "repo", owner+"/"+repo, "br", branch, "pr", prNumber, "sha", sha[:min(7, len(sha))])
			ri := s.repoByForge(owner + "/" + repo)
			if ri != nil {
				go s.monitorCI(ctx, entry, s.forgeFor(ctx, ri.ForgeKind), owner, repo, sha)
			}
		}
	}
}

// handleIssueCommentEvent creates a task when @caic is mentioned in a comment.
// Trigger: action=="created" AND body contains "@caic".
func (s *Server) handleIssueCommentEvent(ctx context.Context, ev *github.IssueCommentEvent) {
	if ev.Action != "created" {
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	s.bot.OnIssueComment(ctx, bot.CommentEvent{
		ForgeFullName: ev.Repository.FullName,
		IssueNumber:   ev.Issue.Number,
		IssueTitle:    ev.Issue.Title,
		CommentBody:   ev.Comment.Body,
		CommentURL:    ev.Comment.HTMLURL,
	}, s.commenterFor(ev.Installation.ID))
}

// handleInstallationEvent enforces the owner allowlist on new installs.
// When GITHUB_APP_ALLOWED_OWNERS is set and the installing account is not in
// the list, the installation is deleted immediately.
func (s *Server) handleInstallationEvent(ctx context.Context, ev *github.InstallationEvent) {
	if ev.Action != "created" {
		return
	}
	login := ev.Installation.Account.Login
	if s.githubAppAllowedOwners == nil {
		s.storeInstallationID(login, ev.Installation.ID)
		return
	}
	if _, ok := s.githubAppAllowedOwners[strings.ToLower(login)]; ok {
		s.storeInstallationID(login, ev.Installation.ID)
		return
	}
	slog.Warn("github app: rejecting installation from non-allowed owner", "owner", login, "installation_id", ev.Installation.ID)
	if err := s.githubApp.DeleteInstallation(ctx, ev.Installation.ID); err != nil {
		slog.Warn("github app: delete installation failed", "owner", login, "err", err)
	}
}

// handleCheckSuiteEvent updates CI status when a check suite completes.
// It caches the result, updates default-branch repo CI status, and delivers
// the terminal result to any task that was monitoring that SHA.
func (s *Server) handleCheckSuiteEvent(ctx context.Context, ev *github.CheckSuiteEvent) {
	if ev.Action != "completed" {
		return
	}
	repo := s.repoByForge(ev.Repository.FullName)
	if repo == nil {
		return // not a repo we manage
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)

	sha := ev.CheckSuite.HeadSHA
	client, err := s.githubApp.ForgeClient(ctx, ev.Installation.ID)
	if err != nil {
		slog.Warn("handleCheckSuiteEvent: forge client", "err", err)
		return
	}

	runs, err := client.GetCheckRuns(ctx, repo.ForgeOwner, repo.ForgeRepo, sha)
	if err != nil {
		slog.Warn("handleCheckSuiteEvent: get check-runs", "sha", sha, "err", err)
		return
	}
	result, done := bot.EvaluateCheckRuns(repo.ForgeOwner, repo.ForgeRepo, runs)
	if !done {
		return
	}
	if err := s.ciCache.Put(repo.ForgeOwner, repo.ForgeRepo, sha, result); err != nil {
		slog.Warn("handleCheckSuiteEvent: cache put", "err", err)
	}

	// Update default-branch CI status only when this SHA is still the HEAD of
	// the default branch. Webhooks may arrive out of order, so an older commit's
	// check suite could complete after a newer one's; skipping stale events
	// prevents the displayed CI status from regressing.
	if ev.CheckSuite.HeadBranch == repo.BaseBranch || repo.BaseBranch == "" {
		headSHA, err := client.GetDefaultBranchSHA(ctx, repo.ForgeOwner, repo.ForgeRepo, repo.BaseBranch)
		switch {
		case err != nil:
			slog.Warn("handleCheckSuiteEvent: get HEAD SHA", "repo", repo.RelPath, "err", err)
		case headSHA == sha:
			s.setRepoCIStatus(repo.RelPath, sha, result)
		default:
			slog.Debug("handleCheckSuiteEvent: ignoring stale check suite", "sha", sha, "head", headSHA)
		}
	}

	// Deliver the result to any task monitoring this SHA.
	// Match by branch since multiple commits can have the same SHA across different branches.
	s.mu.Lock()
	var waiting []*taskEntry
	for _, entry := range s.tasks {
		if entry.monitorBranch != "" {
			snap := entry.task.Snapshot()
			if snap.ForgeOwner == repo.ForgeOwner && snap.ForgeRepo == repo.ForgeRepo {
				if p := entry.task.Primary(); p != nil && p.Branch == entry.monitorBranch {
					waiting = append(waiting, entry)
				}
			}
		}
	}
	s.mu.Unlock()
	for _, entry := range waiting {
		go s.applyMonitorCIResult(s.ctx, entry, client, repo.ForgeOwner, repo.ForgeRepo, sha, result) //nolint:contextcheck // fire-and-forget; must outlive webhook request
	}
}

// storeInstallationIDFromFullName extracts the owner from "owner/repo" and
// stores the installation ID for that owner.
func (s *Server) storeInstallationIDFromFullName(fullName string, id int64) {
	owner, _, ok := strings.Cut(fullName, "/")
	if ok {
		s.storeInstallationID(owner, id)
	}
}

// repoByForge finds the repoInfo whose forge matches "owner/repo".
func (s *Server) repoByForge(fullName string) *repoInfo {
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil
	}
	for i := range s.repos {
		r := &s.repos[i]
		if strings.EqualFold(r.ForgeOwner, owner) && strings.EqualFold(r.ForgeRepo, repo) {
			return r
		}
	}
	return nil
}

// commenterFor returns a bot.Commenter for posting comments via the GitHub App
// (when installationID is non-zero) or the configured PAT, or nil if neither
// is available.
func (s *Server) commenterFor(installationID int64) bot.Commenter {
	if s.githubApp != nil && installationID != 0 {
		return &appInstallCommenter{app: s.githubApp, installationID: installationID}
	}
	if s.githubToken != "" {
		return github.NewClient(s.githubToken, s.githubPATThrottle)
	}
	return nil
}

// appInstallCommenter adapts githubAppClient.PostComment to bot.Commenter by
// binding a fixed installation ID.
type appInstallCommenter struct {
	app            githubAppClient
	installationID int64
}

func (c *appInstallCommenter) PostComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	return c.app.PostComment(ctx, c.installationID, owner, repo, issueNumber, body)
}
