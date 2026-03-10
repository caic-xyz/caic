// Webhook event handlers for GitHub webhook delivery.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/github"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

// handleGitHubWebhook handles POST /api/v1/github/webhook.
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
	default:
		// Unknown event — silently ignore, return 200.
	}
	w.WriteHeader(http.StatusNoContent)
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
	prompt := fmt.Sprintf("Fix the following GitHub issue:\n\nTitle: %s\nURL: %s\n\n%s",
		ev.Issue.Title, ev.Issue.HTMLURL, ev.Issue.Body)
	installationID := int64(0) // not used for comment if app not configured
	s.createWebhookTask(ctx, repo, prompt, installationID, ev.Repository.FullName, ev.Issue.Number)
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
	prompt := fmt.Sprintf("A user mentioned @caic in a comment on issue #%d:\n\nIssue: %s\nComment URL: %s\n\n%s",
		ev.Issue.Number,
		ev.Issue.Title,
		ev.Comment.HTMLURL,
		ev.Comment.Body)
	s.createWebhookTask(ctx, repo, prompt, 0, "", 0)
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
