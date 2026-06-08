// HTTP handlers for bot-specific operations like fixing CI failures and fetching CI logs.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

type botHandlers struct {
	taskMgr    *tasks.Manager
	repos      *repos.Service
	forge      *ForgeManager
	provider   genai.Provider
	taskClient bot.Client
	taskRoutes *taskHandlers
}

// handleGetCILog fetches the log for a specific CI job by jobID.
// The jobID is a required query parameter; the caller knows it from the
// task's ciChecks field. The log is capped at ~8 KB (tail).
func (h *botHandlers) handleGetCILog(w http.ResponseWriter, r *http.Request) {
	entry, err := h.taskRoutes.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.Task()
	snap := t.Snapshot()
	ciPrimaryName := ""
	if p := t.Primary(); p != nil {
		ciPrimaryName = p.Name
	}
	info, ok := h.repos.InfoFor(ciPrimaryName)
	if !ok {
		writeError(w, api.BadRequest("no repo info found"))
		return
	}
	f := h.forge.forgeForInfo(r.Context(), &info)
	if f == nil {
		writeError(w, api.BadRequest("no forge token configured for this repo"))
		return
	}

	jobIDStr := r.URL.Query().Get("jobID")
	if jobIDStr == "" {
		writeError(w, api.BadRequest("jobID query parameter is required"))
		return
	}
	var jobID int64
	if _, scanErr := fmt.Sscanf(jobIDStr, "%d", &jobID); scanErr != nil || jobID <= 0 {
		writeError(w, api.BadRequest("invalid jobID"))
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
		writeError(w, api.NotFound("no CI check with that jobID"))
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

// fixCI creates a task to fix failing CI on a repo's default branch.
// It fetches CI logs via the forge, builds a rich prompt using bot.FailureSummary,
// and creates a new agent task — the same path as the automated maybeAutoFix.
func (h *botHandlers) fixCI(ctx context.Context, req *v1.BotFixCIReq) (*v1.CreateTaskResp, error) {
	info, ok := h.repos.InfoFor(req.Repo)
	if !ok {
		return nil, api.BadRequest("repo not found")
	}
	f := h.forge.forgeForInfo(ctx, &info)
	if f == nil {
		return nil, api.BadRequest("no forge token configured for this repo")
	}

	state := h.repos.CIStatusFor(req.Repo)

	if state.Status != forge.CIStatusFailure {
		return nil, api.BadRequest("no CI failure on default branch")
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
			Status:      c.Status,
			Conclusion:  c.Conclusion,
			QueuedAt:    c.QueuedAt,
			StartedAt:   c.StartedAt,
			CompletedAt: c.CompletedAt,
		}
	}
	result := forgecache.Result{Status: forge.CIStatusFailure, Checks: checks}
	summary := bot.FailureSummary(ctx, f, h.provider, result)

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}
	taskIDStr, err := h.taskClient.CreateTask(ctx, bot.TaskRequest{Repo: info.RelPath, Prompt: summary, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	taskID, err := ksid.Parse(taskIDStr)
	if err != nil {
		return nil, fmt.Errorf("parse task id: %w", err)
	}
	return &v1.CreateTaskResp{ID: taskID}, nil
}

// fixPR injects a fix-PR command into an existing task's agent session.
// It fetches CI logs via the forge using the task's existing CI checks,
// builds a rich prompt using bot.FailureSummary, and sends it as input to the task.
func (h *botHandlers) fixPR(ctx context.Context, req *v1.BotFixPRReq) (*v1.StatusResp, error) {
	entry, ok := h.taskMgr.GetEntry(req.TaskID)
	if !ok {
		return nil, api.NotFound("task")
	}
	t := entry.Task()
	snap := t.Snapshot()
	if snap.ForgePR == 0 {
		return nil, api.BadRequest("task has no associated PR")
	}
	primary := t.Primary()
	if primary == nil {
		return nil, api.BadRequest("task has no primary repo")
	}
	info, ok := h.repos.InfoFor(primary.Name)
	if !ok {
		return nil, api.BadRequest("repo not found")
	}
	f := h.forge.forgeForInfo(ctx, &info)
	if f == nil {
		return nil, api.BadRequest("no forge token configured for this repo")
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
	summary := bot.FailureSummary(ctx, f, h.provider, result)

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
