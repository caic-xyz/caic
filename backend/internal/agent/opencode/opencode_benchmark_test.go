// Benchmarks parsing OpenCode ACP cost and context usage updates.

package opencode

import "testing"

func BenchmarkWireFormatUsageUpdate(b *testing.B) {
	w := &wireFormat{sessionID: "ses_1"}
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"usage_update","used":45000,"size":200000,"cost":{"amount":0.42,"currency":"USD"}}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := w.ParseMessage(line); err != nil {
			b.Fatal(err)
		}
	}
}
