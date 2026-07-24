// MCP notification feed generates bounded, service-authored alerts for Go Mode clients.

package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type serviceNotification struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	OccurredAt time.Time `json:"occurredAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type notificationFeed struct {
	mu sync.Mutex

	blockedTaskIDs map[string]map[string]struct{}
	events         map[string][]serviceNotification
}

func newNotificationFeed() *notificationFeed {
	return &notificationFeed{
		blockedTaskIDs: make(map[string]map[string]struct{}),
		events:         make(map[string][]serviceNotification),
	}
}

func (f *notificationFeed) notifications(ctx context.Context, tasks []v1.Task, usage v1.UsageResp) []serviceNotification {
	owner := notificationOwner(ctx)
	now := time.Now().UTC()
	blockedTaskIDs := make(map[string]struct{})
	for i := range tasks {
		task := &tasks[i]
		if task.State == v1.TaskStateWaiting && taskQuotaBlocked(task, &usage, now) {
			blockedTaskIDs[task.ID.String()] = struct{}{}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	previouslyBlocked := f.blockedTaskIDs[owner]
	for i := range tasks {
		task := &tasks[i]
		id := task.ID.String()
		if task.State != v1.TaskStateWaiting || !wasBlocked(previouslyBlocked, id) || isBlocked(blockedTaskIDs, id) {
			continue
		}
		f.events[owner] = append(f.events[owner], serviceNotification{
			ID:         "quota-recovered-" + id + "-" + now.Format(time.RFC3339Nano),
			Title:      "Quota available",
			Body:       task.Title + " can continue.",
			OccurredAt: now,
			ExpiresAt:  now.Add(24 * time.Hour),
		})
	}
	f.blockedTaskIDs[owner] = blockedTaskIDs
	f.events[owner] = recentNotifications(f.events[owner], now)
	return append(make([]serviceNotification, 0, len(f.events[owner])), f.events[owner]...)
}

func notificationOwner(ctx context.Context) string {
	if user, ok := auth.UserFromContext(ctx); ok {
		return user.ID
	}
	return "local"
}

func recentNotifications(events []serviceNotification, now time.Time) []serviceNotification {
	const maxEvents = 100
	start := 0
	for start < len(events) && !events[start].ExpiresAt.After(now) {
		start++
	}
	events = events[start:]
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events
}

func wasBlocked(taskIDs map[string]struct{}, id string) bool {
	_, ok := taskIDs[id]
	return ok
}

func isBlocked(taskIDs map[string]struct{}, id string) bool {
	_, ok := taskIDs[id]
	return ok
}

func taskQuotaBlocked(task *v1.Task, usage *v1.UsageResp, now time.Time) bool {
	if task.RateLimit.Blocked && task.RateLimit.ResetsAt.After(now) {
		return true
	}
	for i := range usage.Providers {
		provider := &usage.Providers[i]
		if !taskUsesProvider(task, agent.QuotaProvider(provider.Provider)) {
			continue
		}
		for _, limit := range provider.RateLimits {
			if limit.UsedPct >= 100 && limit.ResetsAt.After(now) {
				return true
			}
		}
	}
	return false
}

func taskUsesProvider(task *v1.Task, provider agent.QuotaProvider) bool {
	candidates := map[agent.QuotaProvider]struct{}{}
	switch task.Harness {
	case v1.HarnessClaude:
		addProviderCandidate(candidates, agent.QuotaProviderAnthropic)
		addProviderCandidate(candidates, agent.QuotaProviderClaudeCode)
	case v1.HarnessCodex:
		addProviderCandidate(candidates, agent.QuotaProviderCodex)
	case v1.HarnessOpenCode, v1.HarnessPi:
		// These harnesses select the billing provider through the task model.
	}
	addModelProviderCandidate(candidates, task.Model)
	_, ok := candidates[provider]
	return ok
}

func addModelProviderCandidate(candidates map[agent.QuotaProvider]struct{}, model string) {
	if model == "" {
		return
	}
	if provider, _, ok := strings.Cut(model, "/"); ok {
		addProviderCandidate(candidates, agent.QuotaProvider(provider))
		return
	}
	if provider, _, ok := strings.Cut(model, ":"); ok {
		addProviderCandidate(candidates, agent.QuotaProvider(provider))
		return
	}
	switch {
	case strings.HasPrefix(model, "claude-"):
		addProviderCandidate(candidates, agent.QuotaProviderAnthropic)
	case strings.HasPrefix(model, "deepseek"):
		addProviderCandidate(candidates, agent.QuotaProviderDeepSeek)
	case strings.Contains(model, "mimo"):
		addProviderCandidate(candidates, agent.QuotaProviderXiaomi)
	}
}

func addProviderCandidate(candidates map[agent.QuotaProvider]struct{}, value agent.QuotaProvider) {
	value = agent.QuotaProvider(strings.ToLower(strings.TrimSpace(string(value))))
	if value.Valid() {
		candidates[value] = struct{}{}
	}
}
