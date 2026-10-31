// Benchmarks for streaming authoritative recorded history into a reconnecting wire.

package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkWarmHistoryReplay measures the per-reconnect host cost of replaying
// the authoritative recorded task history. Files are opened and streamed through
// the production warm path on each iteration; the wire is deliberately cheap to
// isolate record dispatch and I/O. The 128 MiB case captures long-task latency
// beyond the relay adoption snapshot bound without retaining the whole file.
func BenchmarkWarmHistoryReplay(b *testing.B) {
	for _, size := range []int64{1 << 20, 10 << 20, 128 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			stream := warmHistoryStream(b, size)
			path := filepath.Join(b.TempDir(), "history.jsonl")
			header := []byte(`{"t":"caic_meta","version":3,"prompt":"benchmark","harness":"claude"}` + "\n")
			if err := os.WriteFile(path, append(header, stream...), 0o600); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := warmRecordedHistory(b.Context(), path, &warmHistoryWire{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// warmHistoryStream builds a canonical v3 agent-record stream of at most size
// bytes, ending on a whole record.
func warmHistoryStream(b *testing.B, size int64) []byte {
	b.Helper()
	var buf bytes.Buffer
	line := []byte(`{"t":"agent","ts":1.000,"msg":{"type":"native_subagent","subagent":{"id":"codex:thread:test","status":"running"}}}` + "\n")
	for int64(buf.Len()+len(line)) <= size {
		buf.Write(line)
	}
	return buf.Bytes()
}

// warmHistoryWire is the cheapest possible wire format: it accepts every record,
// so the benchmark measures the record envelope and dispatch cost rather than a
// particular harness parser.
type warmHistoryWire struct{}

func (*warmHistoryWire) ParseOutput(line []byte) ([]Message, error) { return nil, nil }

func (*warmHistoryWire) WritePrompt(io.Writer, Prompt, LogSink) error { return nil }

func (*warmHistoryWire) ParseInput([]byte) ([]Message, error) { return nil, nil }
