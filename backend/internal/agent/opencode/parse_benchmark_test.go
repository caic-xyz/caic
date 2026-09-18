// Benchmarks OpenCode ACP session-update parsing.

package opencode

import (
	"encoding/json"
	"testing"

	genaiopencode "github.com/maruel/genai/providers/opencode"
)

func BenchmarkParseToolCallUpdateRawOutput(b *testing.B) {
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress","rawOutput":{"output":"streaming output"}}}}`)
	b.ReportAllocs()
	for b.Loop() {
		msgs, _, err := parseMessage(line)
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) != 1 {
			b.Fatalf("parseMessage returned %d messages, want 1", len(msgs))
		}
	}
}

func BenchmarkHandshakeResultSetConfigOptions(b *testing.B) {
	options := []genaiopencode.SessionConfigOption{
		{ID: genaiopencode.ConfigOptionModel, CurrentValue: json.RawMessage(`"openai/gpt-5"`)},
		{ID: genaiopencode.ConfigOptionEffort, CurrentValue: json.RawMessage(`"high"`)},
	}
	b.ReportAllocs()
	for b.Loop() {
		var result handshakeResult
		if err := result.setConfigOptions(options); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseNativeSubagentUpdates measures the per-line cost the stateful
// adapter adds to ACP tool-call updates: an unrelated tool update and a
// delegation completion carrying the task output.
func BenchmarkParseNativeSubagentUpdates(b *testing.B) {
	for _, test := range []struct {
		name string
		line string
	}{
		{
			name: "unrelated_tool_update",
			line: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress","kind":"execute","title":"bash","rawInput":{"command":"ls"}}}}`,
		},
		{
			name: "delegation_completion",
			line: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_2","status":"completed","title":"README joke","content":[{"type":"content","content":{"type":"text","text":"<task id=\"child\" state=\"completed\"><task_result>a joke</task_result></task>"}}],"rawOutput":{"output":"<task id=\"child\" state=\"completed\"><task_result>a joke</task_result></task>","metadata":{"parentSessionId":"s","sessionId":"child"}}}}}`,
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
