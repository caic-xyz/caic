// CI check-run evaluation and complete, bounded failure prompts with linked diagnostic excerpts.

package ci

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

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

// maxFailurePromptBytes includes metadata, excerpts, omission notices and footer.
// Per-job logs are limited separately by forge.MaxLogExcerptBytes.
const maxFailurePromptBytes = 256 << 10

// repairWrapperBytes reserves PR metadata and instructions before collecting
// excerpts. ForPR bounds branch and URL metadata to fit this allowance.
const repairWrapperBytes = 8 << 10

// FailurePrompt owns an agent-facing CI failure summary and its repair wrappers.
// Its private text is constructed only by FailureSummary, which reserves space
// for bounded repair metadata so direct and wrapped prompts fit 256 KiB.
type FailurePrompt struct{ text string }

// FailureSummary builds a bounded agent-facing failure prompt one job at a time.
// Full logs remain available through job links. When the aggregate budget is
// exhausted, remaining checks are counted explicitly rather than fetched and
// retained. LLM failure and oversized responses follow the same excerpt budget.
func FailureSummary(ctx context.Context, log *slog.Logger, f forge.Forge, provider genai.Provider, result forgecache.Result) FailurePrompt {
	if log == nil {
		panic("logger is required")
	}
	failed := 0
	for i := range result.Checks {
		if result.Checks[i].Conclusion.IsFailed() {
			failed++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s CI: %d check(s) failed:\n", failureMetadata(f.Name(), 128), failed)
	included := 0
	// Leave enough room for an omitted-check count, cancellation and instruction.
	const footerBudget = 256
	for i := range result.Checks {
		c := &result.Checks[i]
		if !c.Conclusion.IsFailed() {
			continue
		}
		if ctx.Err() != nil {
			sb.WriteString("\n[CI log acquisition canceled; remaining checks omitted.]\n")
			break
		}
		if sb.Len()+8192 > maxFailurePromptBytes-repairWrapperBytes-footerBudget {
			break
		}
		if c.JobID != 0 && len(c.Labels) == 0 {
			labels, err := f.GetJobLabels(ctx, c.Owner, c.Repo, c.JobID)
			if err == nil {
				c.Labels = labels
			} else {
				log.WarnContext(ctx, "CI job labels unavailable", "job", c.JobID, "err", err)
			}
		}
		header := failureCheckHeader(f, c)
		allowance := min(forge.MaxLogExcerptBytes, maxFailurePromptBytes-repairWrapperBytes-footerBudget-sb.Len()-len(header)-32)
		if allowance < 128 {
			break
		}
		text := "(job log unavailable)"
		if c.JobID != 0 {
			fetched, err := f.GetJobLog(ctx, c.Owner, c.Repo, c.JobID, true)
			if err != nil {
				log.WarnContext(ctx, "CI job log unavailable", "job", c.JobID, "err", err)
				text = "(job log unavailable: " + failureMetadata(err.Error(), 1024) + ")"
			} else {
				text = forge.LimitLogExcerpt(fetched, forge.MaxLogExcerptBytes)
				if provider != nil && len(text) > 16_000 {
					if summary := summarizeCILog(ctx, log, provider, c.Name, text); summary != "" {
						text = "[Summary of bounded log excerpt; open job link for complete log.]\n" + summary
					}
				}
			}
		}
		sb.WriteString(header)
		fmt.Fprintf(&sb, "  Log:\n  ```\n%s\n  ```\n", forge.LimitLogExcerpt(text, allowance))
		included++
	}
	if omitted := failed - included; omitted > 0 {
		fmt.Fprintf(&sb, "\n[%d remaining failing check(s) omitted from this prompt; consult the forge CI results.]\n", omitted)
	}
	sb.WriteString("\nPlease fix the failures above.")
	return FailurePrompt{text: sb.String()}
}

// String returns the failure prompt for direct injection or default-branch repair.
func (p FailurePrompt) String() string { return p.text }

// ForPR adds PR repair instructions for manual and automatic repair tasks.
//
// It bounds metadata within the reserved wrapper allowance, preserving PR links
// when they fit and marking omitted links or abbreviated branch names. The
// bounded, independently owned summary remains intact.
func (p FailurePrompt) ForPR(prURL string, number int, branch string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CI failed on PR #%d", number)
	if len(prURL) > 4096 {
		sb.WriteString(" [PR link omitted: too long]")
	} else if prURL != "" {
		fmt.Fprintf(&sb, " (%s)", failureMetadata(prURL, 4096))
	}
	fmt.Fprintf(&sb, ". Please fix the failing CI checks on branch %q", failureMetadata(branch, 512))
	if len(branch) > 512 {
		sb.WriteString(" [branch name abbreviated]")
	}
	sb.WriteString(" and push the fix:\n\n")
	sb.WriteString(p.text)
	return sb.String()
}

func failureCheckHeader(f forge.Forge, c *forge.Check) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "- %s (%s), job %d", failureMetadata(c.Name, 1024), failureMetadata(string(c.Conclusion), 128), c.JobID)
	if len(c.Labels) > 0 {
		sb.WriteString(" [")
		used := 0
		for i, label := range c.Labels {
			if used >= 1021 {
				sb.WriteString("; additional labels omitted")
				break
			}
			if i > 0 {
				sb.WriteString(", ")
				used += 2
			}
			part := failureMetadata(label, 1024-used)
			sb.WriteString(part)
			used += len(part)
		}
		sb.WriteByte(']')
	}
	url := f.CIJobURL(c.Owner, c.Repo, c.RunID, c.JobID)
	if len(url) > 4096 {
		sb.WriteString(": [job link omitted: too long]")
	} else if url != "" {
		sb.WriteString(": ")
		sb.WriteString(failureMetadata(url, 4096))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// failureMetadata bounds untrusted check metadata before allocating formatting
// buffers. Excerpts have their own notice policy; metadata uses an ellipsis.
func failureMetadata(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n < 3 {
		return strings.Repeat(".", min(len(s), n))
	}
	omitted := len(s) > n
	s = s[:min(len(s), n)]
	s = strings.ToValidUTF8(s, "\uFFFD")
	if omitted || len(s) > n {
		s = s[:min(len(s), n-3)]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return strings.Clone(s)
}

const ciLogSummaryPrompt = `You are a CI log analyst. Extract only the meaningful error information from the CI log below.
Return a concise summary that a developer needs to fix the failure: the actual error messages, failing test names, compiler errors, or command failures.
Strip setup noise, download progress, timing lines, and successful steps.
Return plain text, no markdown.`

// summarizeCILog asks the LLM to extract the meaningful error from a large CI log.
// Returns empty string on failure so the caller can fall back to the raw log.
func summarizeCILog(ctx context.Context, log *slog.Logger, provider genai.Provider, checkName, logText string) string {
	input := fmt.Sprintf("CI check %q failed. Log:\n%s", failureMetadata(checkName, 1024), logText)
	res, err := provider.GenSync(ctx,
		genai.Messages{genai.NewTextMessage(input)},
		&genai.GenOptionText{SystemPrompt: ciLogSummaryPrompt},
	)
	if err != nil {
		log.WarnContext(ctx, "summarizeCILog: LLM call failed", "check", checkName, "err", err)
		return ""
	}
	return forge.LimitLogExcerpt(res.String(), forge.MaxLogExcerptBytes)
}
