// Benchmarks for replaying retained relay history through a session wire.

package agent

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// BenchmarkWarmHistoryReplay measures the per-adoption cost that WarmHistory adds
// on the host: the transfer retains the warm payload so a failed attempt cannot
// reach the wire, and the replay then parses it through the session wire. The
// benchmark copies the synthetic stream into a fresh payload per iteration, the
// way a transfer does, so the allocation and dispatch costs are both reported.
// The SSH round trips the transfer also needs are not measurable here.
func BenchmarkWarmHistoryReplay(b *testing.B) {
	for _, size := range []int64{1 << 20, 10 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			stream := warmHistoryStream(b, size)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			bound := int64(len(stream))
			for range b.N {
				// Retain the payload through the same helper the transfer uses,
				// with the stream filling the bound exactly as a live transfer does.
				payload, err := retainRelayPayload(bytes.NewReader(stream), bound)
				if err != nil {
					b.Fatal(err)
				}
				wire := &warmHistoryWire{}
				parser, err := NewLogRecordParser(LogVersionV3, wire.ParseMessage)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := scanRelayTailRecords(bytes.NewReader(payload), parser, 0, false, "bench", "gen", nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// warmHistoryStream builds a canonical v3 agent-record stream of at most size
// bytes, ending on a whole record so a transfer bound of len(stream) is filled
// exactly.
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

func (*warmHistoryWire) ParseMessage(line []byte) ([]Message, error) { return nil, nil }

func (*warmHistoryWire) WritePrompt(io.Writer, Prompt, LogSink) error { return nil }

func (*warmHistoryWire) WriteCompact(io.Writer, string, LogSink) error { return nil }
