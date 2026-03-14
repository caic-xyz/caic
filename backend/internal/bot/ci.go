// CI check-run evaluation and failure summary building for bot-driven CI workflows.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
)

// EvaluateCheckRuns inspects runs for a SHA and returns a forgecache.Result plus
// whether all checks have completed (done=true). Only call with len(runs)>0.
func EvaluateCheckRuns(owner, repo string, runs []forge.CheckRun) (forgecache.Result, bool) {
	checks := make([]forge.Check, len(runs))
	allDone := true
	anyFailed := false
	for i := range runs {
		checks[i] = forge.CheckFromRun(owner, repo, &runs[i])
		if runs[i].Status != forge.CheckRunStatusCompleted {
			allDone = false
		} else if runs[i].Conclusion.IsFailed() {
			anyFailed = true
		}
	}
	if !allDone {
		return forgecache.Result{Checks: checks}, false
	}
	status := forge.CIStatusSuccess
	if anyFailed {
		status = forge.CIStatusFailure
	}
	return forgecache.Result{Status: status, Checks: checks}, true
}

// InterimCIStatus returns the CI status to display while checks are still
// running. Returns CIStatusFailure as soon as any completed check has a
// failing conclusion, otherwise CIStatusPending.
func InterimCIStatus(runs []forge.CheckRun) forge.CIStatus {
	for i := range runs {
		if runs[i].Status == forge.CheckRunStatusCompleted && runs[i].Conclusion.IsFailed() {
			return forge.CIStatusFailure
		}
	}
	return forge.CIStatusPending
}

// FailureSummary builds the agent-facing text summary for a CI failure result.
// It fetches the log for each failing check, optionally summarises large logs
// via the LLM provider, and formats the result with job URLs and log excerpts.
// provider may be nil (LLM summarisation is skipped).
func FailureSummary(ctx context.Context, f forge.Forge, provider genai.Provider, result forgecache.Result) string {
	logs := fetchFailingJobLogs(ctx, f, provider, result)

	var sb strings.Builder
	numFailed := 0
	for i := range result.Checks {
		if result.Checks[i].Conclusion.IsFailed() {
			numFailed++
		}
	}
	fmt.Fprintf(&sb, "%s CI: %d check(s) failed:\n", f.Name(), numFailed)
	for i := range result.Checks {
		c := &result.Checks[i]
		if !c.Conclusion.IsFailed() {
			continue
		}
		if jobURL := f.CIJobURL(c.Owner, c.Repo, c.RunID, c.JobID); jobURL != "" {
			fmt.Fprintf(&sb, "- %s (%s): %s\n", c.Name, c.Conclusion, jobURL)
		} else {
			fmt.Fprintf(&sb, "- %s (%s)\n", c.Name, c.Conclusion)
		}
		if logText := logs[c.JobID]; logText != "" {
			fmt.Fprintf(&sb, "  Log:\n  ```\n%s\n  ```\n", logText)
		}
	}
	sb.WriteString("\nPlease fix the failures above.")
	return strings.TrimRight(sb.String(), "\n")
}

// fetchFailingJobLogs fetches the log for each failing check, extracts the
// relevant step content, and summarises via LLM if the result is still large.
func fetchFailingJobLogs(ctx context.Context, f forge.Forge, provider genai.Provider, result forgecache.Result) map[int64]string {
	const summarizeAbove = 16_000   // ask LLM to summarize logs larger than this
	const summaryMaxChars = 100_000 // truncate input to LLM
	logs := make(map[int64]string)
	for i := range result.Checks {
		c := &result.Checks[i]
		if !c.Conclusion.IsFailed() || c.JobID == 0 {
			continue
		}
		logText, err := f.GetJobLog(ctx, c.Owner, c.Repo, c.JobID, true)
		if err != nil {
			slog.Warn("fetchFailingJobLogs: get log", "job", c.JobID, "check", c.Name, "err", err)
			continue
		}
		// Summarize with LLM when the log is still large.
		if provider != nil && len(logText) > summarizeAbove {
			if summary := summarizeCILog(ctx, provider, c.Name, logText, summaryMaxChars); summary != "" {
				logs[c.JobID] = summary
				continue
			}
		}
		logs[c.JobID] = logText
	}
	return logs
}

const ciLogSummaryPrompt = `You are a CI log analyst. Extract only the meaningful error information from the CI log below.
Return a concise summary that a developer needs to fix the failure: the actual error messages, failing test names, compiler errors, or command failures.
Strip setup noise, download progress, timing lines, and successful steps.
Return plain text, no markdown.`

// summarizeCILog asks the LLM to extract the meaningful error from a large CI log.
// Returns empty string on failure so the caller can fall back to the raw log.
func summarizeCILog(ctx context.Context, provider genai.Provider, checkName, logText string, maxChars int) string {
	if len(logText) > maxChars {
		logText = logText[len(logText)-maxChars:]
	}
	input := fmt.Sprintf("CI check %q failed. Log:\n%s", checkName, logText)
	res, err := provider.GenSync(ctx,
		genai.Messages{genai.NewTextMessage(input)},
		&genai.GenOptionText{SystemPrompt: ciLogSummaryPrompt},
	)
	if err != nil {
		slog.Warn("summarizeCILog: LLM call failed", "check", checkName, "err", err)
		return ""
	}
	return res.String()
}
