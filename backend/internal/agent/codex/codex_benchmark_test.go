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
