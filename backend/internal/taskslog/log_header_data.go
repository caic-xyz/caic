// Projects runtime task metadata to the independent versioned header-cache schema.

package taskslog

import (
	"errors"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v6 "github.com/caic-xyz/caic/backend/internal/taskslog/data/headercache/v6"
)

func headerTaskToData(t *LoadedTask) *v6.Task {
	if t == nil {
		return nil
	}
	return &v6.Task{
		TaskID:            t.TaskID,
		Prompt:            t.Prompt,
		Title:             t.Title,
		Repos:             headerReposToData(t.Repos),
		LogVersion:        int(t.LogVersion),
		Harness:           string(t.Harness),
		StartedAt:         t.StartedAt,
		LastStateUpdateAt: t.LastStateUpdateAt,
		State:             string(t.State),
		ForgeIssue:        t.ForgeIssue,
		OwnerID:           t.OwnerID,
		ForkedFromTaskID:  t.ForkedFromTaskID,
		ParentTaskID:      t.ParentTaskID,
		CaicMCP:           t.CaicMCP,
		ForgeOwner:        t.ForgeOwner,
		ForgeRepo:         t.ForgeRepo,
		ForgePR:           t.ForgePR,
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		GitHubToken:       t.GitHubToken,
		RuntimeName:       string(t.RuntimeName),
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       headerCacheMountsToData(t.CacheMounts),
		Mounts:            headerMountsToData(t.Mounts),
		RequestedModel:    t.RequestedModel,
		RequestedEffort:   t.RequestedEffort,
		ReportedModel:     t.ReportedModel,
		ReportedEffort:    t.ReportedEffort,
		SessionID:         t.SessionID,
		AgentVersion:      t.AgentVersion,
		LogSize:           t.LogSize,
		DiffCreated:       t.DiffCreated,
		LastTrailer:       headerResultToData(t.LastTrailer),
	}
}

func headerTaskFromData(t *v6.Task) *LoadedTask {
	if t == nil {
		return nil
	}
	return &LoadedTask{
		TaskID:            t.TaskID,
		Prompt:            t.Prompt,
		Title:             t.Title,
		Repos:             headerReposFromData(t.Repos),
		LogVersion:        agent.LogVersion(t.LogVersion),
		Harness:           harness.Name(t.Harness),
		StartedAt:         t.StartedAt,
		LastStateUpdateAt: t.LastStateUpdateAt,
		State:             State(t.State),
		ForgeIssue:        t.ForgeIssue,
		OwnerID:           t.OwnerID,
		ForkedFromTaskID:  t.ForkedFromTaskID,
		ParentTaskID:      t.ParentTaskID,
		CaicMCP:           t.CaicMCP,
		ForgeOwner:        t.ForgeOwner,
		ForgeRepo:         t.ForgeRepo,
		ForgePR:           t.ForgePR,
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		GitHubToken:       t.GitHubToken,
		RuntimeName:       runtime.Name(t.RuntimeName),
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       headerCacheMountsFromData(t.CacheMounts),
		Mounts:            headerMountsFromData(t.Mounts),
		RequestedModel:    t.RequestedModel,
		RequestedEffort:   t.RequestedEffort,
		ReportedModel:     t.ReportedModel,
		ReportedEffort:    t.ReportedEffort,
		SessionID:         t.SessionID,
		AgentVersion:      t.AgentVersion,
		LogSize:           t.LogSize,
		DiffCreated:       t.DiffCreated,
		LastTrailer:       headerResultFromData(t.LastTrailer),
	}
}

func headerReposToData(in []RepoMount) []v6.Repo {
	if in == nil {
		return nil
	}
	out := make([]v6.Repo, len(in))
	for i := range in {
		out[i] = v6.Repo(in[i])
	}
	return out
}

func headerReposFromData(in []v6.Repo) []RepoMount {
	if in == nil {
		return nil
	}
	out := make([]RepoMount, len(in))
	for i := range in {
		out[i] = RepoMount(in[i])
	}
	return out
}

func headerCacheMountsToData(in []runtime.CacheMount) []v6.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]v6.CacheMount, len(in))
	for i := range in {
		out[i] = v6.CacheMount(in[i])
	}
	return out
}

func headerCacheMountsFromData(in []v6.CacheMount) []runtime.CacheMount {
	if in == nil {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i := range in {
		out[i] = runtime.CacheMount(in[i])
	}
	return out
}

func headerMountsToData(in []runtime.Mount) []v6.Mount {
	if in == nil {
		return nil
	}
	out := make([]v6.Mount, len(in))
	for i := range in {
		out[i] = v6.Mount(in[i])
	}
	return out
}

func headerMountsFromData(in []v6.Mount) []runtime.Mount {
	if in == nil {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i := range in {
		out[i] = runtime.Mount(in[i])
	}
	return out
}

func headerDiffStatToData(in []agent.DiffFileStat) []v6.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v6.DiffFileStat, len(in))
	for i := range in {
		out[i] = v6.DiffFileStat(in[i])
	}
	return out
}

func headerDiffStatFromData(in []v6.DiffFileStat) []agent.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]agent.DiffFileStat, len(in))
	for i := range in {
		out[i] = agent.DiffFileStat(in[i])
	}
	return out
}

func headerResultToData(r *Result) *v6.Result {
	if r == nil {
		return nil
	}
	text := ""
	if r.Err != nil {
		text = r.Err.Error()
	}
	var failure *v6.StartupFailure
	if r.StartupFailure != nil {
		failure = new(v6.StartupFailure(*r.StartupFailure))
	}
	return &v6.Result{State: string(r.State), DiffStat: headerDiffStatToData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, Duration: r.Duration, NumTurns: r.NumTurns, Usage: v6.Usage(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Error: text}
}

func headerResultFromData(r *v6.Result) *Result {
	if r == nil {
		return nil
	}
	var err error
	if r.Error != "" {
		err = errors.New(r.Error)
	}
	var failure *agent.StartupFailure
	if r.StartupFailure != nil {
		failure = new(agent.StartupFailure(*r.StartupFailure))
	}
	return &Result{State: State(r.State), DiffStat: headerDiffStatFromData(r.DiffStat), DiskUsedBytes: r.DiskUsedBytes, CostUSD: r.CostUSD, Duration: r.Duration, NumTurns: r.NumTurns, Usage: agent.Usage(r.Usage), AgentResult: r.AgentResult, StartupFailure: failure, Err: err}
}
