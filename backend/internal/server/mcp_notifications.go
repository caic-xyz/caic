// MCP notification feed generates bounded, service-authored alerts for Go Mode clients.

package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type serviceNotification struct {
	ID         string    `json:"id"`
	ItemID     string    `json:"itemId"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	OccurredAt time.Time `json:"occurredAt"`
	ExpiresAt  time.Time `json:"expiresAt"`

	state         v1.TaskState
	quotaRecovery bool
}

type observedTaskState struct {
	state v1.TaskState
	seen  uint64
}

type notificationFeed struct {
	mu sync.Mutex

	blockedTaskIDs map[string]map[string]struct{}
	states         map[string]map[string]observedTaskState
	generations    map[string]uint64
	events         map[string][]serviceNotification
}

func newNotificationFeed() *notificationFeed {
	return &notificationFeed{
		blockedTaskIDs: make(map[string]map[string]struct{}),
		states:         make(map[string]map[string]observedTaskState),
		generations:    make(map[string]uint64),
		events:         make(map[string][]serviceNotification),
	}
}

func (f *notificationFeed) notifications(ctx context.Context, tasks []v1.Task, usage v1.UsageResp) []serviceNotification {
	owner := notificationOwner(ctx)
	now := time.Now().UTC()
	blockedTaskIDs := make(map[string]struct{})
	unknownTaskIDs := make(map[string]struct{})
	taskIndexes := make(map[string]int, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		taskIndexes[task.ID.String()] = i
		if task.State != v1.TaskStateWaiting {
			continue
		}
		blocked, known := taskQuotaBlocked(task, &usage, now)
		if blocked {
			blockedTaskIDs[task.ID.String()] = struct{}{}
		} else if !known {
			unknownTaskIDs[task.ID.String()] = struct{}{}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	previouslyBlocked := f.blockedTaskIDs[owner]
	// Unknown quota neither proves recovery nor erases a prior blocked observation.
	for id := range unknownTaskIDs {
		if wasBlocked(previouslyBlocked, id) {
			blockedTaskIDs[id] = struct{}{}
		}
	}
	previousStates := f.states[owner]
	if previousStates == nil {
		previousStates = make(map[string]observedTaskState, len(tasks))
		f.states[owner] = previousStates
	}
	// The generation also distinguishes occurrence IDs when wall-clock readings repeat.
	generation := f.generations[owner] + 1
	f.generations[owner] = generation
	// The feed is a snapshot of actionable alerts, so task changes withdraw old
	// occurrences before new ones are added. Display numbers follow current order;
	// destinations and occurrence identities always use the stable task ID.
	events := f.events[owner][:0]
	for j := range f.events[owner] {
		event := &f.events[owner][j]
		i, exists := taskIndexes[event.ItemID]
		if !exists || !event.ExpiresAt.After(now) {
			continue
		}
		task := &tasks[i]
		if task.State != event.state {
			continue
		}
		if event.quotaRecovery {
			_, unknown := unknownTaskIDs[event.ItemID]
			if isBlocked(blockedTaskIDs, event.ItemID) || unknown {
				continue
			}
			event.Body = taskTitle(task) + " can continue."
		} else {
			event.Title = taskNotificationTitle(task.State, i+1)
			event.Body = taskTitle(task)
		}
		events = append(events, *event)
	}
	f.events[owner] = events
	for i := range tasks {
		task := &tasks[i]
		id := task.ID.String()
		old := previousStates[id]
		previousStates[id] = observedTaskState{state: task.State, seen: generation}
		if old.seen != 0 && old.state != task.State && taskNotificationTitle(task.State, i+1) != "" {
			f.events[owner] = append(f.events[owner], serviceNotification{
				ID:         fmt.Sprintf("ready-%s-%s-%d", id, now.Format(time.RFC3339Nano), generation),
				ItemID:     id,
				URL:        "/task/@" + id,
				Title:      taskNotificationTitle(task.State, i+1),
				Body:       taskTitle(task),
				OccurredAt: now,
				ExpiresAt:  now.Add(24 * time.Hour),
				state:      task.State,
			})
		}
		if task.State != v1.TaskStateWaiting || !wasBlocked(previouslyBlocked, id) || isBlocked(blockedTaskIDs, id) {
			continue
		}
		f.events[owner] = append(f.events[owner], serviceNotification{
			ID:            fmt.Sprintf("quota-recovered-%s-%s-%d", id, now.Format(time.RFC3339Nano), generation),
			ItemID:        id,
			URL:           "/task/@" + id,
			Title:         "Quota available",
			Body:          taskTitle(task) + " can continue.",
			OccurredAt:    now,
			ExpiresAt:     now.Add(24 * time.Hour),
			state:         task.State,
			quotaRecovery: true,
		})
	}
	for id, observation := range previousStates {
		if observation.seen != generation {
			delete(previousStates, id)
		}
	}
	f.blockedTaskIDs[owner] = blockedTaskIDs
	f.events[owner] = recentNotifications(f.events[owner], now)
	return append(make([]serviceNotification, 0, len(f.events[owner])), f.events[owner]...)
}

func taskNotificationTitle(state v1.TaskState, number int) string {
	switch state {
	case v1.TaskStateWaiting:
		return fmt.Sprintf("Task #%d is waiting", number)
	case v1.TaskStateAsking:
		return fmt.Sprintf("Task #%d has a question", number)
	case v1.TaskStateHasPlan:
		return fmt.Sprintf("Task #%d has a plan ready", number)
	default:
		return ""
	}
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

func taskQuotaBlocked(task *v1.Task, usage *v1.UsageResp, now time.Time) (blocked, known bool) {
	if task.RateLimit.Blocked && task.RateLimit.ResetsAt.After(now) {
		return true, true
	}
	if task.Harness == v1.HarnessAntigravity {
		return antigravityTaskQuotaBlocked(task, usage, now)
	}
	for i := range usage.Providers {
		provider := &usage.Providers[i]
		if !taskUsesProvider(task, agent.QuotaProvider(provider.Provider)) {
			continue
		}
		for _, limit := range provider.RateLimits {
			if limit.Utilization >= 1 && limit.ResetsAt.After(now) {
				return true, true
			}
		}
	}
	return false, true
}

// agy /usage separates Gemini from Claude/GPT-OSS. These are the verified pool
// IDs and model families; unknown models or new bucket IDs do not prove recovery.
func antigravityTaskQuotaBlocked(task *v1.Task, usage *v1.UsageResp, now time.Time) (blocked, known bool) {
	model := task.ReportedModel
	if model == "" {
		model = task.RequestedModel
	}
	var pool string
	switch {
	case strings.HasPrefix(model, "gemini-"):
		pool = "gemini"
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss-"):
		pool = "3p"
	default:
		return false, false
	}
	for i := range usage.Providers {
		provider := &usage.Providers[i]
		if provider.Provider != v1.QuotaProviderAntigravity || provider.FetchStatus != v1.ProviderFetchStatusFresh {
			continue
		}
		var weekly, fiveHour, expired, unknown bool
		for _, window := range provider.UnassessedWindows {
			switch window.Window {
			case "gemini-weekly", "gemini-5h":
				unknown = unknown || pool == "gemini"
			case "3p-weekly", "3p-5h":
				unknown = unknown || pool == "3p"
			default:
				unknown = true // A new or missing ID cannot rule out a matching pool limit.
			}
		}
		for _, limit := range provider.RateLimits {
			switch limit.Window {
			case pool + "-weekly":
				weekly = true
			case pool + "-5h":
				fiveHour = true
			case "gemini-weekly", "gemini-5h", "3p-weekly", "3p-5h":
				continue // A verified bucket in the other pool.
			default:
				unknown = true
				continue
			}
			if limit.Utilization >= 1 {
				if limit.ResetsAt.IsZero() || limit.ResetsAt.After(now) {
					return true, true
				}
				expired = true
			}
		}
		return false, weekly && fiveHour && !expired && !unknown
	}
	return false, false
}

func taskUsesProvider(task *v1.Task, provider agent.QuotaProvider) bool {
	candidates := map[agent.QuotaProvider]struct{}{}
	switch task.Harness {
	case v1.HarnessClaude:
		addProviderCandidate(candidates, agent.QuotaProviderAnthropic)
		addProviderCandidate(candidates, agent.QuotaProviderClaudeCode)
	case v1.HarnessCodex:
		addProviderCandidate(candidates, agent.QuotaProviderCodex)
	case v1.HarnessAntigravity:
		return provider == agent.QuotaProviderAntigravity
	case v1.HarnessOpenCode, v1.HarnessPi:
		// These harnesses select the billing provider through the task model.
	}
	addModelProviderCandidate(candidates, task.ReportedModel)
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
