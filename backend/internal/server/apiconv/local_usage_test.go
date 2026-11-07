// Tests local rolling usage windows, known zero costs, and incomplete cost subtotals.

package apiconv

import (
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestLocalUsage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	t.Run("WindowCompleteness", func(t *testing.T) {
		t.Parallel()
		got := LocalUsage([]LocalUsageInput{
			{StartedAt: now.Add(-time.Minute), CostComplete: true, Usage: agent.Usage{InputTokens: 2, CacheReadInputTokens: 3, OutputTokens: 4}},
			{StartedAt: now.Add(-2 * time.Hour), Usage: agent.Usage{InputTokens: 5}},
			{StartedAt: now.Add(-7 * time.Hour), CostUSD: 0.25, CostComplete: true, Usage: agent.Usage{OutputTokens: 6}},
			{StartedAt: now.Add(-25 * time.Hour)},
			{},
		}, now)
		want := v1.LocalUsage{Windows: []v1.LocalWindow{
			{Duration: "1h", CostKnown: true, InputTokens: 5, OutputTokens: 4},
			{Duration: "6h", CostKnown: false, InputTokens: 10, OutputTokens: 4},
			{Duration: "24h", CostUSD: 0.25, CostKnown: false, InputTokens: 10, OutputTokens: 10},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("local usage = %+v; want %+v", got, want)
		}
	})
	t.Run("Empty", func(t *testing.T) {
		t.Parallel()
		got := LocalUsage(nil, now)
		want := v1.LocalUsage{Windows: []v1.LocalWindow{
			{Duration: "1h", CostKnown: true},
			{Duration: "6h", CostKnown: true},
			{Duration: "24h", CostKnown: true},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("empty usage = %+v; want %+v", got, want)
		}
	})
}
