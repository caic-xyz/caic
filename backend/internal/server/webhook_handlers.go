// Webhook event handlers for GitHub and GitLab webhook delivery.

package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/forge/gitlab"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

const maxWebhookBodyBytes = 10 << 20 // 10 MB

// WebhookHandlers serves forge webhook delivery routes for GitHub and GitLab.
//
// It verifies delivery authenticity, then dispatches events to the bot, the CI
// service, and the task manager. It owns the forge webhook secrets and the
// GitHub App owner allowlist; the bot and CI service it dispatches to are
// app-owned automation services injected at construction.
type WebhookHandlers struct {
	log              *slog.Logger
	serverCtx        context.Context // dispatch context that outlives individual webhook requests
	githubSecret     []byte          // nil when the GitHub webhook is not configured
	gitlabSecret     []byte          // nil when the GitLab webhook is not configured
	appAllowedOwners []string        // nil = allow all GitHub App installs

	bot        *bot.Bot
	ciService  *ci.Service
	ciCache    *forgecache.Cache
	forgeMgr   *forgemgr.Manager
	taskMgr    *taskmgr.Manager
	checkouts  *repo.Registry
	repoStatus *ci.RepoStatusStore
	prefs      *preferences.Store
}

// HandleGitHub handles POST /webhooks/github.
// It verifies the HMAC signature and dispatches on X-GitHub-Event.
func (h *WebhookHandlers) HandleGitHub(w http.ResponseWriter, r *http.Request) {
	if len(h.githubSecret) == 0 {
		http.Error(w, "webhooks not configured", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20)) // 25 MB limit
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	event := r.Header.Get("X-Github-Event")
	delivery := r.Header.Get("X-Github-Delivery")
	sig := r.Header.Get("X-Hub-Signature-256")
	if err := github.VerifySignature(h.githubSecret, body, sig); err != nil {
		h.log.WarnContext(r.Context(), "github webhook signature mismatch",
			"event", event,
			"delivery", delivery,
			"body_bytes", len(body),
			"signature", signaturePresence(sig),
			"err", err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	h.log.InfoContext(r.Context(), "github webhook", "event", event, "delivery", delivery)
	switch event {
	case "issues":
		var ev github.IssuesEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		h.handleIssuesEvent(r.Context(), &ev)
	case "pull_request":
		var ev github.PullRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		h.handlePullRequestEvent(r.Context(), &ev)
	case "issue_comment":
		var ev github.IssueCommentEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		h.handleIssueCommentEvent(r.Context(), &ev)
	case "installation":
		var ev github.InstallationEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		h.handleInstallationEvent(r.Context(), &ev)
	case "check_suite":
		var ev github.CheckSuiteEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		h.handleCheckSuiteEvent(r.Context(), &ev)
	case "check_run":
		var ev github.CheckRunEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if ev.CheckRun.Status == "completed" {
			owner, repoName, _ := strings.Cut(ev.Repository.FullName, "/")
			if owner != "" && repoName != "" && ev.CheckRun.HeadSHA != "" {
				go h.webhookOnCI(h.serverCtx, forge.KindGitHub, owner, repoName, ev.CheckRun.HeadSHA) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
			}
		}
	case "workflow_run":
		var ev github.WorkflowRunEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if ev.WorkflowRun.Status == "completed" {
			owner, repoName, _ := strings.Cut(ev.Repository.FullName, "/")
			if owner != "" && repoName != "" && ev.WorkflowRun.HeadSHA != "" {
				go h.webhookOnCI(h.serverCtx, forge.KindGitHub, owner, repoName, ev.WorkflowRun.HeadSHA) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
			}
		}
	default:
		// Unknown event — silently ignore, return 200.
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleGitLab verifies the X-Gitlab-Token header and dispatches
// Pipeline Hook and Merge Request Hook events.
func (h *WebhookHandlers) HandleGitLab(w http.ResponseWriter, r *http.Request) {
	if len(h.gitlabSecret) == 0 {
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
	if subtle.ConstantTimeCompare([]byte(token), h.gitlabSecret) != 1 {
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
		owner, repoName, _ := strings.Cut(ev.Project.PathWithNamespace, "/")
		if owner != "" && repoName != "" && sha != "" {
			go h.webhookOnCI(h.serverCtx, forge.KindGitLab, owner, repoName, sha) //nolint:contextcheck // intentionally using server context; webhook dispatch must outlive request
		}
	case "Merge Request Hook":
		var ev gitlab.MergeRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleGitLabMergeRequestEvent(&ev)
	default:
		// Unknown event — silently ignore, return 200.
	}
	w.WriteHeader(http.StatusNoContent)
}

// routes returns the webhook handler with POST /webhooks/github and
// POST /webhooks/gitlab registered.
func (h *WebhookHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /webhooks/github", h.HandleGitHub)
	m.HandleFunc("POST /webhooks/gitlab", h.HandleGitLab)
	// Unmatched webhook paths must not fall through to the SPA.
	m.Handle("/webhooks/", http.NotFoundHandler())
	m.Handle("/webhooks", http.NotFoundHandler())
	return m
}

func signaturePresence(sig string) string {
	if sig == "" {
		return "missing"
	}
	return "present"
}

// webhookOnCI handles a CI completion event from a forge webhook by fetching
// the current check-run state and updating affected tasks and repos.
func (h *WebhookHandlers) webhookOnCI(ctx context.Context, kind forge.Kind, owner, repoName, sha string) {
	f := h.forgeMgr.ForgeFor(ctx, kind)
	if f == nil {
		return
	}

	affected := h.taskMgr.FindTasksMonitoringBranch(owner, repoName)
	affectedRepoPaths := h.repoStatus.PathsAtSHA(repoRefs(h.checkouts.Checkouts()), owner, repoName, sha)

	if len(affected) == 0 && len(affectedRepoPaths) == 0 {
		return
	}

	runs, err := f.GetCheckRuns(ctx, owner, repoName, sha)
	if err != nil {
		h.log.WarnContext(ctx, "get CI check runs", "repo", owner+"/"+repoName, "sha", sha[:min(7, len(sha))], "err", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	result, done := ci.EvaluateCheckRuns(owner, repoName, runs)

	// Pre-compute interim status once for all affected tasks/repos.
	var interimStatus forge.CIStatus
	if !done {
		interimStatus = ci.InterimCIStatus(runs)
	}

	for _, e := range affected {
		if !done {
			e.Task().SetCIStatus(interimStatus, result.Checks)
			h.taskMgr.NotifyTaskChange()
			continue
		}
		if err := h.ciCache.Put(owner, repoName, sha, result); err != nil {
			h.log.WarnContext(ctx, "cache CI status", "err", err)
		}
		h.ciService.ApplyMonitorCIResult(ctx, e, f, owner, repoName, sha, result)
	}

	for _, relPath := range affectedRepoPaths {
		if !done {
			repoStatus := forge.CIStatusPending
			if interimStatus == forge.CIStatusFailure {
				repoStatus = forge.CIStatusFailure
			}
			h.ciService.SetRepoCIStatus(relPath, sha, forgecache.Result{Status: repoStatus, Checks: result.Checks})
			continue
		}
		if err := h.ciCache.Put(owner, repoName, sha, result); err != nil {
			h.log.WarnContext(ctx, "cache CI checks", "err", err)
		}
		h.ciService.SetRepoCIStatus(relPath, sha, forgecache.Result{Status: result.Status, Checks: result.Checks})
	}
}

// handleIssuesEvent creates a task when a labeled issue is opened.
// Trigger: action=="opened" AND label "caic" present.
func (h *WebhookHandlers) handleIssuesEvent(ctx context.Context, ev *github.IssuesEvent) {
	if ev.Action != "opened" {
		return
	}
	h.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	labels := make([]string, len(ev.Issue.Labels))
	for i, l := range ev.Issue.Labels {
		labels[i] = l.Name
	}
	h.bot.OnIssueOpened(ctx, &bot.IssueEvent{
		ForgeFullName: ev.Repository.FullName,
		Number:        ev.Issue.Number,
		Title:         ev.Issue.Title,
		Body:          ev.Issue.Body,
		HTMLURL:       ev.Issue.HTMLURL,
		Labels:        labels,
	}, h.forgeMgr.CommenterFor(ev.Installation.ID))
}

// handlePullRequestEvent creates a task when a PR is opened or reopened,
// updates PR state when closed, or updates an existing task if a PR is
// opened for a branch that already has a runtime instance/task but no PR yet.
// Trigger: action=="opened", "reopened", or "closed".
func (h *WebhookHandlers) handlePullRequestEvent(ctx context.Context, ev *github.PullRequestEvent) {
	// Create a new task to review/fix the PR if auto-fix on PR open is enabled.
	if (ev.Action == "opened" || ev.Action == "reopened") && h.prefs.Get("default").Settings.AutoFixOnPROpen {
		h.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
		h.bot.OnPROpened(ctx, &bot.PREvent{
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
		h.handlePRClosedEvent(ev)
	case "reopened":
		h.handlePRReopenedEvent(ev)
	}

	// Also check if this PR is for an existing task that doesn't have a PR yet.
	// This handles the case where a user creates a PR outside of caic.
	h.handlePRForExistingTask(ctx, ev)
}

// handlePRForExistingTask updates an existing task with PR info if the PR head
// branch matches a task's branch but the task doesn't have a PR yet.
func (h *WebhookHandlers) handlePRForExistingTask(ctx context.Context, ev *github.PullRequestEvent) {
	// Only handle PR open, reopen, or synchronize actions.
	if ev.Action != "opened" && ev.Action != "reopened" && ev.Action != "synchronize" {
		return
	}
	owner, repoName, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repoName == "" {
		return
	}
	branch := ev.PullRequest.Head.Ref
	if branch == "" {
		return
	}
	prNumber := ev.PullRequest.Number
	sha := ev.PullRequest.Head.SHA

	matchingEntries := h.taskMgr.FindTasksMatchingBranch(owner, repoName, branch)
	for _, entry := range matchingEntries {
		snap := entry.Task().Snapshot()
		if snap.ForgePR == 0 {
			// Task has no PR yet — set the PR info.
			h.log.InfoContext(ctx, "associating external PR", "task", entry.Task().ID, "repo", owner+"/"+repoName, "br", branch, "pr", prNumber)
			entry.Task().SetPR(owner, repoName, prNumber)
			// Start CI monitoring.
			if ri, ok := h.repoByForge(owner + "/" + repoName); ok {
				f := h.forgeMgr.ForgeFor(ctx, ri.ForgeKind)
				if f != nil {
					entry.SetMonitorBranch(branch)
					go h.ciService.MonitorCI(ctx, entry, f, owner, repoName, sha)
				}
			}
		} else if snap.ForgePR == prNumber && ev.Action == "synchronize" {
			// PR already exists, but new commits were pushed — restart CI monitoring.
			h.log.InfoContext(ctx, "restarting CI monitor", "task", entry.Task().ID, "repo", owner+"/"+repoName, "br", branch, "pr", prNumber, "sha", sha[:min(7, len(sha))])
			if ri, ok := h.repoByForge(owner + "/" + repoName); ok {
				go h.ciService.MonitorCI(ctx, entry, h.forgeMgr.ForgeFor(ctx, ri.ForgeKind), owner, repoName, sha)
			}
		}
	}
}

// handlePRClosedEvent updates the PR state for tasks whose PR was closed or merged.
func (h *WebhookHandlers) handlePRClosedEvent(ev *github.PullRequestEvent) {
	owner, repoName, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repoName == "" {
		return
	}
	prNumber := ev.PullRequest.Number

	matchingEntries := h.taskMgr.FindTasksByPR(owner, repoName, prNumber)
	for _, entry := range matchingEntries {
		// Determine state: "merged" if merged, otherwise "closed".
		var state forge.PRState
		if ev.PullRequest.Merged {
			state = forge.PRStateMerged
		} else {
			state = forge.PRStateClosed
		}
		h.log.Info("PR closed or merged", "task", entry.Task().ID, "repo", owner+"/"+repoName, "pr", prNumber, "state", state)
		entry.Task().SetPRState(state)
		h.taskMgr.NotifyTaskChange()
	}
}

// handlePRReopenedEvent resets the PR state to "open" when a closed PR is reopened.
func (h *WebhookHandlers) handlePRReopenedEvent(ev *github.PullRequestEvent) {
	owner, repoName, _ := strings.Cut(ev.Repository.FullName, "/")
	if owner == "" || repoName == "" {
		return
	}
	prNumber := ev.PullRequest.Number

	for _, entry := range h.taskMgr.FindTasksByPR(owner, repoName, prNumber) {
		h.log.Info("PR reopened", "task", entry.Task().ID, "repo", owner+"/"+repoName, "pr", prNumber)
		entry.Task().SetPRState(forge.PRStateOpen)
		h.taskMgr.NotifyTaskChange()
	}
}

// handleGitLabMergeRequestEvent handles GitLab merge request state changes.
func (h *WebhookHandlers) handleGitLabMergeRequestEvent(ev *gitlab.MergeRequestEvent) {
	owner, repoName, _ := strings.Cut(ev.Project.PathWithNamespace, "/")
	if owner == "" || repoName == "" {
		return
	}
	mrIID := ev.ObjectAttributes.IID

	for _, entry := range h.taskMgr.FindTasksByPR(owner, repoName, mrIID) {
		state := forge.PRStateOpen
		if ev.ObjectAttributes.State == "closed" {
			if ev.ObjectAttributes.Merged {
				state = forge.PRStateMerged
			} else {
				state = forge.PRStateClosed
			}
		}
		h.log.Info("GitLab merge request state", "task", entry.Task().ID, "repo", owner+"/"+repoName, "mr", mrIID, "state", state)
		entry.Task().SetPRState(state)
		h.taskMgr.NotifyTaskChange()
	}
}

// handleIssueCommentEvent creates a task when @caic is mentioned in a comment.
// Trigger: action=="created" AND body contains "@caic".
func (h *WebhookHandlers) handleIssueCommentEvent(ctx context.Context, ev *github.IssueCommentEvent) {
	if ev.Action != "created" {
		return
	}
	h.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)
	h.bot.OnIssueComment(ctx, bot.CommentEvent{
		ForgeFullName: ev.Repository.FullName,
		IssueNumber:   ev.Issue.Number,
		IssueTitle:    ev.Issue.Title,
		CommentBody:   ev.Comment.Body,
		CommentURL:    ev.Comment.HTMLURL,
	}, h.forgeMgr.CommenterFor(ev.Installation.ID))
}

// handleInstallationEvent enforces the owner allowlist on new installs.
// When GITHUB_APP_ALLOWED_OWNERS is set and the installing account is not in
// the list, the installation is deleted immediately.
func (h *WebhookHandlers) handleInstallationEvent(ctx context.Context, ev *github.InstallationEvent) {
	if ev.Action != "created" {
		return
	}
	login := ev.Installation.Account.Login
	if h.appAllowedOwners == nil {
		h.forgeMgr.StoreInstallationID(login, ev.Installation.ID)
		return
	}
	if slices.Contains(h.appAllowedOwners, strings.ToLower(login)) {
		h.forgeMgr.StoreInstallationID(login, ev.Installation.ID)
		return
	}
	h.log.WarnContext(ctx, "rejecting GitHub App installation from non-allowed owner", "owner", login, "installation_id", ev.Installation.ID)
	if err := h.forgeMgr.GitHubApp().DeleteInstallation(ctx, ev.Installation.ID); err != nil {
		h.log.WarnContext(ctx, "delete GitHub App installation", "owner", login, "err", err)
	}
}

// handleCheckSuiteEvent updates CI status when a check suite completes.
// It caches the result, updates default-branch repo CI status, and delivers
// the terminal result to any task that was monitoring that SHA.
func (h *WebhookHandlers) handleCheckSuiteEvent(ctx context.Context, ev *github.CheckSuiteEvent) {
	if ev.Action != "completed" {
		return
	}
	repoInfo, ok := h.repoByForge(ev.Repository.FullName)
	if !ok {
		return // not a repo we manage
	}
	h.storeInstallationIDFromFullName(ev.Repository.FullName, ev.Installation.ID)

	sha := ev.CheckSuite.HeadSHA
	client, err := h.forgeMgr.GitHubApp().ForgeClient(ctx, ev.Installation.ID)
	if err != nil {
		h.log.WarnContext(ctx, "resolve forge client for check suite", "err", err)
		return
	}

	runs, err := client.GetCheckRuns(ctx, repoInfo.ForgeOwner, repoInfo.ForgeRepo, sha)
	if err != nil {
		h.log.WarnContext(ctx, "get check suite runs", "sha", sha, "err", err)
		return
	}
	result, done := ci.EvaluateCheckRuns(repoInfo.ForgeOwner, repoInfo.ForgeRepo, runs)
	if !done {
		return
	}
	if err := h.ciCache.Put(repoInfo.ForgeOwner, repoInfo.ForgeRepo, sha, result); err != nil {
		h.log.WarnContext(ctx, "cache check suite", "err", err)
	}

	for checkout := range h.checkouts.Checkouts() {
		if checkout.Repository != repoInfo {
			continue
		}
		if ev.CheckSuite.HeadBranch != checkout.BaseBranch && checkout.BaseBranch != "" {
			continue
		}
		headSHA, err := client.GetDefaultBranchSHA(ctx, repoInfo.ForgeOwner, repoInfo.ForgeRepo, checkout.BaseBranch)
		switch {
		case err != nil:
			h.log.WarnContext(ctx, "get checkout HEAD SHA", "repo", checkout.RelPath, "err", err)
		case headSHA == sha:
			h.ciService.SetRepoCIStatus(checkout.RelPath, sha, forgecache.Result{Status: result.Status, Checks: result.Checks})
		default:
			h.log.DebugContext(ctx, "ignoring stale check suite", "sha", sha, "head", headSHA)
		}
	}

	// Deliver the result to any task monitoring this SHA.
	// Match by branch since multiple commits can have the same SHA across different branches.
	for _, entry := range h.taskMgr.FindTasksMonitoringBranch(repoInfo.ForgeOwner, repoInfo.ForgeRepo) {
		go h.ciService.ApplyMonitorCIResult(h.serverCtx, entry, client, repoInfo.ForgeOwner, repoInfo.ForgeRepo, sha, result) //nolint:contextcheck // fire-and-forget; must outlive webhook request
	}
}

// storeInstallationIDFromFullName extracts the owner from "owner/repo" and
// stores the installation ID for that owner.
func (h *WebhookHandlers) storeInstallationIDFromFullName(fullName string, id int64) {
	owner, _, ok := strings.Cut(fullName, "/")
	if ok {
		h.forgeMgr.StoreInstallationID(owner, id)
	}
}

// repoByForge returns the known repository whose forge name matches "owner/repo".
func (h *WebhookHandlers) repoByForge(fullName string) (*repo.Repository, bool) {
	owner, repoName, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil, false
	}
	return h.checkouts.RepositoryByForge(owner, repoName)
}

func repoRefs(snap iter.Seq[*repo.Checkout]) []ci.RepoRef {
	var refs []ci.RepoRef
	for checkout := range snap {
		if checkout.Repository == nil {
			continue
		}
		refs = append(refs, ci.RepoRef{RelPath: checkout.RelPath, ForgeOwner: checkout.Repository.ForgeOwner, ForgeRepo: checkout.Repository.ForgeRepo})
	}
	return refs
}
