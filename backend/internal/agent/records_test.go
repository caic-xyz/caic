// Tests ordered record admission, in-memory framing, and prompt ingestion performance.

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func TestMarshalInputRecords(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		encoded, err := marshalInputRecords([]byte("\n{\"type\":\"first\"}\n{\"type\":\"second\",\"n\":2}\n"))
		if err != nil {
			t.Fatal(err)
		}
		frames, err := validateRecordBatch(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != 2 || frames[0].Kind != taskslog.KindInput || frames[1].Kind != taskslog.KindInput ||
			string(frames[0].Native) != `{"type":"first"}` || string(frames[1].Native) != `{"type":"second","n":2}` {
			t.Fatalf("unexpected input records: %+v", frames)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := marshalInputRecords([]byte("{}\n{}")); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial record: %v", err)
		}
		encoded, err := marshalInputRecords([]byte("{bad}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateRecordBatch(encoded); err == nil {
			t.Fatal("malformed native input accepted")
		}
	})
}

func TestValidateRecordBatch(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := validateRecordBatch([]byte("{\"t\":\"agent\",\"ts\":1.000,\"msg\":{}}")); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial record: %v", err)
		}
		if _, err := validateRecordBatch([]byte("\n")); err == nil {
			t.Fatal("empty physical record accepted")
		}
	})
}

func TestNextInputRecord(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		data := bytes.Repeat([]byte{'x'}, taskslog.MaxRecordSize)
		data[len(data)-2] = '\n'
		line, rest, err := nextInputRecord(data)
		if err != nil || len(line) != taskslog.MaxRecordSize-1 || len(rest) != 1 {
			t.Fatalf("maximum accepted record: line=%d rest=%d error=%v", len(line), len(rest), err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("terminated at limit", func(t *testing.T) {
			t.Parallel()
			data := bytes.Repeat([]byte{'x'}, taskslog.MaxRecordSize)
			data[len(data)-1] = '\n'
			if _, _, err := nextInputRecord(data); err == nil {
				t.Fatal("exclusive limit accepted")
			}
		})
		t.Run("unterminated at limit", func(t *testing.T) {
			t.Parallel()
			data := bytes.Repeat([]byte{'x'}, taskslog.MaxRecordSize)
			if _, _, err := nextInputRecord(data); err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("oversized unterminated record: %v", err)
			}
		})
	})
}

// BenchmarkPromptRecord measures ordered prompt admission for encoded image input.
func BenchmarkPromptRecord(b *testing.B) {
	for _, size := range []int{1024, 8 << 20} {
		b.Run(fmt.Sprintf("Image%d", size), func(b *testing.B) {
			data := fmt.Appendf(nil, `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}}`+"\n", strings.Repeat("a", size))
			owner := newTestRecordsWithHandler(b, b.Context(), DiscardLogSink{}, &testMessageParser{}, &testRecordHandler{consumer: b.Context()})
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result := owner.submit(b.Context(), &recordWork{op: recordPrompt, data: data})
				if result.err != nil {
					b.Fatal(result.err)
				}
			}
		})
	}
}
