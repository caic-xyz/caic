// CI check-run evaluation and failure summary building for bot-driven CI workflows.
package bot

import (
	"fmt"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/cicache"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// forgeRunToCheck converts a forge.CheckRun to a cicache.ForgeCheck.
func forgeRunToCheck(owner, repo string, r *forge.CheckRun) cicache.ForgeCheck {
	return cicache.ForgeCheck{
		Name:        r.Name,
		Owner:       owner,
		Repo:        repo,
		RunID:       r.RunID,
		JobID:       r.JobID,
		Status:      string(r.Status),
		Conclusion:  cicache.CheckConclusion(r.Conclusion),
		QueuedAt:    r.QueuedAt,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
	}
}

// EvaluateCheckRuns inspects runs for a SHA and returns a cicache.Result plus
// whether all checks have completed (done=true). Only call with len(runs)>0.
func EvaluateCheckRuns(owner, repo string, runs []forge.CheckRun) (cicache.Result, bool) {
	checks := make([]cicache.ForgeCheck, len(runs))
	allDone := true
	anyFailed := false
	for i := range runs {
		checks[i] = forgeRunToCheck(owner, repo, &runs[i])
		if runs[i].Status != forge.CheckRunStatusCompleted {
			allDone = false
		} else if runs[i].Conclusion.IsFailed() {
			anyFailed = true
		}
	}
	if !allDone {
		return cicache.Result{Checks: checks}, false
	}
	status := cicache.StatusSuccess
	if anyFailed {
		status = cicache.StatusFailure
	}
	return cicache.Result{Status: status, Checks: checks}, true
}

// InterimCIStatus returns the CI status and task-level checks to display while
// checks are still running. Returns CIStatusFailure as soon as any completed
// check has a failing conclusion, otherwise CIStatusPending.
func InterimCIStatus(runs []forge.CheckRun, checks []cicache.ForgeCheck) (task.CIStatus, []task.CICheck) {
	status := task.CIStatusPending
	for i := range runs {
		if runs[i].Status == forge.CheckRunStatusCompleted && runs[i].Conclusion.IsFailed() {
			status = task.CIStatusFailure
			break
		}
	}
	taskChecks := make([]task.CICheck, len(checks))
	for i := range checks {
		taskChecks[i] = CacheCheckToTask(&checks[i])
	}
	return status, taskChecks
}

// CacheCheckToTask converts a single cicache.ForgeCheck to a task.CICheck.
func CacheCheckToTask(c *cicache.ForgeCheck) task.CICheck {
	return task.CICheck{
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

// FailureSummary builds the agent-facing text summary for a CI failure result,
// listing each failing check with its conclusion and job URL where available.
func FailureSummary(f forge.Forge, result cicache.Result) string {
	var sb strings.Builder
	numFailed := 0
	for i := range result.Checks {
		if forge.CheckRunConclusion(result.Checks[i].Conclusion).IsFailed() {
			numFailed++
		}
	}
	fmt.Fprintf(&sb, "%s CI: %d check(s) failed:\n", f.Name(), numFailed)
	for i := range result.Checks {
		c := &result.Checks[i]
		if !forge.CheckRunConclusion(c.Conclusion).IsFailed() {
			continue
		}
		if jobURL := f.CIJobURL(c.Owner, c.Repo, c.RunID, c.JobID); jobURL != "" {
			fmt.Fprintf(&sb, "- %s (%s): %s\n", c.Name, c.Conclusion, jobURL)
		} else {
			fmt.Fprintf(&sb, "- %s (%s)\n", c.Name, c.Conclusion)
		}
	}
	sb.WriteString("\nPlease fix the failures above.")
	return strings.TrimRight(sb.String(), "\n")
}
