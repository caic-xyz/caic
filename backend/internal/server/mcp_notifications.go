// MCP notification feed generates bounded, service-authored alerts for Go Mode clients.

package server

import (
	"context"
	"strings"
	"sync"
	"time"

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
	return append([]serviceNotification(nil), f.events[owner]...)
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
	for i := range usage.Providers {
		provider := &usage.Providers[i]
		if !taskUsesProvider(task, provider.Provider) {
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

func taskUsesProvider(task *v1.Task, provider string) bool {
	candidates := map[string]struct{}{}
	addProviderCandidate(candidates, string(task.Harness))
	switch task.Harness {
	case v1.HarnessClaude:
		addProviderCandidate(candidates, "anthropic")
	case v1.HarnessCodex:
		addProviderCandidate(candidates, "codex")
		addProviderCandidate(candidates, "openai")
	case v1.HarnessOpenCode:
		addProviderCandidate(candidates, "opencode")
	case v1.HarnessPi:
		addProviderCandidate(candidates, "pi")
	}
	model := strings.ToLower(strings.TrimSpace(task.Model))
	if model != "" {
		addProviderCandidate(candidates, strings.FieldsFunc(model, func(r rune) bool { return r == '/' || r == ':' })[0])
		switch {
		case strings.HasPrefix(model, "claude-") || strings.Contains(model, "/claude-"):
			addProviderCandidate(candidates, "anthropic")
		case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
			addProviderCandidate(candidates, "openai")
		case strings.HasPrefix(model, "deepseek"):
			addProviderCandidate(candidates, "deepseek")
		case strings.HasPrefix(model, "gemini"):
			addProviderCandidate(candidates, "gemini")
		case strings.Contains(model, "mimo"):
			addProviderCandidate(candidates, "xiaomi")
		}
	}
	_, ok := candidates[strings.ToLower(provider)]
	return ok
}

func addProviderCandidate(candidates map[string]struct{}, value string) {
	if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
		candidates[value] = struct{}{}
	}
}
