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
	v11 "github.com/caic-xyz/caic/backend/internal/task/history/data/headercache/v11"
)

func headerTaskToData(t *LoadedTask) *v11.Task {
	if t == nil {
		return nil
	}
	return &v11.Task{
		TaskID:                t.TaskID,
		Prompt:                t.Prompt,
		Title:                 t.Title,
		Repos:                 headerReposToData(t.Repos),
		Harness:               string(t.Harness),
		StartedAt:             t.StartedAt,
		LastStateUpdateAt:     t.LastActivityAt, // v11 keeps the historical field name.
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

func headerTaskFromData(t *v11.Task) *LoadedTask {
	if t == nil {
		return nil
	}
	return &LoadedTask{
		ReportedModel:         t.ReportedModel,
		ReportedEffort:        t.ReportedEffort,
		SessionID:             t.SessionID,
		AgentVersion:          t.AgentVersion,
		AutoCompactTokenLimit: t.AutoCompactTokenLimit,
		TaskID:                t.TaskID,
		Prompt:                t.Prompt,
		Title:                 t.Title,
		Repos:                 headerReposFromData(t.Repos),
		Harness:               harness.Name(t.Harness),
		StartedAt:             t.StartedAt,
		LastActivityAt:        t.LastStateUpdateAt,
		State:                 task.State(t.State),
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
		LogSize:               t.LogSize,
		DiffCreated:           t.DiffCreated,
		LastTrailer:           headerResultFromData(t.LastTrailer),
	}
}

func headerReposToData(in []repo.Mount) []v11.Repo {
	if in == nil {
		return nil
	}
	out := make([]v11.Repo, len(in))
	for i := range in {
		out[i] = v11.Repo(in[i])
	}
	return out
}

func headerReposFromData(in []v11.Repo) []repo.Mount {
	if in == nil {
		return nil
	}
	out := make([]repo.Mount, len(in))
	for i := range in {
		out[i] = repo.Mount(in[i])
	}
	return out
}

func headerCacheMountsToData(in []runtime.CacheMount) []v11.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]v11.CacheMount, len(in))
	for i := range in {
		out[i] = v11.CacheMount(in[i])
	}
	return out
}

func headerCacheMountsFromData(in []v11.CacheMount) []runtime.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i := range in {
		out[i] = runtime.CacheMount(in[i])
	}
	return out
}

func headerMountsToData(in []runtime.Mount) []v11.Mount {
	if in == nil {
		return nil
	}
	out := make([]v11.Mount, len(in))
	for i := range in {
		out[i] = v11.Mount(in[i])
	}
	return out
}

func headerMountsFromData(in []v11.Mount) []runtime.Mount {
	if in == nil {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i := range in {
		out[i] = runtime.Mount(in[i])
	}
	return out
}

func headerDiffStatToData(in []v3.DiffFileStat) []v11.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v11.DiffFileStat, len(in))
	for i := range in {
		out[i] = v11.DiffFileStat(in[i])
	}
	return out
}

func headerDiffStatFromData(in []v11.DiffFileStat) []v3.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v3.DiffFileStat, len(in))
	for i := range in {
		out[i] = v3.DiffFileStat(in[i])
	}
	return out
}

func headerResultToData(r *task.Result) *v11.Result {
	if r == nil {
		return nil
	}
	text := ""
	if r.Err != nil {
		text = r.Err.Error()
	}
	var failure *v11.StartupFailure
	if r.StartupFailure != nil {
		failure = new(v11.StartupFailure(*r.StartupFailure))
	}
	return &v11.Result{State: string(r.State), DiffStat: headerDiffStatToData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageToData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Error: text}
}

func headerResultFromData(r *v11.Result) *task.Result {
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
	// TODO: Persist CostKnown in the header cache (next schema version) so an
	// exact reported zero survives cache-only loads. This fallback marks only a
	// nonzero cost as known.
	return &task.Result{State: task.State(r.State), DiffStat: headerDiffStatFromData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, CostKnown: r.CostUSD != 0, Duration: r.Duration, NumTurns: r.NumTurns, Usage: headerUsageFromData(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Err: err}
}

func headerUsageToData(u agent.Usage) v11.Usage {
	buckets := make([]v11.CacheWrite, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, v11.CacheWrite{TTLSeconds: int64(b.TTL / time.Second), Tokens: b.Tokens})
	}
	return v11.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}

func headerUsageFromData(u v11.Usage) agent.Usage {
	buckets := make(tokens.CacheWrites, 0, len(u.CacheWrites))
	if u.CacheWrites == nil {
		buckets = nil
	}
	for _, b := range u.CacheWrites {
		buckets = append(buckets, tokens.CacheWrite{TTL: time.Duration(b.TTLSeconds) * time.Second, Tokens: b.Tokens})
	}
	return agent.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens, CacheWrites: buckets, CacheReadInputTokens: u.CacheReadInputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens, CacheTTLSeconds: u.CacheTTLSeconds}
}
