// Benchmarks Codex turn-cost pricing while restoring a task timeline.

package task

import (
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func BenchmarkCodexResultPricing(b *testing.B) {
	const model = "gpt-6-sol"
	prices := fakePricer{model: {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10}}
	messages := make([]agent.Message, 100)
	for i := range messages {
		messages[i] = &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{
			InputTokens: 1_000, CacheReadInputTokens: 10_000, OutputTokens: 500,
		}}
	}
	b.ReportAllocs()
	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Codex, model, "")
		tk.Pricer = prices
		tk.SeedTimeline(messages)
	}
}

func BenchmarkCodexRequestPricing(b *testing.B) {
	const model = "gpt-6.1-sol"
	prices := usage.NewPricer(nil)
	messages := make([]agent.Message, 0, 1_100)
	for range 100 {
		for range 10 {
			messages = append(messages, &agent.UsageMessage{PerCall: true, ModelDerived: true,
				ReportedModel: model, Usage: agent.Usage{InputTokens: 50_000, CacheReadInputTokens: 250_000, OutputTokens: 500}})
		}
		messages = append(messages, &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{
			InputTokens: 500_000, CacheReadInputTokens: 2_500_000, OutputTokens: 5_000,
		}})
	}
	b.ReportAllocs()
	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Codex, model, "")
		tk.Pricer = prices
		tk.SeedTimeline(messages)
	}
}
