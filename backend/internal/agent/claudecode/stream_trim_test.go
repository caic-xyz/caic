// Tests which streamed Claude deltas a settled log drops.

package claudecode

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestStreamTrim(t *testing.T) {
	t.Parallel()
	event := func(parent, apiMessageID, event string) string {
		return `{"type":"stream_event","event":` + event + `,"session_id":"s","parent_tool_use_id":` + parent + `,"api_message_id":"` + apiMessageID + `"}`
	}
	const toolInput = `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`
	observed := []string{
		event(`null`, "m", `{"type":"message_start","message":{"id":"m"}}`),
		event(`null`, "m", `{"type":"content_block_stop","index":0}`),
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"session_id":"s","parent_tool_use_id":null}`,
	}
	for _, test := range []struct {
		name       string
		line       string
		superseded bool
	}{
		{"stopped tool input", event(`null`, "m", toolInput), true},
		{"subagent block with the same index", event(`"spawn"`, "m", toolInput), false},
		{"other message with the same index", event(`null`, "other", toolInput), false},
		{"no API message identity", `{"type":"stream_event","event":` + toolInput + `,"session_id":"s","parent_tool_use_id":null}`, false},
		{"stop record", observed[1], false},
		{"message-level event", event(`null`, "m", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`), false},
		{"assistant record", `{"type":"assistant","message":{"id":"m","role":"assistant","content":[]},"session_id":"s"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			trimmer, ok := New().NewWire().(agent.StreamTrimmer)
			if !ok {
				t.Fatal("Claude wire has no stream trim rule")
			}
			trim := trimmer.NewStreamTrim()
			for _, line := range observed {
				if err := trim.Observe([]byte(line)); err != nil {
					t.Fatal(err)
				}
			}
			superseded, err := trim.Superseded([]byte(test.line))
			if err != nil {
				t.Fatal(err)
			}
			if superseded != test.superseded {
				t.Fatalf("Superseded = %t, want %t", superseded, test.superseded)
			}
		})
	}
}
