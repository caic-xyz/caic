// Tests reported and estimated turn costs across task and compaction boundaries.

package task

import (
	"math"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func TestCostTracker(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	prices := fakePricer{
		"gpt-6-sol":  {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10},
		"gpt-6-luna": {InputPerMTok: 0.1},
	}

	t.Run("PiUnpricedInvocationsAccumulateAcrossBoundaries", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "unknown"}
		pricer := usage.NewPricer(nil)
		for i, boundary := range []agent.Message{nil, nil, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, agent.ContextCleared()} {
			if boundary != nil {
				c.Observe(boundary, at, pricer)
			}
			cost, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.25}, at, pricer)
			if !ok || cost.USD != 0.25 || cost.Source != CostReported || c.TotalUSD != float64(i+1)*0.25 {
				t.Fatalf("invocation %d: cost = %+v, ok = %v, total = %v", i, cost, ok, c.TotalUSD)
			}
		}
	})

	t.Run("PiReportedAndPricedInvocationsAccumulate", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "unknown"}
		c.Observe(&agent.ResultMessage{TotalCostUSD: 0.25}, at, prices)
		c.Observe(&agent.UsageMessage{ReportedModel: "gpt-6-sol", Usage: agent.Usage{InputTokens: 1_000_000}}, at, prices)
		priced, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.125}, at, prices)
		if !ok || priced.USD != 2 || priced.Source != CostEstimated || c.TotalUSD != 2.25 {
			t.Fatalf("priced = %+v, ok = %v, total = %v", priced, ok, c.TotalUSD)
		}
		c.Observe(&agent.UsageMessage{ReportedModel: "unknown", Usage: agent.Usage{InputTokens: 1_000_000}}, at, prices)
		reported, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.5}, at, prices)
		if !ok || reported.USD != 0.5 || reported.Source != CostReported || c.TotalUSD != 2.75 {
			t.Fatalf("reported = %+v, ok = %v, total = %v", reported, ok, c.TotalUSD)
		}
	})

	t.Run("PiMixedCallsWithinInvocation", func(t *testing.T) {
		t.Parallel()
		for _, unknownFirst := range []bool{false, true} {
			c := CostTracker{Harness: harness.Pi, Model: "gpt-6-sol"}
			reported := 0.5
			pricedCall := &agent.UsageMessage{ReportedModel: "gpt-6-sol", Usage: agent.Usage{InputTokens: 1_000_000}}
			unknownCall := &agent.UsageMessage{ReportedModel: "unknown", Usage: agent.Usage{InputTokens: 1_000_000}, ReportedCostUSD: &reported}
			calls := []*agent.UsageMessage{pricedCall, unknownCall}
			if unknownFirst {
				calls[0], calls[1] = calls[1], calls[0]
			}
			for _, call := range calls {
				c.Observe(call, at, prices)
			}
			cost, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 2.5}, at, prices)
			if !ok || cost.USD != 2.5 || cost.Source != CostEstimated || c.TotalUSD != 2.5 {
				t.Fatalf("unknownFirst %v: cost = %+v, ok = %v, total = %v", unknownFirst, cost, ok, c.TotalUSD)
			}
		}
	})

	t.Run("PiReportedCallsAreNotCountedAgainAtResult", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "unknown"}
		for _, usd := range []float64{0.25, 0.5} {
			c.Observe(&agent.UsageMessage{ReportedCostUSD: &usd}, at, nil)
		}
		cost, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.75}, at, nil)
		if !ok || cost.USD != 0.75 || cost.Source != CostReported || c.TotalUSD != 0.75 {
			t.Fatalf("cost = %+v, ok = %v, total = %v", cost, ok, c.TotalUSD)
		}
	})

	t.Run("CodexTurnsAcrossCompactionAndReroute", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Codex, Model: "gpt-6-luna"}
		c.Observe(&agent.InitMessage{ReportedModel: "gpt-6-sol"}, at, prices)
		firstRaw := &agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 1_000_000, OutputTokens: 10_000}}
		first, ok := c.Observe(firstRaw, at, prices)
		if !ok || first.USD != 0.5 || first.Source != CostEstimated || firstRaw.TotalCostUSD != 0 {
			t.Fatalf("first = %+v, ok = %v, raw cost = %v", first, ok, firstRaw.TotalCostUSD)
		}
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at, prices)
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeModelRerouted, ReportedModel: "gpt-6-luna"}, at, prices)
		second, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 1_000_000}}, at, prices)
		if !ok || math.Abs(second.USD-0.1) > 1e-9 || second.Source != CostEstimated || c.TotalUSD != 0.6 {
			t.Errorf("second = %+v, ok = %v, total = %v", second, ok, c.TotalUSD)
		}
	})

	t.Run("ClaudeCumulativeCostBecomesTurnDeltas", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Claude}
		first, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 2}, at, nil)
		if !ok || first.USD != 2 || first.Source != CostReported {
			t.Fatalf("first = %+v, ok = %v", first, ok)
		}
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at, nil)
		second, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 3}, at, nil)
		if !ok || second.USD != 1 || c.TotalUSD != 3 {
			t.Errorf("second = %+v, ok = %v, total = %v", second, ok, c.TotalUSD)
		}
		c.Observe(agent.ContextCleared(), at, nil)
		third, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.5}, at, nil)
		if !ok || third.USD != 0.5 || c.TotalUSD != 3.5 {
			t.Errorf("third = %+v, ok = %v, total = %v", third, ok, c.TotalUSD)
		}
	})

	t.Run("OpenCodeCumulativeSnapshotBecomesTurnDelta", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.OpenCode, Model: "gpt-6-sol"}
		firstCost, secondCost := 0.4, 0.75
		c.Observe(&agent.UsageMessage{CumulativeCostUSD: &firstCost}, at, prices)
		first, ok := c.Observe(&agent.ResultMessage{}, at, prices)
		if !ok || first.USD != 0.4 || first.Source != CostReported {
			t.Fatalf("first = %+v, ok = %v", first, ok)
		}
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at, prices)
		c.Observe(&agent.UsageMessage{CumulativeCostUSD: &secondCost}, at, prices)
		second, ok := c.Observe(&agent.ResultMessage{}, at, prices)
		if !ok || math.Abs(second.USD-0.35) > 1e-9 || c.TotalUSD != 0.75 {
			t.Errorf("second = %+v, ok = %v, total = %v", second, ok, c.TotalUSD)
		}
	})

	t.Run("UnpricedCodexTurnIsUnknown", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Codex, Model: "unknown"}
		cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 1_000}}, at, usage.NewPricer(nil))
		if ok || cost.USD != 0 || c.TotalUSD != 0 {
			t.Errorf("cost = %+v, ok = %v, total = %v", cost, ok, c.TotalUSD)
		}
	})

	t.Run("PiPerCallPricingTakesPrecedenceOverLastReportedCall", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "gpt-6-sol"}
		c.Observe(&agent.UsageMessage{ReportedModel: "gpt-6-sol", Usage: agent.Usage{InputTokens: 1_000_000}}, at, prices)
		cost, ok := c.Observe(&agent.ResultMessage{TotalCostUSD: 0.01}, at, prices)
		if !ok || cost.USD != 2 || cost.Source != CostEstimated || c.TotalUSD != 2 {
			t.Errorf("cost = %+v, ok = %v, total = %v", cost, ok, c.TotalUSD)
		}
	})

	t.Run("OpenCodeZeroSnapshotUsesTurnPrice", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.OpenCode, Model: "gpt-6-sol"}
		zero := 0.0
		c.Observe(&agent.UsageMessage{CumulativeCostUSD: &zero}, at, prices)
		cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 1_000_000}}, at, prices)
		if !ok || cost.USD != 2 || cost.Source != CostEstimated || c.TotalUSD != 2 {
			t.Errorf("cost = %+v, ok = %v, total = %v", cost, ok, c.TotalUSD)
		}
	})
}
