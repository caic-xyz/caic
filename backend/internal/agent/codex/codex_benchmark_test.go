// Benchmarks for stateful Codex app-server usage-event parsing.

package codex

import "testing"

func BenchmarkWireFormatTokenUsageUpdated(b *testing.B) {
	w := &wireFormat{}
	line := []byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"turn_1","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"cachedInputTokens":500,"cacheWriteInputTokens":100,"outputTokens":200,"reasoningOutputTokens":50},"last":{"totalTokens":100,"inputTokens":80,"cachedInputTokens":50,"cacheWriteInputTokens":12,"outputTokens":20,"reasoningOutputTokens":5},"modelContextWindow":258400}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := w.ParseMessage(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseAccountRateLimitsUpdated(b *testing.B) {
	line := []byte(`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1735689720.5},"secondary":{"usedPercent":85,"windowDurationMins":10080,"resetsAt":1736294520},"rateLimitReachedType":"rate_limit_reached"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := parseMessage(line); err != nil {
			b.Fatal(err)
		}
	}
}
