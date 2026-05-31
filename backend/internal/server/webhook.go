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
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/forge/gitlab"
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
	event := r.Header.Get("X-Github-Event")
	slog.Info("github webhook", "event", event)
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
		var ev github.CheckRunEvent
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
		var ev github.WorkflowRunEvent
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
// Pipeline Hook and Merge Request Hook events.
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

	event := r.Header.Get("X-Gitlab-Event")

	switch event {
	case "Pipeline Hook":
		var ev gitlab.PipelineEvent
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
	case "Merge Request Hook":
		var ev gitlab.MergeRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		s.handleGitLabMergeRequestEvent(&ev)
	default:
		// Unknown event — silently ignore, return 200.
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhookOnCI handles a CI completion event from a forge webhook by fetching
// the current check-run state and updating affected tasks and repos.
func (s *Server) webhookOnCI(ctx context.Context, kind forge.Kind, owner, repo, sha string) {
	f := s.forge.forgeFor(ctx, kind)
	if f == nil {
		return
	}

	affected := s.taskMgr.FindTasksMonitoringBranch(owner, repo)
	affectedRepoPaths := s.repoReg.forgePathsAtSHA(owner, repo, sha)

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
	var interimStatus forge.CIStatus
	if !done {
		interimStatus = bot.InterimCIStatus(runs)
	}

	for _, e := range affected {
		if !done {
			e.Task().SetCIStatus(interimStatus, result.Checks)
			s.taskMgr.NotifyTaskChange()
			continue
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("webhookOnCI: cache put", "err", err)
		}
		s.ciService.ApplyMonitorCIResult(ctx, e, f, owner, repo, sha, result)
	}

	for _, relPath := range affectedRepoPaths {
		if !done {
			repoStatus := forge.CIStatusPending
			if interimStatus == forge.CIStatusFailure {
				repoStatus = forge.CIStatusFailure
			}
			s.ciService.SetRepoCIStatus(relPath, sha, forgecache.Result{Status: repoStatus, Checks: result.Checks})
			continue
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("webhookOnCI: cache put", "err", err)
		}
		s.ciService.SetRepoCIStatus(relPath, sha, forgecache.Result{Status: result.Status, Checks: result.Checks})
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
	s.Bot.OnIssueOpened(ctx, &bot.IssueEvent{
		ForgeFullName: ev.Repository.FullName,
		Number:        ev.Issue.Number,
		Title:         ev.Issue.Title,
		Body:          ev.Issue.Body,
		HTMLURL:       ev.Issue.HTMLURL,
		Labels:        labels,
	}, s.forge.commenterFor(ev.Installation.ID))
}

// handlePullRequestEvent creates a task when a PR is opened or reopened,
// updates PR state when closed, or updates an existing task if a PR is
// opened for a branch that already has a container/task but no PR yet.
// Trigger: action=="opened", "reopened", or "closed".
func (s *Server) handlePullRequestEvent(ctx context.Context, ev *github.PullRequestEvent) {
	// Create a new task to review/fix the PR if auto-fix on PR open is enabled.
	if (ev.Action == "opened" || ev.Action == "reopened") && s.prefs.Get("default").Settings.AutoFixOnPROpen {
		s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
		s.Bot.OnPROpened(ctx, &bot.PREvent{
			ForgeFullName: ev.Repository.FullName,
			Number:        ev.PullRequest.Number,
			Title:         ev.PullRequest.Title,
			Body:          ev.PullRequest.Body,
			HTMLURL:       ev.PullRequest.HTMLURL,
			HeadRef:       ev.PullRequest.Head.Ref,
			BaseRef:       ev.PullRequest.Base.Ref,
		})
	}

	// Handle PR closed (merged or closed) or reopened.
	switch ev.Action {
	case "closed":
		s.handlePRClosedEvent(ev)
	case "reopened":
		s.handlePRReopenedEvent(ev)
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

	matchingEntries := s.taskMgr.FindTasksMatchingBranch(owner, repo, branch)
	for _, entry := range matchingEntries {
		snap := entry.Task().Snapshot()
		if snap.ForgePR == 0 {
			// Task has no PR yet — set the PR info.
			slog.Info("webhook: associating external PR with existing task",
				"task", entry.Task().ID, "repo", owner+"/"+repo, "br", branch, "pr", prNumber)
			entry.Task().SetPR(owner, repo, prNumber)
			// Start CI monitoring.
			if ri, ok := s.repoByForge(owner + "/" + repo); ok {
				f := s.forge.forgeFor(ctx, ri.ForgeKind)
				if f != nil {
					entry.SetMonitorBranch(branch)
					go s.ciService.MonitorCI(ctx, entry, f, owner, repo, sha)
				}
			}
		} else if snap.ForgePR == prNumber && ev.Action == "synchronize" {
			// PR already exists, but new commits were pushed — restart CI monitoring.
			slog.Info("webhook: restarting CI monitor for PR",
				"task", entry.Task().ID, "repo", owner+"/"+repo, "br", branch, "pr", prNumber, "sha", sha[:min(7, len(sha))])
			if ri, ok := s.repoByForge(owner + "/" + repo); ok {
				go s.ciService.MonitorCI(ctx, entry, s.forge.forgeFor(ctx, ri.ForgeKind), owner, repo, sha)
			}
		}
	}
}

// handlePRClosedEvent updates the PR state for tasks whose PR was closed or merged.
func (s *Server) handlePRClosedEvent(ev *github.PullRequestEvent) {
	owner, repo, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repo == "" {
		return
	}
	prNumber := ev.PullRequest.Number

	matchingEntries := s.taskMgr.FindTasksByPR(owner, repo, prNumber)
	for _, entry := range matchingEntries {
		// Determine state: "merged" if merged, otherwise "closed".
		var state forge.PRState
		if ev.PullRequest.Merged {
			state = forge.PRStateMerged
		} else {
			state = forge.PRStateClosed
		}
		slog.Info("webhook: PR closed/merged", "task", entry.Task().ID, "repo", owner+"/"+repo, "pr", prNumber, "state", state)
		entry.Task().SetPRState(state)
		s.taskMgr.NotifyTaskChange()
	}
}

// handlePRReopenedEvent resets the PR state to "open" when a closed PR is reopened.
func (s *Server) handlePRReopenedEvent(ev *github.PullRequestEvent) {
	owner, repo, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repo == "" {
		return
	}
	prNumber := ev.PullRequest.Number

	for _, entry := range s.taskMgr.FindTasksByPR(owner, repo, prNumber) {
		slog.Info("webhook: PR reopened", "task", entry.Task().ID, "repo", owner+"/"+repo, "pr", prNumber)
		entry.Task().SetPRState(forge.PRStateOpen)
		s.taskMgr.NotifyTaskChange()
	}
}

// handleGitLabMergeRequestEvent handles GitLab merge request state changes.
func (s *Server) handleGitLabMergeRequestEvent(ev *gitlab.MergeRequestEvent) {
	owner, repo, _ := strings.Cut(ev.Project.PathWithNamespace, "/")
	if owner == "" || repo == "" {
		return
	}
	mrIID := ev.ObjectAttributes.IID

	for _, entry := range s.taskMgr.FindTasksByPR(owner, repo, mrIID) {
		state := forge.PRStateOpen
		if ev.ObjectAttributes.State == "closed" {
			if ev.ObjectAttributes.Merged {
				state = forge.PRStateMerged
			} else {
				state = forge.PRStateClosed
			}
		}
		slog.Info("webhook: GitLab MR state", "task", entry.Task().ID, "repo", owner+"/"+repo, "mr", mrIID, "state", state)
		entry.Task().SetPRState(state)
		s.taskMgr.NotifyTaskChange()
	}
}

// handleIssueCommentEvent creates a task when @caic is mentioned in a comment.
// Trigger: action=="created" AND body contains "@caic".
func (s *Server) handleIssueCommentEvent(ctx context.Context, ev *github.IssueCommentEvent) {
	if ev.Action != "created" {
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	s.Bot.OnIssueComment(ctx, bot.CommentEvent{
		ForgeFullName: ev.Repository.FullName,
		IssueNumber:   ev.Issue.Number,
		IssueTitle:    ev.Issue.Title,
		CommentBody:   ev.Comment.Body,
		CommentURL:    ev.Comment.HTMLURL,
	}, s.forge.commenterFor(ev.Installation.ID))
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
		s.forge.storeInstallationID(login, ev.Installation.ID)
		return
	}
	if _, ok := s.githubAppAllowedOwners[strings.ToLower(login)]; ok {
		s.forge.storeInstallationID(login, ev.Installation.ID)
		return
	}
	slog.Warn("github app: rejecting installation from non-allowed owner", "owner", login, "installation_id", ev.Installation.ID)
	if err := s.forge.githubApp.DeleteInstallation(ctx, ev.Installation.ID); err != nil {
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
	repo, ok := s.repoByForge(ev.Repository.FullName)
	if !ok {
		return // not a repo we manage
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)

	sha := ev.CheckSuite.HeadSHA
	client, err := s.forge.githubApp.ForgeClient(ctx, ev.Installation.ID)
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
			s.ciService.SetRepoCIStatus(repo.RelPath, sha, forgecache.Result{Status: result.Status, Checks: result.Checks})
		default:
			slog.Debug("handleCheckSuiteEvent: ignoring stale check suite", "sha", sha, "head", headSHA)
		}
	}

	// Deliver the result to any task monitoring this SHA.
	// Match by branch since multiple commits can have the same SHA across different branches.
	for _, entry := range s.taskMgr.FindTasksMonitoringBranch(repo.ForgeOwner, repo.ForgeRepo) {
		go s.ciService.ApplyMonitorCIResult(s.ctx, entry, client, repo.ForgeOwner, repo.ForgeRepo, sha, result) //nolint:contextcheck // fire-and-forget; must outlive webhook request
	}
}

// storeInstallationIDFromFullName extracts the owner from "owner/repo" and
// stores the installation ID for that owner.
func (s *Server) storeInstallationIDFromFullName(fullName string, id int64) {
	owner, _, ok := strings.Cut(fullName, "/")
	if ok {
		s.forge.storeInstallationID(owner, id)
	}
}

// repoByForge returns a copy of the repoInfo whose forge matches "owner/repo".
func (s *Server) repoByForge(fullName string) (repoInfo, bool) {
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return repoInfo{}, false
	}
	return s.repoReg.byForge(owner, repo)
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
