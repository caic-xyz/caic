// Benchmarks for Pi wire parsing performance.

package pi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maruel/genai/providers/pi"
)

func BenchmarkParseMessageUpdateWithAccumulatedMessage(b *testing.B) {
	line := benchmarkMessageUpdateLine(b, 256<<10)
	parser := New("", nil).NewWire().ParseMessage

	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		msgs, err := parser(line)
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) != 1 {
			b.Fatalf("got %d messages, want 1", len(msgs))
		}
	}
}

// BenchmarkParseMessageToolExecEnd covers the ParseMessage path that used to
// decode the event type twice: once in ParseMessage's own probe, once again
// inside parseMessage's dispatch. tool_execution_end is representative of
// the largest, most frequent lines on this path (full tool output).
func BenchmarkParseMessageToolExecEnd(b *testing.B) {
	line := benchmarkToolExecEndLine(b, 64<<10)
	parser := New("", nil).NewWire().ParseMessage

	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		msgs, err := parser(line)
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) != 1 {
			b.Fatalf("got %d messages, want 1", len(msgs))
		}
	}
}

func benchmarkToolExecEndLine(b *testing.B, outputBytes int) []byte {
	ev := struct {
		Type       pi.EventType `json:"type"`
		ToolCallID string       `json:"toolCallId"`
		ToolName   string       `json:"toolName"`
		Result     struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		IsError bool `json:"isError"`
	}{
		Type:       pi.EventToolExecEnd,
		ToolCallID: "call_1",
		ToolName:   "bash",
	}
	ev.Result.Content = []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: strings.Repeat("x", outputBytes)}}
	data, err := json.Marshal(ev)
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func benchmarkMessageUpdateLine(b *testing.B, accumulatedBytes int) []byte {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type agentMessage struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	line := struct {
		Type                  pi.EventType `json:"type"`
		AssistantMessageEvent struct {
			Type  pi.DeltaType `json:"type"`
			Delta string       `json:"delta"`
		} `json:"assistantMessageEvent"`
		Message agentMessage `json:"message"`
	}{
		Type: pi.EventMessageUpdate,
		Message: agentMessage{
			Role:    "assistant",
			Content: []contentBlock{{Type: "text", Text: strings.Repeat("x", accumulatedBytes)}},
		},
	}
	line.AssistantMessageEvent.Type = pi.DeltaTextDelta
	line.AssistantMessageEvent.Delta = "x"
	data, err := json.Marshal(line)
	if err != nil {
		b.Fatal(err)
	}
	return data
}
