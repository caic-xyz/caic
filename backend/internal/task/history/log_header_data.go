// Projects runtime task metadata to the independent versioned header-cache schema.

package history

import (
	"errors"
	"time"

	"github.com/caic-xyz/caic/backend/internal/tokens"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	v14 "github.com/caic-xyz/caic/backend/internal/task/history/data/headercache/v14"
)

func headerTaskToData(t *LoadedTask) *v14.Task {
	if t == nil {
		return nil
	}
	return &v14.Task{
		TaskID:                         t.TaskID,
		Prompt:                         t.Prompt,
		Title:                          t.Title,
		Repos:                          headerReposToData(t.Repos),
		Harness:                        string(t.Harness),
		StartedAt:                      t.StartedAt,
		LastActivityAt:                 t.LastActivityAt,
		State:                          string(t.State),
		ForgeIssue:                     t.ForgeIssue,
		OwnerID:                        t.OwnerID,
		ForkedFromTaskID:               t.ForkedFromTaskID,
		ParentTaskID:                   t.ParentTaskID,
		CaicMCP:                        t.CaicMCP,
		ForgeOwner:                     t.ForgeOwner,
		ForgeRepo:                      t.ForgeRepo,
		ForgePR:                        t.ForgePR,
		Tailscale:                      t.Tailscale,
		USB:                            t.USB,
		Display:                        t.Display,
		Sudo:                           t.Sudo,
		GitHubToken:                    t.GitHubToken,
		RuntimeName:                    string(t.RuntimeName),
		BaseImage:                      t.BaseImage,
		ContainerPlatform:              t.ContainerPlatform,
		MaxCPUs:                        t.MaxCPUs,
		CacheMounts:                    headerCacheMountsToData(t.CacheMounts),
		Mounts:                         headerMountsToData(t.Mounts),
		RequestedModel:                 t.RequestedModel,
		RequestedEffort:                t.RequestedEffort,
		RequestedAutoCompactTokenLimit: t.RequestedAutoCompactTokenLimit,
		RequestedCompactionBudget:      v14.CompactionBudget(t.RequestedCompactionBudget),
		ReportedModel:                  t.ReportedModel,
		ReportedEffort:                 t.ReportedEffort,
		SessionID:                      t.SessionID,
		AgentVersion:                   t.AgentVersion,
		AutoCompactTokenLimit:          t.AutoCompactTokenLimit,
		LogSize:                        t.LogSize,
		DiffCreated:                    t.DiffCreated,
		LastTrailer:                    headerResultToData(t.LastTrailer),
	}
}

func headerTaskFromData(t *v14.Task) *LoadedTask {
	if t == nil {
		return nil
	}
	return &LoadedTask{
		ReportedModel:                  t.ReportedModel,
		ReportedEffort:                 t.ReportedEffort,
		SessionID:                      t.SessionID,
		AgentVersion:                   t.AgentVersion,
		AutoCompactTokenLimit:          t.AutoCompactTokenLimit,
		TaskID:                         t.TaskID,
		Prompt:                         t.Prompt,
		Title:                          t.Title,
		Repos:                          headerReposFromData(t.Repos),
		Harness:                        harness.Name(t.Harness),
		StartedAt:                      t.StartedAt,
		LastActivityAt:                 t.LastActivityAt,
		State:                          task.State(t.State),
		ForgeIssue:                     t.ForgeIssue,
		OwnerID:                        t.OwnerID,
		ForkedFromTaskID:               t.ForkedFromTaskID,
		ParentTaskID:                   t.ParentTaskID,
		CaicMCP:                        t.CaicMCP,
		ForgeOwner:                     t.ForgeOwner,
		ForgeRepo:                      t.ForgeRepo,
		ForgePR:                        t.ForgePR,
		Tailscale:                      t.Tailscale,
		USB:                            t.USB,
		Display:                        t.Display,
		Sudo:                           t.Sudo,
		GitHubToken:                    t.GitHubToken,
		RuntimeName:                    runtime.Name(t.RuntimeName),
		BaseImage:                      t.BaseImage,
		ContainerPlatform:              t.ContainerPlatform,
		MaxCPUs:                        t.MaxCPUs,
		CacheMounts:                    headerCacheMountsFromData(t.CacheMounts),
		Mounts:                         headerMountsFromData(t.Mounts),
		RequestedModel:                 t.RequestedModel,
		RequestedEffort:                t.RequestedEffort,
		RequestedAutoCompactTokenLimit: t.RequestedAutoCompactTokenLimit,
		RequestedCompactionBudget:      agent.CompactionBudget(t.RequestedCompactionBudget),
		LogSize:                        t.LogSize,
		DiffCreated:                    t.DiffCreated,
		LastTrailer:                    headerResultFromData(t.LastTrailer),
	}
}

func headerReposToData(in []repo.Mount) []v14.Repo {
	if in == nil {
		return nil
	}
	out := make([]v14.Repo, len(in))
	for i := range in {
		out[i] = v14.Repo(in[i])
	}
	return out
}

func headerReposFromData(in []v14.Repo) []repo.Mount {
	if in == nil {
		return nil
	}
	out := make([]repo.Mount, len(in))
	for i := range in {
		out[i] = repo.Mount(in[i])
	}
	return out
}

func headerCacheMountsToData(in []runtime.CacheMount) []v14.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]v14.CacheMount, len(in))
	for i := range in {
		out[i] = v14.CacheMount(in[i])
	}
	return out
}

func headerCacheMountsFromData(in []v14.CacheMount) []runtime.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i := range in {
		out[i] = runtime.CacheMount(in[i])
	}
	return out
}

func headerMountsToData(in []runtime.Mount) []v14.Mount {
	if in == nil {
		return nil
	}
	out := make([]v14.Mount, len(in))
	for i := range in {
		out[i] = v14.Mount(in[i])
	}
	return out
}

func headerMountsFromData(in []v14.Mount) []runtime.Mount {
	if in == nil {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i := range in {
		out[i] = runtime.Mount(in[i])
	}
	return out
}

func headerDiffStatToData(in []v3.DiffFileStat) []v14.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v14.DiffFileStat, len(in))
	for i := range in {
		out[i] = v14.DiffFileStat(in[i])
	}
	return out
}

func headerDiffStatFromData(in []v14.DiffFileStat) []v3.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v3.DiffFileStat, len(in))
	for i := range in {
		out[i] = v3.DiffFileStat(in[i])
	}
	return out
}

func headerResultToData(r *task.Result) *v14.Result {
	if r == nil {
		return nil
	}
	text := ""
	if r.Err != nil {
		text = r.Err.Error()
	}
	var failure *v14.StartupFailure
	if r.StartupFailure != nil {
		failure = new(v14.StartupFailure(*r.StartupFailure))
	}
	return &v14.Result{State: string(r.State), DiffStat: headerDiffStatToData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, CostKnown: r.CostKnown, CostComplete: r.CostComplete, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageToData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Error: text}
}

func headerResultFromData(r *v14.Result) *task.Result {
	if r == nil {
		return nil
	}
	var err error
	if r.Error != "" {
		err = errors.New(r.Error)
	}
	var failure *v3.StartupFailure
	if r.StartupFailure != nil {
		failure = new(v3.StartupFailure(*r.StartupFailure))
	}
	return &task.Result{State: task.State(r.State), DiffStat: headerDiffStatFromData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, CostKnown: r.CostKnown, CostComplete: r.CostComplete, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageFromData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Err: err}
}

func headerUsageToData(u agent.Usage) v14.Usage {
	buckets := make([]v14.CacheWrite, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, v14.CacheWrite{TTLSeconds: int64(b.TTL / time.Second), Tokens: b.Tokens})
	}
	return v14.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}

func headerUsageFromData(u v14.Usage) agent.Usage {
	buckets := make(tokens.CacheWrites, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, tokens.CacheWrite{TTL: time.Duration(b.TTLSeconds) * time.Second, Tokens: b.Tokens})
	}
	return agent.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}
