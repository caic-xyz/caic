// CI monitoring: polls forge check-runs, drives auto-resync and auto-fix loops.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

// repoCIState holds the live default-branch CI status for one repo.
type repoCIState struct {
	Status  forge.CIStatus
	Checks  []v1.ForgeCheck
	HeadSHA string // default branch HEAD SHA when last updated; used for webhook dispatch
}

// monitorCI watches CI check-runs for a task's PR head SHA until all checks
// complete, then injects a summary into the agent via SendInput.
//
// With a GitHub App configured, it performs a single initial check and returns;
// subsequent updates are delivered via check_suite webhook events.
// Without an App, it polls every 15 s.
func (s *Server) monitorCI(ctx context.Context, entry *taskEntry, f forge.Forge, owner, repo, sha string) {
	t := entry.task
	slog.Info("monitorCI: start", "task", t.ID, "owner", owner, "repo", repo, "sha", sha, "hasApp", s.githubApp != nil)

	// Fast path: result already cached (e.g. after a server restart).
	if cached, ok := s.ciCache.Get(owner, repo, sha); ok {
		slog.Info("monitorCI: cache hit", "task", t.ID, "status", cached.Status)
		s.applyMonitorCIResult(ctx, entry, f, owner, repo, sha, cached)
		return
	}

	// With GitHub App: do one initial check to seed pending state, then rely on
	// check_suite webhook events for the terminal result.
	if s.githubApp != nil {
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
				if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
					slog.Warn("monitorCI: cache put", "err", err)
				}
				slog.Info("monitorCI: done (app path)", "task", t.ID, "status", result.Status)
				s.applyMonitorCIResult(ctx, entry, f, owner, repo, sha, result)
				return
			}
			status := bot.InterimCIStatus(runs)
			slog.Info("monitorCI: interim status (app path)", "task", t.ID, "status", status, "checks", len(result.Checks))
			t.SetCIStatus(status, result.Checks)
			s.notifyTaskChange()
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
			s.notifyTaskChange()
			return false
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("monitorCI: cache put", "err", err)
		}
		s.applyMonitorCIResult(ctx, entry, f, owner, repo, sha, result)
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

// waitForAgentResult subscribes to task messages and blocks until the agent
// emits a ResultMessage (end of turn) or ctx is cancelled. Returns true when
// a ResultMessage arrives, false on cancellation or closed channel.
func (s *Server) waitForAgentResult(ctx context.Context, t *task.Task) bool {
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
func (s *Server) autoResync(ctx context.Context, entry *taskEntry, f forge.Forge, owner, repo string) {
	t := entry.task
	if !s.waitForAgentResult(ctx, t) {
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
	runner, ok := s.runners[p.Name]
	if !ok {
		slog.Warn("autoResync: no runner", "task", t.ID)
		return
	}

	slog.Info("autoResync: syncing branch", "task", t.ID, "br", p.Branch)
	if _, _, err := runner.SyncToOrigin(ctx, p.Branch, t.Container, false, t.ExtraMDRepos()); err != nil {
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
	s.notifyTaskChange()
	go s.monitorCI(ctx, entry, f, owner, repo, newSHA)
}

// applyMonitorCIResult updates the task CI status, injects the CI summary
// into the agent, and drives the seamless PR lifecycle:
//   - CI failure: notify agent, then launch autoResync to push fixes and
//     re-monitor so the loop repeats automatically.
//   - CI success: squash-merge the PR via the forge API, then notify the agent.
func (s *Server) applyMonitorCIResult(ctx context.Context, entry *taskEntry, f forge.Forge, owner, repo, sha string, result forgecache.Result) {
	t := entry.task

	// Dedup: skip if we already notified this task for this SHA.
	if s.ciCache.IsNotified(t.ID.String(), sha) {
		slog.Info("applyMonitorCIResult: already notified, skipping", "task", t.ID, "sha", sha[:min(7, len(sha))])
		t.SetCIStatus(result.Status, result.Checks)
		s.notifyTaskChange()
		return
	}

	ciStatus := forge.CIStatusSuccess
	var summary string
	if result.Status == forge.CIStatusFailure {
		ciStatus = forge.CIStatusFailure
		summary = bot.FailureSummary(ctx, f, s.provider, result)
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
	s.notifyTaskChange()
	if err := t.SendInput(ctx, agent.Prompt{Text: summary}); err != nil {
		slog.Warn("monitorCI: send input", "task", t.ID, "err", err)
		// No active session — attempt auto-fix for CI failures if enabled.
		if result.Status == forge.CIStatusFailure {
			snap := t.Snapshot()
			if snap.ForgePR > 0 {
				s.maybeAutoFix(t, f, summary)
			}
		}
	}
	if err := s.ciCache.MarkNotified(t.ID.String(), sha); err != nil {
		slog.Warn("applyMonitorCIResult: mark notified", "task", t.ID, "err", err)
	}
	// On CI failure: wait for the agent to finish its fix turn, then
	// auto-sync the branch and restart CI monitoring.
	if ciStatus == forge.CIStatusFailure {
		go s.autoResync(ctx, entry, f, owner, repo)
	}
}

// lastResultText returns the Result field of the most recent ResultMessage in
// the task's message history. Used as the squash-merge commit body.
func lastResultText(t *task.Task) string {
	msgs := t.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if rm, ok := msgs[i].(*agent.ResultMessage); ok {
			return rm.Result
		}
	}
	return ""
}

// maybeAutoFix creates a new task to fix CI failures when auto-fix is enabled
// in the task owner's preferences. It is called when the original task's agent
// session is no longer active and cannot receive CI failure input directly.
func (s *Server) maybeAutoFix(t *task.Task, f forge.Forge, ciSummary string) {
	ownerID := t.OwnerID
	if ownerID == "" {
		ownerID = "default"
	}
	if !s.prefs.Get(ownerID).Settings.AutoFixOnCIFailure {
		return
	}
	primary := t.Primary()
	if primary == nil {
		slog.Warn("maybeAutoFix: task has no primary repo")
		return
	}
	repo := s.repoInfoFor(primary.Name)
	if repo == nil {
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
	if _, err := s.CreateTask(s.ctx, bot.TaskRequest{Repo: repo.RelPath, Prompt: prompt, OwnerID: t.OwnerID}); err != nil {
		slog.Warn("maybeAutoFix: create task", "repo", primary.Name, "err", err)
	}
}

// handleGetCILog fetches the log for a specific CI job by jobID.
// The jobID is a required query parameter; the caller knows it from the
// task's ciChecks field. The log is capped at ~8 KB (tail).
func (s *Server) handleGetCILog(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.task
	snap := t.Snapshot()
	ciPrimaryName := ""
	if p := t.Primary(); p != nil {
		ciPrimaryName = p.Name
	}
	info := s.repoInfoFor(ciPrimaryName)
	if info == nil {
		writeError(w, dto.BadRequest("no repo info found"))
		return
	}
	f := s.forgeForInfo(r.Context(), info)
	if f == nil {
		writeError(w, dto.BadRequest("no forge token configured for this repo"))
		return
	}

	jobIDStr := r.URL.Query().Get("jobID")
	if jobIDStr == "" {
		writeError(w, dto.BadRequest("jobID query parameter is required"))
		return
	}
	var jobID int64
	if _, scanErr := fmt.Sscanf(jobIDStr, "%d", &jobID); scanErr != nil || jobID <= 0 {
		writeError(w, dto.BadRequest("invalid jobID"))
		return
	}

	// Find the check by jobID to get owner/repo/name.
	var check *forge.Check
	for i := range snap.CIChecks {
		if snap.CIChecks[i].JobID == jobID {
			check = &snap.CIChecks[i]
			break
		}
	}
	if check == nil {
		writeError(w, dto.NotFound("no CI check with that jobID"))
		return
	}

	jobLog, logErr := f.GetJobLog(r.Context(), check.Owner, check.Repo, jobID, false)
	if logErr != nil {
		slog.Warn("getTaskCILog: fetch job log", "task", t.ID, "jobID", jobID, "err", logErr)
		jobLog = "(log unavailable: " + logErr.Error() + ")"
	}

	if r.URL.Query().Get("raw") == "true" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "Step: %s\n\n%s", check.Name, jobLog)
		return
	}
	writeJSONResponse(w, &v1.CILogResp{StepName: check.Name, Log: jobLog}, nil)
}

// botFixCI creates a task to fix failing CI on a repo's default branch.
// It fetches CI logs via the forge, builds a rich prompt using bot.FailureSummary,
// and creates a new agent task — the same path as the automated maybeAutoFix.
func (s *Server) botFixCI(ctx context.Context, req *v1.BotFixCIReq) (*v1.CreateTaskResp, error) {
	info := s.repoInfoFor(req.Repo)
	if info == nil {
		return nil, dto.BadRequest("repo not found")
	}
	f := s.forgeForInfo(ctx, info)
	if f == nil {
		return nil, dto.BadRequest("no forge token configured for this repo")
	}

	s.mu.Lock()
	state := s.repoCIStatus[req.Repo]
	s.mu.Unlock()

	if state.Status != forge.CIStatusFailure {
		return nil, dto.BadRequest("no CI failure on default branch")
	}

	// Convert stored DTO checks back to forge.Check for bot.FailureSummary.
	checks := make([]forge.Check, len(state.Checks))
	for i := range state.Checks {
		c := &state.Checks[i]
		checks[i] = forge.Check{
			Name:        c.Name,
			Owner:       c.Owner,
			Repo:        c.Repo,
			RunID:       c.RunID,
			JobID:       c.JobID,
			Status:      forge.CheckRunStatus(c.Status),
			Conclusion:  forge.CheckRunConclusion(c.Conclusion),
			QueuedAt:    c.QueuedAt,
			StartedAt:   c.StartedAt,
			CompletedAt: c.CompletedAt,
		}
	}
	result := forgecache.Result{Status: forge.CIStatusFailure, Checks: checks}
	summary := bot.FailureSummary(ctx, f, s.provider, result)

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}
	taskIDStr, err := s.CreateTask(ctx, bot.TaskRequest{Repo: info.RelPath, Prompt: summary, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	taskID, err := ksid.Parse(taskIDStr)
	if err != nil {
		return nil, fmt.Errorf("parse task id: %w", err)
	}
	return &v1.CreateTaskResp{ID: taskID}, nil
}

// botFixPR injects a fix-PR command into an existing task's agent session.
// It fetches CI logs via the forge using the task's existing CI checks,
// builds a rich prompt using bot.FailureSummary, and sends it as input to the task.
func (s *Server) botFixPR(ctx context.Context, req *v1.BotFixPRReq) (*v1.StatusResp, error) {
	s.mu.Lock()
	entry, ok := s.tasks[req.TaskID]
	s.mu.Unlock()
	if !ok {
		return nil, dto.NotFound("task")
	}
	t := entry.task
	snap := t.Snapshot()
	if snap.ForgePR == 0 {
		return nil, dto.BadRequest("task has no associated PR")
	}
	primary := t.Primary()
	if primary == nil {
		return nil, dto.BadRequest("task has no primary repo")
	}
	info := s.repoInfoFor(primary.Name)
	if info == nil {
		return nil, dto.BadRequest("repo not found")
	}
	f := s.forgeForInfo(ctx, info)
	if f == nil {
		return nil, dto.BadRequest("no forge token configured for this repo")
	}

	checks := snap.CIChecks
	if len(checks) == 0 {
		sha, shaErr := f.GetDefaultBranchSHA(ctx, snap.ForgeOwner, snap.ForgeRepo, primary.Branch)
		if shaErr != nil {
			return nil, fmt.Errorf("get branch SHA: %w", shaErr)
		}
		runs, runsErr := f.GetCheckRuns(ctx, snap.ForgeOwner, snap.ForgeRepo, sha)
		if runsErr != nil {
			return nil, fmt.Errorf("get check runs: %w", runsErr)
		}
		result, _ := bot.EvaluateCheckRuns(snap.ForgeOwner, snap.ForgeRepo, runs)
		checks = result.Checks
	}
	result := forgecache.Result{Status: forge.CIStatusFailure, Checks: checks}
	summary := bot.FailureSummary(ctx, f, s.provider, result)

	prURL := f.PRURL(snap.ForgeOwner, snap.ForgeRepo, snap.ForgePR)
	prompt := fmt.Sprintf("CI failed on PR #%d", snap.ForgePR)
	if prURL != "" {
		prompt += fmt.Sprintf(" (%s)", prURL)
	}
	prompt += fmt.Sprintf(". Please fix the failing CI checks on branch %q and push the fix:\n\n%s", primary.Branch, summary)

	if err := t.SendInput(ctx, agent.Prompt{Text: prompt}); err != nil {
		return nil, fmt.Errorf("send input: %w", err)
	}
	return &v1.StatusResp{Status: "ok"}, nil
}

// pollRepoCIOnce fetches the default branch CI status for a single repo.
// Returns immediately; safe to call from any goroutine with a user context.
func (s *Server) pollRepoCIOnce(ctx context.Context, info repoInfo, f forge.Forge) { //nolint:gocritic // repoInfo passed by value intentionally
	sha, err := f.GetDefaultBranchSHA(ctx, info.ForgeOwner, info.ForgeRepo, info.BaseBranch)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get SHA", "repo", info.RelPath, "err", err)
		}
		return
	}
	// Cache hit: use stored terminal result directly.
	if cached, ok := s.ciCache.Get(info.ForgeOwner, info.ForgeRepo, sha); ok {
		s.setRepoCIStatus(info.RelPath, sha, cached)
		return
	}
	// Fetch check-runs for the new SHA.
	runs, err := f.GetCheckRuns(ctx, info.ForgeOwner, info.ForgeRepo, sha)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get check-runs", "repo", info.RelPath, "err", err)
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
		s.setRepoCIStatus(info.RelPath, sha, forgecache.Result{Status: repoStatus, Checks: result.Checks})
		return
	}
	if err := s.ciCache.Put(info.ForgeOwner, info.ForgeRepo, sha, result); err != nil {
		slog.Warn("pollRepoCIOnce: cache put", "repo", info.RelPath, "err", err)
	}
	s.setRepoCIStatus(info.RelPath, sha, result)
}

// pollCIForActiveRepos checks the default branch CI status for all repos that
// have active (non-terminal) tasks. ctx must carry the user's auth token (via
// context.WithoutCancel so it is not cancelled when the SSE request ends).
// The outer timeout scales with repo count: 2 API calls per repo at 1 req/s
// (via the throttled HTTP client) plus a 30-second buffer.
func (s *Server) pollCIForActiveRepos(ctx context.Context) {
	s.mu.Lock()
	var activeIdx []int
	for i := range s.repos {
		if s.repos[i].ForgeOwner != "" && s.repoHasActiveTasksLocked(s.repos[i].RelPath) {
			activeIdx = append(activeIdx, i)
		}
	}
	s.mu.Unlock()

	total := time.Duration(2*len(activeIdx)+30) * time.Second
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	for _, i := range activeIdx {
		f := s.forgeForInfo(ctx, &s.repos[i])
		if f == nil {
			continue
		}
		rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
		s.pollRepoCIOnce(rctx, s.repos[i], f)
		rcancel()
	}
}

// checkToDTO converts a forge.Check to a v1.ForgeCheck for API responses.
func checkToDTO(c *forge.Check) v1.ForgeCheck {
	return v1.ForgeCheck{
		Name:        c.Name,
		Owner:       c.Owner,
		Repo:        c.Repo,
		RunID:       c.RunID,
		JobID:       c.JobID,
		Status:      v1.CheckStatus(c.Status),
		Conclusion:  v1.CheckConclusion(c.Conclusion),
		QueuedAt:    c.QueuedAt,
		StartedAt:   c.StartedAt,
		CompletedAt: c.CompletedAt,
	}
}

// setRepoCIStatus updates the in-memory CI state for a repo and notifies
// SSE subscribers if the status changed.
func (s *Server) setRepoCIStatus(relPath, sha string, result forgecache.Result) {
	dtoChecks := make([]v1.ForgeCheck, len(result.Checks))
	for i := range result.Checks {
		dtoChecks[i] = checkToDTO(&result.Checks[i])
	}
	next := repoCIState{Status: result.Status, Checks: dtoChecks, HeadSHA: sha}
	s.mu.Lock()
	prev := s.repoCIStatus[relPath]
	changed := prev.Status != next.Status
	s.repoCIStatus[relPath] = next
	s.mu.Unlock()
	if changed {
		s.notifyTaskChange()
	}
}

// repoHasActiveTasksLocked returns true if relPath has any non-terminal tasks.
// Must be called with mu held.
func (s *Server) repoHasActiveTasksLocked(relPath string) bool {
	for _, e := range s.tasks {
		if p := e.task.Primary(); p != nil && p.Name == relPath && e.result == nil {
			return true
		}
	}
	return false
}
