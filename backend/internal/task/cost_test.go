// Tests reported and estimated turn costs across task and compaction boundaries.

package task

import (
	"math"
	"strings"
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

	t.Run("OpenAIRequestsAcrossHarnesses", func(t *testing.T) {
		t.Parallel()
		for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
			for _, model := range []string{"openai/gpt-6.1-sol", "openai-codex/gpt-6.1-sol", "openai/gpt-6.1-terra", "openai-codex/gpt-6.1-terra"} {
				t.Run(string(h)+"/"+model, func(t *testing.T) {
					t.Parallel()
					pricer := usage.NewPricer(nil)
					c := CostTracker{Harness: h, Model: model}
					long := agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 200_000, OutputTokens: 1_000}
					short := agent.Usage{InputTokens: 100_000, OutputTokens: 1_000}
					c.Observe(&agent.UsageMessage{PerCall: true, ReportedModel: model, Usage: long}, at, pricer)
					for range 3 {
						c.Observe(&agent.UsageMessage{PerCall: true, ReportedModel: model, Usage: short}, at, pricer)
					}
					result := agent.Usage{InputTokens: 400_000, CacheReadInputTokens: 200_000, OutputTokens: 4_000}
					if h == harness.Pi {
						result = short // Pi repeats only the last request in its result.
					}
					cost, ok := c.Observe(&agent.ResultMessage{Usage: result}, at, pricer)
					want := 1.085
					if strings.HasSuffix(model, "terra") {
						want = 0.5425
					}
					if !ok || cost.Source != CostEstimated || math.Abs(cost.USD-want) > 1e-9 {
						t.Fatalf("cost = %+v/%v, want %v", cost, ok, want)
					}
				})
			}
		}
	})

	t.Run("PiAggregateSummaryRetainsBaseRates", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "openai-codex/gpt-6.1-sol"}
		pricer := usage.NewPricer(nil)
		c.Observe(&agent.UsageMessage{Usage: agent.Usage{InputTokens: 300_000}}, at, pricer)
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at, pricer)
		cost, ok := c.Observe(&agent.ResultMessage{}, at, pricer)
		if !ok || math.Abs(cost.USD-0.6) > 1e-9 {
			t.Fatalf("aggregate summary cost = %+v/%v, want 0.6", cost, ok)
		}
	})

	t.Run("PiHistoricalRequestStillUsesContextTier", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.Pi, Model: "openai-codex/gpt-6.1-sol"}
		pricer := usage.NewPricer(nil)
		c.Observe(&agent.UsageMessage{ReportedModel: "openai-codex/gpt-6.1-sol", Usage: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 200_000, OutputTokens: 1_000}}, at, pricer)
		cost, ok := c.Observe(&agent.ResultMessage{}, at, pricer)
		if !ok || math.Abs(cost.USD-0.455) > 1e-9 {
			t.Fatalf("historical request cost = %+v/%v, want 0.455", cost, ok)
		}
	})

	t.Run("OpenCodeReportedOpenAICostSupersedesRequests", func(t *testing.T) {
		t.Parallel()
		c := CostTracker{Harness: harness.OpenCode, Model: "openai-codex/gpt-6.1-sol"}
		pricer := usage.NewPricer(nil)
		call := &agent.UsageMessage{PerCall: true, ReportedModel: c.Model, Usage: agent.Usage{InputTokens: 300_000}}
		c.Observe(call, at, pricer)
		reported := 0.5
		c.Observe(&agent.UsageMessage{CumulativeCostUSD: &reported}, at, pricer)
		c.Observe(call, at, pricer) // Once reported, additional calls await a new snapshot.
		cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 600_000}}, at, pricer)
		if !ok || cost.Source != CostReported || cost.USD != 0.5 || c.TotalUSD != 0.5 {
			t.Fatalf("reported cost = %+v/%v, total = %v", cost, ok, c.TotalUSD)
		}
	})
	t.Run("OpenCodeAggregateOpenAIUsageKeepsBaseRates", func(t *testing.T) {
		t.Parallel()
		for _, model := range []string{"openai/gpt-6.1-sol", "openai-codex/gpt-6.1-sol"} {
			c := CostTracker{Harness: harness.OpenCode, Model: model}
			cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 400_000, CacheReadInputTokens: 100_000, OutputTokens: 1_000}}, at, usage.NewPricer(nil))
			if !ok || math.Abs(cost.USD-0.82) > 1e-9 || cost.Source != CostEstimated {
				t.Fatalf("%s aggregate cost = %+v/%v, want 0.82 estimated", model, cost, ok)
			}
		}
	})

	t.Run("CodexRequestContextPricing", func(t *testing.T) {
		t.Parallel()
		pricer := usage.NewPricer(nil)
		for _, tc := range []struct {
			name  string
			calls []*agent.UsageMessage
			total agent.Usage
			want  float64
		}{
			{
				name: "three short requests exceed threshold in aggregate",
				calls: []*agent.UsageMessage{
					{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000, OutputTokens: 1_000}},
					{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000, OutputTokens: 1_000}},
					{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000, OutputTokens: 1_000}},
				},
				total: agent.Usage{InputTokens: 300_000, OutputTokens: 3_000}, want: 0.63,
			},
			{
				name: "long request includes cache reads and writes",
				calls: []*agent.UsageMessage{
					{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 200_000, CacheCreationInputTokens: 1_000, OutputTokens: 1_000}},
				},
				total: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 200_000, CacheCreationInputTokens: 1_000, OutputTokens: 1_000}, want: 0.46,
			},
			{
				name: "legacy turn totals retain base rate",
				calls: []*agent.UsageMessage{
					{ModelDerived: true, Usage: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}},
				},
				total: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}, want: 0.61,
			},
			{
				name: "missing requests price only residual at base rate",
				calls: []*agent.UsageMessage{
					{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}},
				},
				total: agent.Usage{InputTokens: 400_000, OutputTokens: 2_000}, want: 1.425,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				c := CostTracker{Harness: harness.Codex, Model: "gpt-6.1-sol"}
				for _, call := range tc.calls {
					c.Observe(call, at, pricer)
				}
				cost, ok := c.Observe(&agent.ResultMessage{Usage: tc.total}, at, pricer)
				if !ok || math.Abs(cost.USD-tc.want) > 1e-9 || cost.Source != CostEstimated {
					t.Fatalf("cost = %+v/%v, want %v estimated", cost, ok, tc.want)
				}
				// The next turn must not subtract the preceding turn's requests.
				cost, ok = c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
				if !ok || math.Abs(cost.USD-0.2) > 1e-9 || math.Abs(c.TotalUSD-tc.want-0.2) > 1e-9 {
					t.Fatalf("next cost = %+v/%v, total = %v", cost, ok, c.TotalUSD)
				}
			})
		}
	})

	t.Run("CodexSessionBoundaryResetsRequestCoverage", func(t *testing.T) {
		t.Parallel()
		for _, boundary := range []agent.Message{agent.ContextCleared(), &agent.ExitMessage{}, &agent.InitMessage{SessionID: "next"}} {
			c := CostTracker{Harness: harness.Codex, Model: "gpt-6.1-sol"}
			pricer := usage.NewPricer(nil)
			c.Observe(&agent.UsageMessage{PerCall: true, Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
			c.Observe(boundary, at, pricer)
			c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
			if math.Abs(c.TotalUSD-0.4) > 1e-9 {
				t.Fatalf("total after %T = %v, want 0.4", boundary, c.TotalUSD)
			}
		}
	})

	t.Run("CodexSessionStartResetsCoverageAfterFilteredExit", func(t *testing.T) {
		t.Parallel()
		for _, filteredExit := range []bool{false, true} {
			c := CostTracker{Harness: harness.Codex, Model: "gpt-6.1-sol"}
			pricer := usage.NewPricer(nil)
			c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
			c.Observe(&agent.UsageMessage{PerCall: true, ReportedModel: "gpt-6.1-terra",
				Source: agent.MessageSource{ThreadID: "child", RootThreadID: "root"}, Usage: agent.Usage{InputTokens: 300_000}}, at, pricer)
			if !filteredExit {
				c.Observe(&agent.ExitMessage{ExitCode: 1}, at, pricer)
			}
			c.Observe(&agent.InitMessage{SessionID: "next"}, at, pricer)
			cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
			if !ok || math.Abs(c.TotalUSD-1) > 1e-9 || math.Abs(cost.USD-0.8) > 1e-9 {
				t.Fatalf("filtered exit %v: cost=%+v/%v, total=%v", filteredExit, cost, ok, c.TotalUSD)
			}
		}
	})

	t.Run("CodexPricedChildDoesNotEstablishUnpricedParentCost", func(t *testing.T) {
		t.Parallel()
		for _, parentInput := range []int{0, 100_000} {
			c := CostTracker{Harness: harness.Codex, Model: "unknown"}
			pricer := usage.NewPricer(nil)
			c.Observe(&agent.UsageMessage{PerCall: true, ModelDerived: true, ReportedModel: "gpt-6.1-terra",
				Source: agent.MessageSource{ThreadID: "child", RootThreadID: "root"},
				Usage:  agent.Usage{InputTokens: 300_000}}, at, pricer)
			cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 300_000 + parentInput}}, at, pricer)
			if ok != (parentInput == 0) || (parentInput > 0 && cost.Source != "") || math.Abs(c.TotalUSD-0.6) > 1e-9 {
				t.Fatalf("parent input %d: cost=%+v/%v, total=%v", parentInput, cost, ok, c.TotalUSD)
			}
		}
	})

	t.Run("CodexChildModelsReroutingAndMidTurnCompaction", func(t *testing.T) {
		t.Parallel()
		pricer := usage.NewPricer(nil)
		c := CostTracker{Harness: harness.Codex, Model: "gpt-6.1-sol"}
		c.Observe(&agent.UsageMessage{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
		child := agent.MessageSource{ThreadID: "child", RootThreadID: "root"}
		c.Observe(&agent.UsageMessage{PerCall: true, ModelDerived: true, ReportedModel: "gpt-6.1-terra", Source: child, Usage: agent.Usage{InputTokens: 300_000}}, at, pricer)
		if c.Model != "gpt-6.1-sol" {
			t.Fatalf("child changed parent model to %s", c.Model)
		}
		if _, ok := c.Observe(&agent.ResultMessage{Source: child, Usage: agent.Usage{InputTokens: 300_000}}, at, pricer); ok {
			t.Fatal("child result ended root turn")
		}
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at, pricer)
		c.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeModelRerouted, ReportedModel: "gpt-6.1-terra"}, at, pricer)
		c.Observe(&agent.UsageMessage{PerCall: true, ModelDerived: true, Usage: agent.Usage{InputTokens: 100_000}}, at, pricer)
		cost, ok := c.Observe(&agent.ResultMessage{Usage: agent.Usage{InputTokens: 500_000}}, at, pricer)
		if !ok || math.Abs(cost.USD-0.9) > 1e-9 || math.Abs(c.TotalUSD-0.9) > 1e-9 {
			t.Fatalf("cost = %+v/%v, total = %v, want 0.9", cost, ok, c.TotalUSD)
		}
	})

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
