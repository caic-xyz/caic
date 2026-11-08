// Tests physical ordinals, partial append boundaries, descriptor authority, and compressed EOF ownership.

package taskslog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/klauspost/compress/zstd"
)

const testHeader = `{"t":"caic_meta","version":3,"prompt":"history","harness":"claude"}`

func writeRawFixture(t *testing.T, compressed bool, raw string) string {
	path := filepath.Join(t.TempDir(), "task.jsonl")
	data := []byte(raw)
	if compressed {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		data = enc.EncodeAll(data, nil)
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		path += ".zst"
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRawCursor(t *testing.T) {
	t.Parallel()
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("large record compressed=%t", compressed), func(t *testing.T) {
			t.Parallel()
			line := `{"t":"agent","ts":1.000,"msg":{"text":"` + strings.Repeat("x", 33<<20) + `"}}`
			tail := `{"t":"text","text":"after"}`
			path := writeRawFixture(t, compressed, testHeader+"\n"+line+"\n"+tail+"\n")
			cursor, err := OpenRawCursor(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var count uint64
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				count++
				if record.ID != count {
					t.Fatalf("record ID = %d, want %d", record.ID, count)
				}
				if count == 2 && string(record.Frame.Encoded) != line {
					t.Fatal("large record bytes changed")
				}
				if count == 3 && string(record.Frame.Encoded) != tail {
					t.Fatal("record following large record changed")
				}
			}
			if count != 3 {
				t.Fatalf("records = %d, want 3", count)
			}
		})
	}

	t.Run("partial append and ordinals", func(t *testing.T) {
		t.Parallel()
		path := writeRawFixture(t, false, testHeader+"\n\n"+`{"t":"model_info","context_window":123}`+"\n"+`{"t":"text","text":"pending"}`)
		cursor, err := OpenRawCursor(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		var ids []uint64
		read := func() {
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
			}
		}
		read()
		if !reflect.DeepEqual(ids, []uint64{1, 2}) {
			t.Fatalf("partial IDs=%v", ids)
		}
		read()
		if len(ids) != 2 {
			t.Fatalf("partial record delivered twice: %v", ids)
		}
		f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString("\n" + testHeader + "\n")
		if err := errors.Join(writeErr, f.Close()); err != nil {
			t.Fatal(err)
		}
		read()
		if !reflect.DeepEqual(ids, []uint64{1, 2, 3, 4}) {
			t.Fatalf("appended IDs=%v", ids)
		}
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
		for _, err := range cursor.Read(t.Context()) {
			if err != nil {
				return
			}
		}
		t.Fatal("truncation accepted")
	})
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete final record compressed=%t", compressed), func(t *testing.T) {
			t.Parallel()
			path := writeRawFixture(t, compressed, testHeader+"\n"+`{"t":"text","text":"EOF"}`)
			cursor, err := OpenRawCursor(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var count int
			for _, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				count++
			}
			want := 1
			if compressed {
				want = 2
			}
			if count != want {
				t.Fatalf("records=%d want=%d", count, want)
			}
			if compressed && (!cursor.Exhausted() || cursor.scanner != nil) {
				t.Fatal("completed compressed buffers retained")
			}
		})
	}
	t.Run("compressed descriptor released at EOF", func(t *testing.T) {
		t.Parallel()
		path := writeRawFixture(t, true, testHeader+"\n")
		cursor, err := OpenRawCursor(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		stream, ok := cursor.compressed.(*zstdReadCloser)
		if !ok {
			t.Fatal("compressed cursor has no zstd reader")
		}
		file := stream.file
		for _, err := range cursor.Read(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("descriptor retained: %v", err)
		}
	})
	t.Run("cancellation before read", func(t *testing.T) {
		t.Parallel()
		cursor, err := OpenRawCursor(writeRawFixture(t, false, testHeader+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, err := range cursor.Read(ctx) {
			if errors.Is(err, context.Canceled) {
				return
			}
			t.Fatalf("canceled read: %v", err)
		}
		t.Fatal("cancellation ignored")
	})
}

func TestScanner(t *testing.T) {
	t.Parallel()
	t.Run("EOF final record", func(t *testing.T) {
		t.Parallel()
		scanner := NewScanner(strings.NewReader(testHeader+"\n"+`{"t":"text","text":"EOF"}`), "fixture")
		if _, err := scanner.ReadHeader(); err != nil {
			t.Fatal(err)
		}
		if !scanner.Scan() || scanner.Frame().Kind != KindText {
			t.Fatal("final record missing")
		}
		if scanner.Scan() || !scanner.EOFValidated() {
			t.Fatalf("EOF=%v", scanner.Err())
		}
	})
	t.Run("header only does not prove EOF", func(t *testing.T) {
		t.Parallel()
		path := writeRawFixture(t, false, testHeader+"\n")
		err := ScanFile(path, true, func(_ os.FileInfo, _ *Scanner, _ v3.MetaMessage) error { return nil })
		if err == nil {
			t.Fatal("unconsumed EOF accepted")
		}
	})
}
