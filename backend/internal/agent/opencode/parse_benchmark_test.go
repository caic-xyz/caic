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
		msgs, err := parseMessage(line)
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
