// Tests for backend-generated Go Mode notification events.

package server

import (
	"testing"
	"time"

	"github.com/maruel/ksid"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestNotificationFeed(t *testing.T) {
	t.Parallel()
	feed := newNotificationFeed()
	task := v1.Task{
		ID:      ksid.NewID(),
		Title:   "Build feature",
		State:   v1.TaskStateWaiting,
		Harness: v1.HarnessClaude,
	}
	now := time.Now().UTC()
	blockedUsage := v1.UsageResp{Providers: []v1.ProviderQuota{{
		Provider: "anthropic",
		RateLimits: []v1.QuotaRateLimit{{
			UsedPct:  100,
			ResetsAt: now.Add(time.Hour),
		}},
	}}}
	availableUsage := v1.UsageResp{Providers: []v1.ProviderQuota{{
		Provider: "anthropic",
		RateLimits: []v1.QuotaRateLimit{{
			UsedPct:  42,
			ResetsAt: now.Add(time.Hour),
		}},
	}}}

	if got := feed.notifications(t.Context(), []v1.Task{task}, blockedUsage); len(got) != 0 {
		t.Fatalf("initial notifications = %#v, want none", got)
	}
	got := feed.notifications(t.Context(), []v1.Task{task}, availableUsage)
	if len(got) != 1 {
		t.Fatalf("notifications = %#v, want one", got)
	}
	if got[0].Title != "Quota available" || got[0].Body != "Build feature can continue." {
		t.Fatalf("notification = %#v", got[0])
	}
	if got := feed.notifications(t.Context(), []v1.Task{task}, availableUsage); len(got) != 1 {
		t.Fatalf("notifications after repeated availability = %#v, want one retained event", got)
	}
}
