// Benchmarks for the Codex wire parser and its native-subagent adapter.

package codex

import (
	"bytes"
	"os"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

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

// BenchmarkParseStreamingDelta covers the frequent small text and thinking
// records at the live wire boundary, including stateful dispatch overhead.
func BenchmarkParseStreamingDelta(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"text", `{"method":"item/agentMessage/delta","params":{"threadId":"t","turnId":"u","itemId":"i","delta":"a short fragment"}}`},
		{"thinking", `{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"t","turnId":"u","itemId":"i","delta":"a short fragment","summaryIndex":0}}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			line := []byte(tc.line)
			wire := New("", nil).NewWire()
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				msgs, err := wire.ParseMessage(line)
				if err != nil {
					b.Fatal(err)
				}
				if len(msgs) != 1 {
					b.Fatalf("messages = %d, want 1", len(msgs))
				}
			}
		})
	}
}

// BenchmarkParseToolLifecycle measures the source-bearing command events that
// child threads interleave with parent content on the same app-server wire.
func BenchmarkParseToolLifecycle(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"start", `{"method":"item/started","params":{"threadId":"child","turnId":"turn","item":{"id":"command","type":"commandExecution","command":"printf child","cwd":"/workspace","status":"inProgress"}}}`},
		{"output", `{"method":"item/commandExecution/outputDelta","params":{"threadId":"child","turnId":"turn","itemId":"command","delta":"child output"}}`},
		{"completed", `{"method":"item/completed","params":{"threadId":"child","turnId":"turn","item":{"id":"command","type":"commandExecution","command":"printf child","cwd":"/workspace","status":"completed","exitCode":0}}}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			line := []byte(tc.line)
			wire := New("", nil).NewWire()
			if _, err := wire.ParseMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"root"}}}`)); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := wire.ParseMessage(line); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReplayConcurrentChildTools measures recorded raw history through the
// same stateful wire and canonical record parser used to warm a reattachment.
func BenchmarkReplayConcurrentChildTools(b *testing.B) {
	data, err := os.ReadFile("testdata/evidence/concurrent-child-tools-v3.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		wire := New("", nil).NewWire()
		parser, err := agent.NewLogRecordParser(agent.LogVersionV3, wire.ParseMessage)
		if err != nil {
			b.Fatal(err)
		}
		for _, line := range lines {
			if _, err := parser.ParseRecord(line); err != nil {
				b.Fatal(err)
			}
		}
	}
}
