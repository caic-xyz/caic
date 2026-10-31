// Projects runtime task metadata to the independent versioned header-cache schema.

package taskslog

import (
	"errors"
	"time"

	"github.com/caic-xyz/caic/backend/internal/tokens"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v9 "github.com/caic-xyz/caic/backend/internal/taskslog/data/headercache/v9"
)

func headerTaskToData(t *LoadedTask) *v9.Task {
	if t == nil {
		return nil
	}
	return &v9.Task{
		TaskID:                t.TaskID,
		Prompt:                t.Prompt,
		Title:                 t.Title,
		Repos:                 headerReposToData(t.Repos),
		Harness:               string(t.Harness),
		StartedAt:             t.StartedAt,
		LastStateUpdateAt:     t.LastStateUpdateAt,
		State:                 string(t.State),
		ForgeIssue:            t.ForgeIssue,
		OwnerID:               t.OwnerID,
		ForkedFromTaskID:      t.ForkedFromTaskID,
		ParentTaskID:          t.ParentTaskID,
		CaicMCP:               t.CaicMCP,
		ForgeOwner:            t.ForgeOwner,
		ForgeRepo:             t.ForgeRepo,
		ForgePR:               t.ForgePR,
		Tailscale:             t.Tailscale,
		USB:                   t.USB,
		Display:               t.Display,
		Sudo:                  t.Sudo,
		GitHubToken:           t.GitHubToken,
		RuntimeName:           string(t.RuntimeName),
		BaseImage:             t.BaseImage,
		ContainerPlatform:     t.ContainerPlatform,
		MaxCPUs:               t.MaxCPUs,
		CacheMounts:           headerCacheMountsToData(t.CacheMounts),
		Mounts:                headerMountsToData(t.Mounts),
		RequestedModel:        t.RequestedModel,
		RequestedEffort:       t.RequestedEffort,
		ReportedModel:         t.ReportedModel,
		ReportedEffort:        t.ReportedEffort,
		SessionID:             t.SessionID,
		AgentVersion:          t.AgentVersion,
		AutoCompactTokenLimit: t.AutoCompactTokenLimit,
		LogSize:               t.LogSize,
		DiffCreated:           t.DiffCreated,
		LastTrailer:           headerResultToData(t.LastTrailer),
	}
}

func headerTaskFromData(t *v9.Task) *LoadedTask {
	if t == nil {
		return nil
	}
	return &LoadedTask{
		TaskID:                t.TaskID,
		Prompt:                t.Prompt,
		Title:                 t.Title,
		Repos:                 headerReposFromData(t.Repos),
		Harness:               harness.Name(t.Harness),
		StartedAt:             t.StartedAt,
		LastStateUpdateAt:     t.LastStateUpdateAt,
		State:                 State(t.State),
		ForgeIssue:            t.ForgeIssue,
		OwnerID:               t.OwnerID,
		ForkedFromTaskID:      t.ForkedFromTaskID,
		ParentTaskID:          t.ParentTaskID,
		CaicMCP:               t.CaicMCP,
		ForgeOwner:            t.ForgeOwner,
		ForgeRepo:             t.ForgeRepo,
		ForgePR:               t.ForgePR,
		Tailscale:             t.Tailscale,
		USB:                   t.USB,
		Display:               t.Display,
		Sudo:                  t.Sudo,
		GitHubToken:           t.GitHubToken,
		RuntimeName:           runtime.Name(t.RuntimeName),
		BaseImage:             t.BaseImage,
		ContainerPlatform:     t.ContainerPlatform,
		MaxCPUs:               t.MaxCPUs,
		CacheMounts:           headerCacheMountsFromData(t.CacheMounts),
		Mounts:                headerMountsFromData(t.Mounts),
		RequestedModel:        t.RequestedModel,
		RequestedEffort:       t.RequestedEffort,
		ReportedModel:         t.ReportedModel,
		ReportedEffort:        t.ReportedEffort,
		SessionID:             t.SessionID,
		AgentVersion:          t.AgentVersion,
		AutoCompactTokenLimit: t.AutoCompactTokenLimit,
		LogSize:               t.LogSize,
		DiffCreated:           t.DiffCreated,
		LastTrailer:           headerResultFromData(t.LastTrailer),
	}
}

func headerReposToData(in []RepoMount) []v9.Repo {
	if in == nil {
		return nil
	}
	out := make([]v9.Repo, len(in))
	for i := range in {
		out[i] = v9.Repo(in[i])
	}
	return out
}

func headerReposFromData(in []v9.Repo) []RepoMount {
	if in == nil {
		return nil
	}
	out := make([]RepoMount, len(in))
	for i := range in {
		out[i] = RepoMount(in[i])
	}
	return out
}

func headerCacheMountsToData(in []runtime.CacheMount) []v9.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]v9.CacheMount, len(in))
	for i := range in {
		out[i] = v9.CacheMount(in[i])
	}
	return out
}

func headerCacheMountsFromData(in []v9.CacheMount) []runtime.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i := range in {
		out[i] = runtime.CacheMount(in[i])
	}
	return out
}

func headerMountsToData(in []runtime.Mount) []v9.Mount {
	if in == nil {
		return nil
	}
	out := make([]v9.Mount, len(in))
	for i := range in {
		out[i] = v9.Mount(in[i])
	}
	return out
}

func headerMountsFromData(in []v9.Mount) []runtime.Mount {
	if in == nil {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i := range in {
		out[i] = runtime.Mount(in[i])
	}
	return out
}

func headerDiffStatToData(in []v3.DiffFileStat) []v9.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v9.DiffFileStat, len(in))
	for i := range in {
		out[i] = v9.DiffFileStat(in[i])
	}
	return out
}

func headerDiffStatFromData(in []v9.DiffFileStat) []v3.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v3.DiffFileStat, len(in))
	for i := range in {
		out[i] = v3.DiffFileStat(in[i])
	}
	return out
}

func headerResultToData(r *Result) *v9.Result {
	if r == nil {
		return nil
	}
	text := ""
	if r.Err != nil {
		text = r.Err.Error()
	}
	var failure *v9.StartupFailure
	if r.StartupFailure != nil {
		failure = new(v9.StartupFailure(*r.StartupFailure))
	}
	return &v9.Result{State: string(r.State), DiffStat: headerDiffStatToData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageToData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Error: text}
}

func headerResultFromData(r *v9.Result) *Result {
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
	return &Result{State: State(r.State), DiffStat: headerDiffStatFromData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageFromData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Err: err}
}

func headerUsageToData(u agent.Usage) v9.Usage {
	buckets := make([]v9.CacheWrite, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, v9.CacheWrite{TTLSeconds: int64(b.TTL / time.Second), Tokens: b.Tokens})
	}
	return v9.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}

func headerUsageFromData(u v9.Usage) agent.Usage {
	buckets := make(tokens.CacheWrites, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, tokens.CacheWrite{TTL: time.Duration(b.TTLSeconds) * time.Second, Tokens: b.Tokens})
	}
	return agent.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}
