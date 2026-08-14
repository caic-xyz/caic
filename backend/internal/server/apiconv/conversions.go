// API conversion functions between backend domain values and v1 DTOs.

package apiconv

import (
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// PromptToAgent converts v1.Prompt to agent.Prompt at the API boundary.
func PromptToAgent(p v1.Prompt) agent.Prompt {
	var images []agent.ImageData
	if len(p.Images) > 0 {
		images = make([]agent.ImageData, len(p.Images))
		for i, img := range p.Images {
			images[i] = agent.ImageData{MediaType: img.MediaType, Data: img.Data}
		}
	}
	return agent.Prompt{Text: p.Text, Images: images}
}

// Harness converts harness.Name to v1.Harness.
func Harness(h harness.Name) (v1.Harness, error) {
	switch h {
	case harness.Claude:
		return v1.HarnessClaude, nil
	case harness.Codex:
		return v1.HarnessCodex, nil
	case harness.OpenCode:
		return v1.HarnessOpenCode, nil
	case harness.Pi:
		return v1.HarnessPi, nil
	default:
		return "", fmt.Errorf("unsupported harness %q", h)
	}
}

// ParseHarness converts a harness name string to v1.Harness.
func ParseHarness(s string) (v1.Harness, error) {
	switch s {
	case string(harness.Claude):
		return v1.HarnessClaude, nil
	case string(harness.Codex):
		return v1.HarnessCodex, nil
	case string(harness.OpenCode):
		return v1.HarnessOpenCode, nil
	case string(harness.Pi):
		return v1.HarnessPi, nil
	default:
		return "", fmt.Errorf("unsupported harness %q", s)
	}
}

// AgentHarness converts v1.Harness to harness.Name.
func AgentHarness(h v1.Harness) (harness.Name, error) {
	switch h {
	case v1.HarnessClaude:
		return harness.Claude, nil
	case v1.HarnessCodex:
		return harness.Codex, nil
	case v1.HarnessOpenCode:
		return harness.OpenCode, nil
	case v1.HarnessPi:
		return harness.Pi, nil
	default:
		return "", fmt.Errorf("unsupported API harness %q", h)
	}
}

// TaskState converts task.State to v1.TaskState.
func TaskState(s task.State) (v1.TaskState, error) {
	switch s {
	case task.StatePending:
		return v1.TaskStatePending, nil
	case task.StateBranching:
		return v1.TaskStateBranching, nil
	case task.StateProvisioning:
		return v1.TaskStateProvisioning, nil
	case task.StateStarting:
		return v1.TaskStateStarting, nil
	case task.StateRunning:
		return v1.TaskStateRunning, nil
	case task.StateWaiting:
		return v1.TaskStateWaiting, nil
	case task.StateAsking:
		return v1.TaskStateAsking, nil
	case task.StateHasPlan:
		return v1.TaskStateHasPlan, nil
	case task.StatePulling:
		return v1.TaskStatePulling, nil
	case task.StatePushing:
		return v1.TaskStatePushing, nil
	case task.StateStopping:
		return v1.TaskStateStopping, nil
	case task.StateStopped:
		return v1.TaskStateStopped, nil
	case task.StatePurging:
		return v1.TaskStatePurging, nil
	case task.StateCrashed:
		return v1.TaskStateCrashed, nil
	case task.StateFailed:
		return v1.TaskStateFailed, nil
	case task.StatePurged:
		return v1.TaskStatePurged, nil
	default:
		return "", fmt.Errorf("unsupported task state %d", s)
	}
}

// ForgePRState converts forge.PRState to v1.ForgePRState.
func ForgePRState(s forge.PRState) (v1.ForgePRState, error) {
	switch s {
	case "":
		return "", nil
	case forge.PRStateOpen:
		return v1.ForgePRStateOpen, nil
	case forge.PRStateClosed:
		return v1.ForgePRStateClosed, nil
	case forge.PRStateMerged:
		return v1.ForgePRStateMerged, nil
	default:
		return "", fmt.Errorf("unsupported forge PR state %q", s)
	}
}

// CIStatus converts forge.CIStatus to v1.CIStatus.
func CIStatus(s forge.CIStatus) (v1.CIStatus, error) {
	switch s {
	case forge.CIStatusNone:
		return "", nil
	case forge.CIStatusPending:
		return v1.CIStatusPending, nil
	case forge.CIStatusSuccess:
		return v1.CIStatusSuccess, nil
	case forge.CIStatusFailure:
		return v1.CIStatusFailure, nil
	default:
		return "", fmt.Errorf("unsupported CI status %q", s)
	}
}

// RepoForge converts forge.Kind to v1.Forge.
func RepoForge(k forge.Kind) (v1.Forge, error) {
	switch k {
	case "":
		return "", nil
	case forge.KindGitHub:
		return v1.ForgeGitHub, nil
	case forge.KindGitLab:
		return v1.ForgeGitLab, nil
	default:
		return "", fmt.Errorf("unsupported forge %q", k)
	}
}

// SafetyIssues converts task safety issues to API DTOs.
func SafetyIssues(issues []repo.SafetyIssue) []v1.SafetyIssue {
	if len(issues) == 0 {
		return nil
	}
	out := make([]v1.SafetyIssue, len(issues))
	for i, si := range issues {
		out[i] = v1.SafetyIssue{File: si.File, Kind: si.Kind, Detail: si.Detail}
	}
	return out
}

// ProviderQuota converts a provider quota snapshot to an API DTO.
func ProviderQuota(q *usage.ProviderQuota) (v1.ProviderQuota, error) {
	if q == nil {
		return v1.ProviderQuota{}, nil
	}
	provider, err := quotaProvider(q.Provider)
	if err != nil {
		return v1.ProviderQuota{}, err
	}
	authKind, err := providerAuthKind(q.AuthKind)
	if err != nil {
		return v1.ProviderQuota{}, err
	}
	out := v1.ProviderQuota{
		Provider:   provider,
		Label:      q.Label,
		AuthKind:   authKind,
		RateLimits: make([]v1.QuotaRateLimit, len(q.RateLimits)),
		Balance: v1.QuotaBalance{
			Currency: q.Balance.Currency,
			Total:    q.Balance.Total,
			Granted:  q.Balance.Granted,
			ToppedUp: q.Balance.ToppedUp,
		},
		ExtraUsage: v1.QuotaExtraUsage{
			Currency:     q.ExtraUsage.Currency,
			IsEnabled:    q.ExtraUsage.IsEnabled,
			UsedCredits:  q.ExtraUsage.UsedCredits,
			MonthlyLimit: q.ExtraUsage.MonthlyLimit,
			UsedPct:      q.ExtraUsage.UsedPct,
		},
	}
	for i := range q.RateLimits {
		out.RateLimits[i] = v1.QuotaRateLimit{
			Window:   q.RateLimits[i].Window,
			UsedPct:  q.RateLimits[i].UsedPct,
			ResetsAt: q.RateLimits[i].ResetsAt,
		}
	}
	return out, nil
}

// ForgeCheck converts a forge check run to an API DTO.
func ForgeCheck(c *forge.Check) (v1.ForgeCheck, error) {
	status, err := checkStatus(c.Status)
	if err != nil {
		return v1.ForgeCheck{}, err
	}
	conclusion, err := checkConclusion(c.Conclusion)
	if err != nil {
		return v1.ForgeCheck{}, err
	}
	return v1.ForgeCheck{
		Name:        c.Name,
		Owner:       c.Owner,
		Repo:        c.Repo,
		RunID:       c.RunID,
		JobID:       c.JobID,
		Status:      status,
		Conclusion:  conclusion,
		QueuedAt:    c.QueuedAt,
		StartedAt:   c.StartedAt,
		CompletedAt: c.CompletedAt,
	}, nil
}

// DiffStat converts an agent diff stat to an API DTO.
func DiffStat(ds agent.DiffStat) v1.DiffStat {
	if len(ds) == 0 {
		return nil
	}
	out := make(v1.DiffStat, len(ds))
	for i, f := range ds {
		out[i] = v1.DiffFileStat{Path: f.Path, Added: f.Added, Deleted: f.Deleted, Binary: f.Binary}
	}
	return out
}

func quotaProvider(p agent.QuotaProvider) (v1.QuotaProvider, error) {
	switch p {
	case agent.QuotaProviderAnthropic:
		return v1.QuotaProviderAnthropic, nil
	case agent.QuotaProviderClaudeCode:
		return v1.QuotaProviderClaudeCode, nil
	case agent.QuotaProviderCodex:
		return v1.QuotaProviderCodex, nil
	case agent.QuotaProviderDeepSeek:
		return v1.QuotaProviderDeepSeek, nil
	case agent.QuotaProviderOpenRouter:
		return v1.QuotaProviderOpenRouter, nil
	case agent.QuotaProviderXiaomi:
		return v1.QuotaProviderXiaomi, nil
	default:
		return "", fmt.Errorf("unsupported quota provider %q", p)
	}
}

func providerAuthKind(k usage.AuthKind) (v1.ProviderAuthKind, error) {
	switch k {
	case usage.AuthKindOAuth:
		return v1.ProviderAuthKindOAuth, nil
	case usage.AuthKindAPIKey:
		return v1.ProviderAuthKindAPIKey, nil
	default:
		return "", fmt.Errorf("unsupported provider auth kind %q", k)
	}
}

func checkStatus(s forge.CheckRunStatus) (v1.CheckStatus, error) {
	switch s {
	case forge.CheckRunStatusQueued:
		return v1.CheckStatusQueued, nil
	case forge.CheckRunStatusInProgress:
		return v1.CheckStatusInProgress, nil
	case forge.CheckRunStatusCompleted:
		return v1.CheckStatusCompleted, nil
	default:
		return "", fmt.Errorf("unsupported check status %q", s)
	}
}

func checkConclusion(c forge.CheckRunConclusion) (v1.CheckConclusion, error) {
	switch c {
	case "":
		return "", nil
	case forge.CheckRunConclusionSuccess:
		return v1.CheckConclusionSuccess, nil
	case forge.CheckRunConclusionFailure:
		return v1.CheckConclusionFailure, nil
	case forge.CheckRunConclusionNeutral:
		return v1.CheckConclusionNeutral, nil
	case forge.CheckRunConclusionSkipped:
		return v1.CheckConclusionSkipped, nil
	case forge.CheckRunConclusionCancelled:
		return v1.CheckConclusionCancelled, nil
	case forge.CheckRunConclusionTimedOut:
		return v1.CheckConclusionTimedOut, nil
	case forge.CheckRunConclusionActionRequired:
		return v1.CheckConclusionActionRequired, nil
	case forge.CheckRunConclusionStale:
		return v1.CheckConclusionStale, nil
	default:
		return "", fmt.Errorf("unsupported check conclusion %q", c)
	}
}

// ProcessInfos converts runtime process info to API DTOs.
func ProcessInfos(procs []runtime.ProcessInfo) []v1.ProcessInfo {
	out := make([]v1.ProcessInfo, len(procs))
	for i := range procs {
		p := procs[i]
		out[i] = v1.ProcessInfo{
			PID:       p.PID,
			PPID:      p.PPID,
			PGRP:      p.PGRP,
			User:      p.User,
			State:     p.State,
			Priority:  p.Priority,
			Nice:      p.Nice,
			Threads:   p.Threads,
			CPU:       p.CPU,
			Mem:       p.Mem,
			RSSBytes:  p.RSSBytes,
			CPUTime:   p.CPUTime,
			StartedAt: p.StartedAt,
			Command:   p.Command,
		}
	}
	return out
}

// StatsEvent converts runtime stats to an API event.
func StatsEvent(cs *runtime.Stats) v1.EventMessage {
	return v1.EventMessage{
		Kind: v1.EventKindStats,
		Ts:   cs.Ts.UnixMilli(),
		Stats: &v1.EventStats{
			Ts:         cs.Ts.UnixMilli(),
			CPUPerc:    cs.CPUPerc,
			MemUsed:    cs.MemUsed,
			MemLimit:   cs.MemLimit,
			MemPerc:    cs.MemPerc,
			NetRx:      cs.NetRx,
			NetTx:      cs.NetTx,
			BlockRead:  cs.BlockRead,
			BlockWrite: cs.BlockWrite,
			DiskUsed:   cs.DiskUsed,
		},
	}
}
