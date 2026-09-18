// Benchmarks for the Codex wire parser and its native-subagent adapter.

package codex

import "testing"

// BenchmarkParseNativeSubagentLines measures the per-line cost the stateful
// adapter adds to every app-server notification: an unrelated item, a
// collaboration spawn with receivers, and a coordination item whose agent
// states settle them. Repeating a record on one wire measures the steady-state
// line cost.
func BenchmarkParseNativeSubagentLines(b *testing.B) {
	for _, test := range []struct {
		name string
		line string
	}{
		{
			name: "unrelated_notification",
			line: `{"method":"turn/completed","params":{"threadId":"t","turnId":"u"},"emittedAtMs":1}`,
		},
		{
			name: "spawn",
			line: `{"method":"item/started","params":{"item":{"id":"item1","type":"collabAgentToolCall","tool":"spawnAgent","status":"inProgress","senderThreadId":"parent","receiverThreadIds":["th1"],"prompt":"Do work","agentsStates":{}},"threadId":"parent"}}`,
		},
		{
			name: "agent_states",
			line: `{"method":"item/completed","params":{"item":{"id":"item2","type":"collabAgentToolCall","tool":"wait","status":"completed","senderThreadId":"parent","receiverThreadIds":["th1"],"prompt":null,"agentsStates":{"th1":{"status":"completed","message":"done"}}},"threadId":"parent"}}`,
		},
	} {
		b.Run(test.name, func(b *testing.B) {
			line := []byte(test.line)
			wire := New("", nil).NewWire()
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			b.ResetTimer()
			for range b.N {
				if _, err := wire.ParseMessage(line); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
