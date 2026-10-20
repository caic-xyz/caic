// MCP timestamp projections use caller timezones without changing shared API snapshots.

package server

import (
	"slices"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/gomode/mcp"
)

func mcpRequestLocation(meta *mcp.RequestMeta) *time.Location {
	if zone := meta.UserLocation.Timezone; zone != "" && zone != "Local" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc
		}
	}
	return time.UTC
}

func mcpUsageInLocation(usage v1.UsageResp, loc *time.Location) v1.UsageResp {
	usage.Providers = slices.Clone(usage.Providers)
	for i := range usage.Providers {
		p := &usage.Providers[i]
		p.PricingTransitionAt = mcpTimeInLocation(p.PricingTransitionAt, loc)
		p.RateLimits = slices.Clone(p.RateLimits)
		for j := range p.RateLimits {
			p.RateLimits[j].ResetsAt = mcpTimeInLocation(p.RateLimits[j].ResetsAt, loc)
		}
	}
	return usage
}

func mcpTaskInLocation(source *v1.Task, loc *time.Location) v1.Task {
	task := *source
	task.CacheExpiresAt = mcpTimeInLocation(task.CacheExpiresAt, loc)
	task.StartedAt = mcpTimeInLocation(task.StartedAt, loc)
	task.StateUpdatedAt = mcpTimeInLocation(task.StateUpdatedAt, loc)
	task.TurnStartedAt = mcpTimeInLocation(task.TurnStartedAt, loc)
	task.RateLimit.ResetsAt = mcpTimeInLocation(task.RateLimit.ResetsAt, loc)
	task.CIChecks = slices.Clone(task.CIChecks)
	for i := range task.CIChecks {
		c := &task.CIChecks[i]
		c.QueuedAt = mcpTimeInLocation(c.QueuedAt, loc)
		c.StartedAt = mcpTimeInLocation(c.StartedAt, loc)
		c.CompletedAt = mcpTimeInLocation(c.CompletedAt, loc)
	}
	return task
}

func mcpNotificationsInLocation(events []serviceNotification, loc *time.Location) []serviceNotification {
	events = slices.Clone(events)
	for i := range events {
		events[i].OccurredAt = mcpTimeInLocation(events[i].OccurredAt, loc)
		events[i].ExpiresAt = mcpTimeInLocation(events[i].ExpiresAt, loc)
	}
	return events
}

func mcpTimeInLocation(t time.Time, loc *time.Location) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.In(loc)
}

func mcpSystemStatsInLocation(stats v1.SystemStatsResp, loc *time.Location) v1.SystemStatsResp {
	stats.Samples = slices.Clone(stats.Samples)
	for i := range stats.Samples {
		stats.Samples[i].Time = mcpTimeInLocation(stats.Samples[i].Time, loc)
	}
	return stats
}
