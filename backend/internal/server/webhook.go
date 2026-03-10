// Webhook event handlers for GitHub webhook delivery.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/cicache"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/github"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
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
		if e.ciSHA == sha {
			snap := e.task.Snapshot()
			if snap.ForgeOwner == owner && snap.ForgeRepo == repo {
				affected = append(affected, e)
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

	result, done := evaluateCheckRuns(owner, repo, runs)

	for _, e := range affected {
		if !done {
			e.task.SetCIStatus(task.CIStatusPending, nil)
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
			s.setRepoCIStatus(relPath, sha, cicache.Result{Status: cicache.StatusPending})
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
	hasCaicLabel := false
	for _, l := range ev.Issue.Labels {
		if l.Name == "caic" {
			hasCaicLabel = true
			break
		}
	}
	if !hasCaicLabel {
		return
	}
	repo := s.repoByForge(ev.Repository.FullName)
	if repo == nil {
		slog.Warn("webhook: no repo for", "full_name", ev.Repository.FullName)
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	prompt := fmt.Sprintf("Fix the following GitHub issue:\n\nTitle: %s\nURL: %s\n\n%s",
		ev.Issue.Title, ev.Issue.HTMLURL, ev.Issue.Body)
	s.createWebhookTask(ctx, repo, prompt, ev.Installation.ID, ev.Repository.FullName, ev.Issue.Number)
}

// handlePullRequestEvent creates a task when a PR is opened or reopened.
// Trigger: action=="opened" OR action=="reopened".
func (s *Server) handlePullRequestEvent(ctx context.Context, ev *github.PullRequestEvent) {
	if ev.Action != "opened" && ev.Action != "reopened" {
		return
	}
	repo := s.repoByForge(ev.Repository.FullName)
	if repo == nil {
		slog.Warn("webhook: no repo for", "full_name", ev.Repository.FullName)
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	prompt := fmt.Sprintf("Review and fix the following pull request:\n\nTitle: %s\nBranch: %s → %s\nURL: %s\n\n%s",
		ev.PullRequest.Title,
		ev.PullRequest.Head.Ref,
		ev.PullRequest.Base.Ref,
		ev.PullRequest.HTMLURL,
		ev.PullRequest.Body)
	s.createWebhookTask(ctx, repo, prompt, 0, "", 0)
}

// handleIssueCommentEvent creates a task when @caic is mentioned in a comment.
// Trigger: action=="created" AND body contains "@caic".
func (s *Server) handleIssueCommentEvent(ctx context.Context, ev *github.IssueCommentEvent) {
	if ev.Action != "created" {
		return
	}
	if !strings.Contains(ev.Comment.Body, "@caic") {
		return
	}
	repo := s.repoByForge(ev.Repository.FullName)
	if repo == nil {
		slog.Warn("webhook: no repo for", "full_name", ev.Repository.FullName)
		return
	}
	s.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	prompt := fmt.Sprintf("A user mentioned @caic in a comment on issue #%d:\n\nIssue: %s\nComment URL: %s\n\n%s",
		ev.Issue.Number,
		ev.Issue.Title,
		ev.Comment.HTMLURL,
		ev.Comment.Body)
	s.createWebhookTask(ctx, repo, prompt, 0, "", 0)
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
	result, done := evaluateCheckRuns(repo.ForgeOwner, repo.ForgeRepo, runs)
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
	s.mu.Lock()
	var waiting []*taskEntry
	for _, entry := range s.tasks {
		if entry.ciSHA == sha {
			waiting = append(waiting, entry)
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
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	owner, repo := parts[0], parts[1]
	for i := range s.repos {
		r := &s.repos[i]
		if strings.EqualFold(r.ForgeOwner, owner) && strings.EqualFold(r.ForgeRepo, repo) {
			return r
		}
	}
	return nil
}

// createWebhookTask creates a task triggered by a webhook event.
// issueNumber > 0 and forgeFullName != "" enables post-completion comment.
func (s *Server) createWebhookTask(_ context.Context, repo *repoInfo, prompt string, installationID int64, forgeFullName string, issueNumber int) {
	runner, ok := s.runners[repo.RelPath]
	if !ok {
		slog.Warn("webhook: runner not found", "repo", repo.RelPath)
		return
	}
	// Pick harness: prefer agent.Claude if available, otherwise take the first one.
	var harness agent.Harness
	if _, ok := runner.Backends[agent.Claude]; ok {
		harness = agent.Claude
	} else {
		for h := range runner.Backends {
			harness = h
			break
		}
	}
	if harness == "" {
		slog.Warn("webhook: no backend available", "repo", repo.RelPath)
		return
	}

	t := &task.Task{
		ID:            ksid.NewID(),
		InitialPrompt: agent.Prompt{Text: prompt},
		Repo:          repo.RelPath,
		Harness:       harness,
		StartedAt:     time.Now().UTC(),
		Provider:      s.provider,
	}
	t.SetTitle(prompt)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}
	// Store webhook callback info for post-completion comment.
	if issueNumber > 0 && forgeFullName != "" {
		entry.webhookInstallationID = installationID
		entry.webhookForgeFullName = forgeFullName
		entry.webhookIssueNumber = issueNumber
	}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	go func() {
		h, err := runner.Start(s.ctx, t)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, runner, h)
	}()

	slog.Info("webhook task created", "id", t.ID, "repo", repo.RelPath, "harness", harness)
}

// postWebhookComment posts a comment on the originating GitHub issue/PR after a webhook-triggered task completes.
func (s *Server) postWebhookComment(entry *taskEntry) {
	if s.githubApp == nil && s.githubToken == "" {
		return
	}
	parts := strings.SplitN(entry.webhookForgeFullName, "/", 2)
	if len(parts) != 2 {
		return
	}
	owner, repo := parts[0], parts[1]
	t := entry.task
	snap := t.Snapshot()
	var body string
	if entry.result != nil && entry.result.AgentResult != "" {
		body = fmt.Sprintf("caic task completed (state: %s)\n\n%s", snap.State, entry.result.AgentResult)
	} else {
		body = fmt.Sprintf("caic task completed (state: %s)", snap.State)
	}
	if s.githubApp != nil && entry.webhookInstallationID != 0 {
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		if err := s.githubApp.PostComment(ctx, entry.webhookInstallationID, owner, repo, entry.webhookIssueNumber, body); err != nil {
			slog.Warn("webhook comment failed", "err", err, "repo", entry.webhookForgeFullName)
		}
		return
	}
	// Fall back to PAT if no app configured.
	if s.githubToken != "" {
		client := &github.Client{Token: s.githubToken}
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		if err := client.PostComment(ctx, owner, repo, entry.webhookIssueNumber, body); err != nil {
			slog.Warn("webhook comment (PAT) failed", "err", err)
		}
	}
}
