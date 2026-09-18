// Benchmarks for the Claude wire parser and its native-subagent adapter.

package claudecode

import (
	"testing"
)

// BenchmarkParseNativeSubagentLines measures the per-line cost the stateful
// adapter adds to every Claude record: an unrelated assistant message, the Agent
// tool use that starts a delegation, and the task notification that settles it.
// Repeating a record on one wire measures the steady-state line cost.
func BenchmarkParseNativeSubagentLines(b *testing.B) {
	for _, test := range []struct {
		name string
		line string
	}{
		{
			name: "assistant_text",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}`,
		},
		{
			name: "agent_tool_use",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"description":"Joke about README.md","prompt":"Tell a short joke about README.md","subagent_type":"general-purpose","run_in_background":false}}]}}`,
		},
		{
			name: "task_started",
			line: `{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"toolu_1","description":"Joke about README.md","subagent_type":"general-purpose","is_backgrounded":false,"task_type":"local_agent","prompt":"Tell a short joke about README.md"}`,
		},
		{
			name: "task_notification",
			line: `{"type":"system","subtype":"task_notification","task_id":"agent-1","tool_use_id":"toolu_1","status":"completed","output_file":"/workspace/tasks/agent-1.output","summary":"a joke","uuid":"u","session_id":"s"}`,
		},
	} {
		b.Run(test.name, func(b *testing.B) {
			line := []byte(test.line)
			wire := New().NewWire()
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
