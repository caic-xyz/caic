// Task conversions from backend task entries to API DTOs.

package v1conv

import (
	"context"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// TaskResolvers supplies server-owned lookups needed to convert a task entry.
type TaskResolvers struct {
	RepoURL            func(name string) string
	RepoForge          func(name string) v1.Forge
	SudoPassword       func(ctx context.Context, t *task.Task) string
	OwnerName          func(ownerID string) string
	ContextWindowLimit func(repo string, harness harness.Name, model string) int
}

// Task converts a task entry to its API DTO.
func Task(ctx context.Context, e *taskmgr.Entry, r TaskResolvers) v1.Task {
	t := e.Task()
	// Read all volatile fields in a single locked snapshot to avoid data races
	// with addMessage/RestoreMessages.
	snap := t.Snapshot()

	taskRepos := make([]v1.TaskRepo, len(snap.Repos))
	for i, repo := range snap.Repos {
		taskRepos[i] = v1.TaskRepo{
			Name:       repo.Name,
			BaseBranch: repo.BaseBranch,
			Branch:     repo.Branch,
			RemoteURL:  callStringResolver(r.RepoURL, repo.Name),
			Forge:      callForgeResolver(r.RepoForge, repo.Name),
		}
	}
	if len(taskRepos) == 0 {
		taskRepos = nil
	}

	var primaryName string
	if p := t.Primary(); p != nil {
		primaryName = p.Name
	}

	// For purged tasks loaded from logs, snap stats may be zero because messages
	// aren't restored. Fall back to the result trailer.
	costUSD := snap.CostUSD
	numTurns := snap.NumTurns
	duration := snap.Duration
	cumulativeUsage := snap.Usage
	if e.Result() != nil {
		if costUSD == 0 {
			costUSD = e.Result().CostUSD
		}
		if numTurns == 0 {
			numTurns = e.Result().NumTurns
		}
		if duration == 0 {
			duration = e.Result().Duration
		}
		if cumulativeUsage == (agent.Usage{}) {
			cumulativeUsage = e.Result().Usage
		}
	}

	out := v1.Task{
		ID:            t.ID,
		InitialPrompt: t.InitialPrompt.Text,
		Title:         snap.Title,
		Repos:         taskRepos,
		Runtime: v1.RuntimeInstance{
			ID:           string(snap.RuntimeInstanceID),
			RuntimeName:  string(snap.RuntimeName),
			Tailscale:    tailscaleURLFromSnapshot(&snap),
			USB:          snap.USB,
			Display:      snap.Display,
			Sudo:         snap.Sudo,
			SudoPassword: callSudoPasswordResolver(ctx, r.SudoPassword, t),
			VNCPort:      snap.VNCPort,
		},
		State:          TaskState(ctx, snap.State),
		StateUpdatedAt: snap.StateUpdatedAt,
		Harness:        Harness(t.Harness),
		Model:          snap.Model,
		Effort:         t.Effort,
		AgentVersion:   snap.AgentVersion,
		SessionID:      snap.SessionID,
		InPlanMode:     snap.InPlanMode,
		PlanContent:    snap.PlanContent,
		GitHubToken:    snap.GitHubToken,
		CostUSD:        costUSD,
		NumTurns:       numTurns,
		Duration:       duration.Seconds(),
	}
	out.StartedAt = t.StartedAt
	out.TurnStartedAt = snap.TurnStartedAt
	if snap.RateLimit.Status == "rejected" && !snap.RateLimit.IsUsingOverage && snap.RateLimit.ResetsAt.After(time.Now()) {
		window := snap.RateLimit.QuotaWindow
		if window == "" {
			window = snap.RateLimit.RateLimitType
		}
		out.RateLimit = v1.TaskRateLimit{
			Blocked:  true,
			Window:   window,
			ResetsAt: snap.RateLimit.ResetsAt,
		}
	}
	out.CumulativeInputTokens = cumulativeUsage.InputTokens
	out.CumulativeOutputTokens = cumulativeUsage.OutputTokens
	out.CumulativeCacheCreationInputTokens = cumulativeUsage.CacheCreationInputTokens
	out.CumulativeCacheReadInputTokens = cumulativeUsage.CacheReadInputTokens
	// Active tokens = last API call's context window fill (not the per-query sum).
	out.ActiveInputTokens = snap.LastAPIUsage.InputTokens + snap.LastAPIUsage.CacheCreationInputTokens
	out.ActiveCacheReadTokens = snap.LastAPIUsage.CacheReadInputTokens
	out.CacheTTLSeconds = snap.LastAPIUsage.CacheTTLSeconds
	out.CacheExpiresAt = snap.CacheExpiresAt
	if snap.ContextWindowLimit > 0 {
		out.ContextWindowLimit = snap.ContextWindowLimit
	} else if primaryName != "" && r.ContextWindowLimit != nil {
		out.ContextWindowLimit = r.ContextWindowLimit(primaryName, t.Harness, snap.Model)
	}
	if e.Result() != nil {
		out.DiffStat = DiffStat(e.Result().DiffStat)
		out.Result = e.Result().AgentResult
		if e.Result().Err != nil {
			out.Error = e.Result().Err.Error()
		}
	} else {
		out.DiffStat = DiffStat(snap.DiffStat)
	}
	out.ForgeOwner = snap.ForgeOwner
	out.ForgeRepo = snap.ForgeRepo
	out.ForgePR = snap.ForgePR
	out.ForgePRState = ForgePRState(snap.ForgePRState)
	out.ForgeIssue = snap.ForgeIssue
	out.CIStatus = v1.CIStatus(snap.CIStatus)
	if len(snap.CIChecks) > 0 {
		out.CIChecks = make([]v1.ForgeCheck, len(snap.CIChecks))
		for i := range snap.CIChecks {
			out.CIChecks[i] = ForgeCheck(&snap.CIChecks[i])
		}
	}
	if t.OwnerID != "" {
		out.Owner = callStringResolver(r.OwnerName, t.OwnerID)
	}
	return out
}

func tailscaleURLFromSnapshot(s *task.Snapshot) string {
	if s.TailscaleFQDN != "" {
		return "https://" + s.TailscaleFQDN
	}
	if s.TailscaleAuthURL != "" {
		return s.TailscaleAuthURL
	}
	if s.Tailscale {
		return "true"
	}
	return ""
}

func callStringResolver(fn func(string) string, value string) string {
	if fn == nil {
		return ""
	}
	return fn(value)
}

func callForgeResolver(fn func(string) v1.Forge, value string) v1.Forge {
	if fn == nil {
		return ""
	}
	return fn(value)
}

func callSudoPasswordResolver(ctx context.Context, fn func(context.Context, *task.Task) string, t *task.Task) string {
	if fn == nil {
		return ""
	}
	return fn(ctx, t)
}
