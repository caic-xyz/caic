// Task conversions from backend task entries to API DTOs.

package apiconv

import (
	"fmt"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// TaskInput is the complete server-resolved task state needed for an API DTO.
type TaskInput struct {
	Task               *task.Task
	Snapshot           task.Snapshot
	Result             *taskslog.Result
	Repos              []v1.TaskRepo
	SudoPassword       string
	ContextWindowLimit int
	Owner              string
}

// Task converts complete server-resolved task state to its API DTO.
func Task(in *TaskInput) (v1.Task, error) {
	t := in.Task
	snap := in.Snapshot
	result := in.Result

	// For purged tasks loaded from logs, snap stats may be zero because messages
	// aren't restored. Fall back to the result trailer.
	costUSD := snap.CostUSD
	numTurns := snap.NumTurns
	duration := snap.Duration
	cumulativeUsage := snap.Usage
	if result != nil {
		if costUSD == 0 {
			costUSD = result.CostUSD
		}
		if numTurns == 0 {
			numTurns = result.NumTurns
		}
		if duration == 0 {
			duration = result.Duration
		}
		if cumulativeUsage == (agent.Usage{}) {
			cumulativeUsage = result.Usage
		}
	}
	state, err := TaskState(snap.State)
	if err != nil {
		return v1.Task{}, fmt.Errorf("task %s state: %w", t.ID, err)
	}
	harnessName, err := Harness(t.Harness)
	if err != nil {
		return v1.Task{}, fmt.Errorf("task %s harness: %w", t.ID, err)
	}
	prState, err := ForgePRState(snap.ForgePRState)
	if err != nil {
		return v1.Task{}, fmt.Errorf("task %s forge PR state: %w", t.ID, err)
	}
	ciStatus, err := CIStatus(snap.CIStatus)
	if err != nil {
		return v1.Task{}, fmt.Errorf("task %s CI status: %w", t.ID, err)
	}

	out := v1.Task{
		ID:               t.ID,
		InitialPrompt:    t.InitialPrompt.Text,
		Title:            snap.Title,
		ForkedFromTaskID: t.ForkedFromTaskID,
		Repos:            in.Repos,
		Runtime: v1.RuntimeInstance{
			ID:           string(snap.RuntimeInstanceID),
			RuntimeName:  string(snap.RuntimeName),
			Tailscale:    tailscaleURLFromSnapshot(&snap),
			USB:          snap.USB,
			Display:      snap.Display,
			Sudo:         snap.Sudo,
			SudoPassword: in.SudoPassword,
			VNCPort:      snap.VNCPort,
		},
		State:          state,
		StateUpdatedAt: snap.StateUpdatedAt,
		Harness:        harnessName,
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
	} else {
		out.ContextWindowLimit = in.ContextWindowLimit
	}
	if result != nil {
		out.DiffStat = DiffStat(result.DiffStat)
		out.Result = result.AgentResult
		if result.Err != nil {
			out.Error = result.Err.Error()
		}
	} else {
		out.DiffStat = DiffStat(snap.DiffStat)
	}
	out.ForgeOwner = snap.ForgeOwner
	out.ForgeRepo = snap.ForgeRepo
	out.ForgePR = snap.ForgePR
	out.ForgePRState = prState
	out.ForgeIssue = snap.ForgeIssue
	out.CIStatus = ciStatus
	if len(snap.CIChecks) > 0 {
		out.CIChecks = make([]v1.ForgeCheck, len(snap.CIChecks))
		for i := range snap.CIChecks {
			check, err := ForgeCheck(&snap.CIChecks[i])
			if err != nil {
				return v1.Task{}, fmt.Errorf("task %s CI check %d: %w", t.ID, i, err)
			}
			out.CIChecks[i] = check
		}
	}
	out.Owner = in.Owner
	return out, nil
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
