// Shared conversions between backend domain values and API v1 DTOs.

package v1conv

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/repowork"
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
func Harness(h harness.Name) v1.Harness {
	return v1.Harness(h)
}

// AgentHarness converts v1.Harness to harness.Name.
func AgentHarness(h v1.Harness) harness.Name {
	return harness.Name(h)
}

// TaskState converts task.State to v1.TaskState.
func TaskState(ctx context.Context, s task.State) v1.TaskState {
	switch s {
	case task.StatePending:
		return v1.TaskStatePending
	case task.StateBranching:
		return v1.TaskStateBranching
	case task.StateProvisioning:
		return v1.TaskStateProvisioning
	case task.StateStarting:
		return v1.TaskStateStarting
	case task.StateRunning:
		return v1.TaskStateRunning
	case task.StateWaiting:
		return v1.TaskStateWaiting
	case task.StateAsking:
		return v1.TaskStateAsking
	case task.StateHasPlan:
		return v1.TaskStateHasPlan
	case task.StatePulling:
		return v1.TaskStatePulling
	case task.StatePushing:
		return v1.TaskStatePushing
	case task.StateStopping:
		return v1.TaskStateStopping
	case task.StateStopped:
		return v1.TaskStateStopped
	case task.StatePurging:
		return v1.TaskStatePurging
	case task.StateCrashed:
		return v1.TaskStateCrashed
	case task.StateFailed:
		return v1.TaskStateFailed
	case task.StatePurged:
		return v1.TaskStatePurged
	default:
		slog.ErrorContext(ctx, "unknown task state", "state", int(s))
		return v1.TaskStatePending
	}
}

// ForgePRState converts forge.PRState to v1.ForgePRState.
func ForgePRState(s forge.PRState) v1.ForgePRState {
	switch s {
	case forge.PRStateOpen:
		return v1.ForgePRStateOpen
	case forge.PRStateClosed:
		return v1.ForgePRStateClosed
	case forge.PRStateMerged:
		return v1.ForgePRStateMerged
	default:
		return ""
	}
}

// SafetyIssues converts task safety issues to API DTOs.
func SafetyIssues(issues []repowork.SafetyIssue) []v1.SafetyIssue {
	if len(issues) == 0 {
		return nil
	}
	out := make([]v1.SafetyIssue, len(issues))
	for i, si := range issues {
		out[i] = v1.SafetyIssue{File: si.File, Kind: si.Kind, Detail: si.Detail}
	}
	return out
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

// ProviderQuota converts a provider quota snapshot to an API DTO.
func ProviderQuota(q *usage.ProviderQuota) v1.ProviderQuota {
	if q == nil {
		return v1.ProviderQuota{}
	}
	out := v1.ProviderQuota{
		Provider:   q.Provider,
		Label:      q.Label,
		AuthKind:   q.AuthKind,
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
	return out
}

// ForgeCheck converts a forge check run to an API DTO.
func ForgeCheck(c *forge.Check) v1.ForgeCheck {
	return v1.ForgeCheck{
		Name:        c.Name,
		Owner:       c.Owner,
		Repo:        c.Repo,
		RunID:       c.RunID,
		JobID:       c.JobID,
		Status:      v1.CheckStatus(c.Status),
		Conclusion:  v1.CheckConclusion(c.Conclusion),
		QueuedAt:    c.QueuedAt,
		StartedAt:   c.StartedAt,
		CompletedAt: c.CompletedAt,
	}
}

// ProcessInfos converts runtime process info to API DTOs.
func ProcessInfos(procs []runtime.ProcessInfo) []v1.ProcessInfo {
	out := make([]v1.ProcessInfo, len(procs))
	for i, p := range procs {
		out[i] = v1.ProcessInfo{
			PID:     p.PID,
			PPID:    p.PPID,
			User:    p.User,
			State:   p.State,
			CPU:     p.CPU,
			Mem:     p.Mem,
			Time:    p.Time,
			Command: p.Command,
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
