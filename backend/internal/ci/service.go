// CI service: orchestrates CI monitoring, auto-fix loops, and PR creation for forge-connected repos.

package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// Service orchestrates CI monitoring, auto-fix loops, and PR creation for
// forge-connected repositories.
type Service struct {
	cache    *forgecache.Cache
	provider genai.Provider
	backend  Backend
}

// NewService creates a CI service.
// All arguments must be non-nil.
func NewService(cache *forgecache.Cache, provider genai.Provider, backend Backend) *Service {
	return &Service{
		cache:    cache,
		provider: provider,
		backend:  backend,
	}
}

// StartPRFlow creates a PR/MR for the synced branch, records it on the task,
// and launches CI monitoring in a goroutine. Returns the PR number on success.
func (svc *Service) StartPRFlow(ctx context.Context, entry TaskEntry, f forge.Forge, info *RepoInfo, branch, baseBranch string) (int, error) {
	t := entry.Task()
	title := t.Title()
	if title == "" {
		title = t.InitialPrompt.Text
	}
	var body string
	if entry.Result() != nil {
		body = entry.Result().AgentResult
	}
	pr, err := f.CreatePR(ctx, info.ForgeOwner, info.ForgeRepo, branch, baseBranch, title, body)
	if err != nil {
		return 0, err
	}
	slog.Info("PR created", "task", t.ID, "forge", f.Name(), "owner", info.ForgeOwner, "repo", info.ForgeRepo, "pr", pr.Number)
	t.SetPR(info.ForgeOwner, info.ForgeRepo, pr.Number)
	if err := t.WriteToLog(&agent.MetaPRMessage{
		MessageType: "caic_pr",
		ForgeOwner:  info.ForgeOwner,
		ForgeRepo:   info.ForgeRepo,
		ForgePR:     pr.Number,
	}); err != nil {
		return pr.Number, fmt.Errorf("write PR metadata: %w", err)
	}
	svc.backend.SetTaskMonitorBranch(entry, branch)
	svc.backend.NotifyTaskChange()
	go svc.MonitorCI(context.WithoutCancel(ctx), entry, f, info.ForgeOwner, info.ForgeRepo, pr.HeadSHA)
	return pr.Number, nil
}

// MonitorCI watches CI check-runs for a task's PR head SHA until all checks
// complete, then injects a summary into the agent via SendInput.
//
// With a GitHub App configured, it performs a single initial check and returns;
// subsequent updates are delivered via check_suite webhook events.
// Without an App, it polls every 15 s.
func (svc *Service) MonitorCI(ctx context.Context, entry TaskEntry, f forge.Forge, owner, repo, sha string) {
	t := entry.Task()
	slog.Info("monitorCI: start", "task", t.ID, "owner", owner, "repo", repo, "sha", sha, "hasApp", svc.backend.GitHubApp() != nil)

	// Fast path: result already cached (e.g. after a server restart).
	if cached, ok := svc.cache.Get(owner, repo, sha); ok {
		slog.Info("monitorCI: cache hit", "task", t.ID, "status", cached.Status)
		svc.ApplyMonitorCIResult(ctx, entry, f, owner, repo, sha, cached)
		return
	}

	// With GitHub App: do one initial check to seed pending state, then rely on
	// check_suite webhook events for the terminal result.
	if svc.backend.GitHubApp() != nil {
		runs, err := f.GetCheckRuns(ctx, owner, repo, sha)
		if err != nil {
			if !errors.Is(err, forge.ErrNotFound) {
				slog.Warn("monitorCI: initial check-runs", "task", t.ID, "err", err)
			} else {
				slog.Info("monitorCI: check-runs not found (404)", "task", t.ID)
			}
			return // webhook will handle completion
		}
		slog.Info("monitorCI: initial check-runs", "task", t.ID, "runs", len(runs))
		if len(runs) > 0 {
			result, done := bot.EvaluateCheckRuns(owner, repo, runs)
			if done {
				if err := svc.cache.Put(owner, repo, sha, result); err != nil {
					slog.Warn("monitorCI: cache put", "err", err)
				}
				slog.Info("monitorCI: done (app path)", "task", t.ID, "status", result.Status)
				svc.ApplyMonitorCIResult(ctx, entry, f, owner, repo, sha, result)
				return
			}
			status := bot.InterimCIStatus(runs)
			slog.Info("monitorCI: interim status (app path)", "task", t.ID, "status", status, "checks", len(result.Checks))
			t.SetCIStatus(status, result.Checks)
			svc.backend.NotifyTaskChange()
		}
		return // check_suite webhook delivers the terminal result
	}

	// Without App: immediate check then poll every 15 s.
	//
	// checkOnce fetches and applies CI status. It returns true when
	// monitoring should stop (terminal result or permanent error).
	checkOnce := func() (stop bool) {
		runs, err := f.GetCheckRuns(ctx, owner, repo, sha)
		if err != nil {
			if errors.Is(err, forge.ErrNotFound) {
				return true
			}
			slog.Warn("monitorCI: get check-runs", "task", t.ID, "err", err)
			return false
		}
		if len(runs) == 0 {
			return false
		}
		result, done := bot.EvaluateCheckRuns(owner, repo, runs)
		if !done {
			status := bot.InterimCIStatus(runs)
			t.SetCIStatus(status, result.Checks)
			svc.backend.NotifyTaskChange()
			return false
		}
		if err := svc.cache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("monitorCI: cache put", "err", err)
		}
		svc.ApplyMonitorCIResult(ctx, entry, f, owner, repo, sha, result)
		return true
	}

	// Always run one immediate check (e.g. after server restart) so CI
	// status shows up in the task card even for stopped/running tasks.
	if checkOnce() {
		return
	}

	// Continue polling only while the task is waiting for CI.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st := t.GetState()
		if st != task.StateWaiting && st != task.StateAsking && st != task.StateHasPlan {
			return
		}
		if checkOnce() {
			return
		}
	}
}

// ApplyMonitorCIResult updates the task CI status, injects the CI summary
// into the agent, and drives the seamless PR lifecycle:
//   - CI failure: notify agent, then launch autoResync to push fixes and
//     re-monitor so the loop repeats automatically.
//   - CI success: squash-merge the PR via the forge API, then notify the agent.
func (svc *Service) ApplyMonitorCIResult(ctx context.Context, entry TaskEntry, f forge.Forge, owner, repo, sha string, result forgecache.Result) {
	t := entry.Task()

	// Dedup: skip if we already notified this task for this SHA.
	if svc.cache.IsNotified(t.ID.String(), sha) {
		slog.Info("applyMonitorCIResult: already notified, skipping", "task", t.ID, "sha", sha[:min(7, len(sha))])
		t.SetCIStatus(result.Status, result.Checks)
		svc.backend.NotifyTaskChange()
		return
	}

	ciStatus := forge.CIStatusSuccess
	var summary string
	if result.Status == forge.CIStatusFailure {
		ciStatus = forge.CIStatusFailure
		summary = bot.FailureSummary(ctx, f, svc.provider, result)
	} else {
		// CI passed — attempt a squash merge.
		snap := t.Snapshot()
		if snap.ForgePR > 0 {
			commitTitle := t.Title()
			if commitTitle == "" {
				if p := t.Primary(); p != nil {
					commitTitle = p.Branch
				}
			}
			commitMsg := lastResultText(t)
			if mergeErr := f.MergePR(ctx, owner, repo, snap.ForgePR, commitTitle, commitMsg); mergeErr != nil {
				slog.Warn("applyMonitorCIResult: merge PR", "task", t.ID, "pr", snap.ForgePR, "err", mergeErr)
				summary = fmt.Sprintf("%s CI: all checks passed. Auto-merge of %s failed: %v", f.Name(), f.PRLabel(snap.ForgePR), mergeErr)
			} else {
				slog.Info("PR merged", "task", t.ID, "forge", f.Name(), "pr", snap.ForgePR)
				summary = fmt.Sprintf("%s CI: all checks passed. %s merged successfully via squash commit.", f.Name(), f.PRLabel(snap.ForgePR))
			}
		} else {
			summary = fmt.Sprintf("%s CI: all checks passed for %s/%s@%s.", f.Name(), owner, repo, sha[:min(7, len(sha))])
		}
	}
	t.SetCIStatus(ciStatus, result.Checks)
	svc.backend.NotifyTaskChange()
	if err := t.SendInput(ctx, agent.Prompt{Text: summary}); err != nil {
		slog.Warn("monitorCI: send input", "task", t.ID, "err", err)
		// No active session — attempt auto-fix for CI failures if enabled.
		if result.Status == forge.CIStatusFailure {
			snap := t.Snapshot()
			if snap.ForgePR > 0 {
				svc.maybeAutoFix(ctx, t, f, summary)
			}
		}
	}
	if err := svc.cache.MarkNotified(t.ID.String(), sha); err != nil {
		slog.Warn("applyMonitorCIResult: mark notified", "task", t.ID, "err", err)
	}
	// On CI failure: wait for the agent to finish its fix turn, then
	// auto-sync the branch and restart CI monitoring.
	if ciStatus == forge.CIStatusFailure {
		go svc.autoResync(ctx, entry, f, owner, repo)
	}
}

// PollCIForActiveRepos checks the default branch CI status for all repos that
// have active (non-terminal) tasks. ctx must carry the user's auth token (via
// context.WithoutCancel so it is not cancelled when the SSE request ends).
// The outer timeout scales with repo count: 2 API calls per repo at 1 req/s
// (via the throttled HTTP client) plus headroom for retry backoff.
func (svc *Service) PollCIForActiveRepos(ctx context.Context) {
	active := svc.backend.ListActiveRepos()

	// 5 s per API call gives room for the 1 QPS throttle plus Retry backoff.
	total := time.Duration(5*len(active)+60) * time.Second
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	for _, info := range active {
		f := svc.backend.ForgeForInfo(ctx, &info)
		if f == nil {
			continue
		}
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		svc.pollRepoCIOnce(rctx, info, f)
		rcancel()
	}
}

// SetRepoCIStatus updates the in-memory CI state for a repo and notifies
// SSE subscribers if the status changed.
func (svc *Service) SetRepoCIStatus(relPath, sha string, result forgecache.Result) {
	if svc.backend.SetRepoCIStatusIfChanged(relPath, sha, result) {
		svc.backend.NotifyTaskChange()
	}
}

// waitForAgentResult subscribes to task messages and blocks until the agent
// emits a ResultMessage (end of turn) or ctx is cancelled. Returns true when
// a ResultMessage arrives, false on cancellation or closed channel.
func (svc *Service) waitForAgentResult(ctx context.Context, t *task.Task) bool {
	_, live, unsub := t.Subscribe(ctx)
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return false
		case msg, ok := <-live:
			if !ok {
				return false
			}
			if _, isResult := msg.(*agent.ResultMessage); isResult {
				return true
			}
		}
	}
}

// autoResync waits for the agent to finish its current turn, then pushes the
// latest branch commits to origin and starts a new CI monitoring goroutine.
// Called after a CI failure so the loop closes: CI fails → agent fixes →
// auto-push → CI re-runs → (repeat or merge on success).
func (svc *Service) autoResync(ctx context.Context, entry TaskEntry, f forge.Forge, owner, repo string) {
	t := entry.Task()
	if !svc.waitForAgentResult(ctx, t) {
		return
	}

	// Only proceed if the task is still waiting for input (agent finished cleanly).
	st := t.GetState()
	if st != task.StateWaiting && st != task.StateAsking {
		return
	}

	p := t.Primary()
	if p == nil {
		slog.Warn("autoResync: no primary repo", "task", t.ID)
		return
	}
	runner, ok := svc.backend.GetRunner(p.Name)
	if !ok {
		slog.Warn("autoResync: no runner", "task", t.ID)
		return
	}

	slog.Info("autoResync: syncing branch", "task", t.ID, "br", p.Branch)
	if _, _, err := runner.SyncToOrigin(ctx, t.RuntimeRepos(), t.ContainerName(), false); err != nil {
		slog.Warn("autoResync: sync failed", "task", t.ID, "err", err)
		return
	}

	// Fetch the new branch HEAD SHA from the forge after the push.
	newSHA, err := f.GetDefaultBranchSHA(ctx, owner, repo, p.Branch)
	if err != nil {
		slog.Warn("autoResync: get SHA", "task", t.ID, "err", err)
		return
	}

	slog.Info("autoResync: restarting CI monitor", "task", t.ID, "sha", newSHA[:min(7, len(newSHA))])
	svc.backend.NotifyTaskChange()
	go svc.MonitorCI(ctx, entry, f, owner, repo, newSHA)
}

// maybeAutoFix creates a new task to fix CI failures when auto-fix is enabled
// in the task owner's preferences. It is called when the original task's agent
// session is no longer active and cannot receive CI failure input directly.
func (svc *Service) maybeAutoFix(ctx context.Context, t *task.Task, f forge.Forge, ciSummary string) {
	ownerID := t.OwnerID
	if ownerID == "" {
		ownerID = "default"
	}
	if !svc.backend.Prefs().Get(ownerID).Settings.AutoFixOnCIFailure {
		return
	}
	primary := t.Primary()
	if primary == nil {
		slog.Warn("maybeAutoFix: task has no primary repo")
		return
	}
	repo := svc.backend.RepoInfoFor(primary.Name)
	if repo.RelPath == "" {
		slog.Warn("maybeAutoFix: repo not found", "repo", primary.Name)
		return
	}
	snap := t.Snapshot()
	prURL := f.PRURL(snap.ForgeOwner, snap.ForgeRepo, snap.ForgePR)
	prompt := fmt.Sprintf("CI failed on PR #%d", snap.ForgePR)
	if prURL != "" {
		prompt += fmt.Sprintf(" (%s)", prURL)
	}
	prompt += fmt.Sprintf(". Please fix the failing CI checks on branch %q and push the fix:\n\n%s", primary.Branch, ciSummary)
	slog.Info("auto-fix: creating task", "repo", primary.Name, "pr", snap.ForgePR, "branch", primary.Branch)
	if _, err := svc.backend.CreateTask(ctx, bot.TaskRequest{Repo: repo.RelPath, Prompt: prompt, OwnerID: t.OwnerID}); err != nil {
		slog.Warn("maybeAutoFix: create task", "repo", primary.Name, "err", err)
	}
}

// pollRepoCIOnce fetches the default branch CI status for a single repo.
// Returns immediately; safe to call from any goroutine with a user context.
func (svc *Service) pollRepoCIOnce(ctx context.Context, info RepoInfo, f forge.Forge) { //nolint:gocritic // RepoInfo passed by value intentionally
	sha, err := f.GetDefaultBranchSHA(ctx, info.ForgeOwner, info.ForgeRepo, info.BaseBranch)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get SHA", "repo", info.RelPath, "err", err)
			svc.backend.EmitWarning(fmt.Sprintf("CI poll failed for %s: %v", info.RelPath, err))
		}
		return
	}
	// Cache hit: use stored terminal result directly.
	if cached, ok := svc.cache.Get(info.ForgeOwner, info.ForgeRepo, sha); ok {
		svc.SetRepoCIStatus(info.RelPath, sha, cached)
		return
	}
	// Fetch check-runs for the new SHA.
	runs, err := f.GetCheckRuns(ctx, info.ForgeOwner, info.ForgeRepo, sha)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get check-runs", "repo", info.RelPath, "err", err)
			svc.backend.EmitWarning(fmt.Sprintf("CI poll failed for %s: %v", info.RelPath, err))
		}
		return
	}
	if len(runs) == 0 {
		return
	}
	result, done := bot.EvaluateCheckRuns(info.ForgeOwner, info.ForgeRepo, runs)
	if !done {
		// Still in progress — show failure early if any check already failed.
		interimStatus := bot.InterimCIStatus(runs)
		repoStatus := forge.CIStatusPending
		if interimStatus == forge.CIStatusFailure {
			repoStatus = forge.CIStatusFailure
		}
		svc.SetRepoCIStatus(info.RelPath, sha, forgecache.Result{Status: repoStatus, Checks: result.Checks})
		return
	}
	if err := svc.cache.Put(info.ForgeOwner, info.ForgeRepo, sha, result); err != nil {
		slog.Warn("pollRepoCIOnce: cache put", "repo", info.RelPath, "err", err)
	}
	svc.SetRepoCIStatus(info.RelPath, sha, result)
}
