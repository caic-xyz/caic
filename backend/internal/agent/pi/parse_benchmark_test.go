// Benchmarks for Pi wire parsing performance.

package pi

import (
	"encoding/json"
	"strings"
	"testing"

	genaipi "github.com/maruel/genai/providers/pi"
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

func benchmarkMessageUpdateLine(b *testing.B, accumulatedBytes int) []byte {
	b.Helper()
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type agentMessage struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	line := struct {
		Type                  genaipi.EventType `json:"type"`
		AssistantMessageEvent struct {
			Type  genaipi.DeltaType `json:"type"`
			Delta string            `json:"delta"`
		} `json:"assistantMessageEvent"`
		Message agentMessage `json:"message"`
	}{
		Type: genaipi.EventMessageUpdate,
		Message: agentMessage{
			Role:    "assistant",
			Content: []contentBlock{{Type: "text", Text: strings.Repeat("x", accumulatedBytes)}},
		},
	}
	line.AssistantMessageEvent.Type = genaipi.DeltaTextDelta
	line.AssistantMessageEvent.Delta = "x"
	data, err := json.Marshal(line)
	if err != nil {
		b.Fatal(err)
	}
	return data
}
