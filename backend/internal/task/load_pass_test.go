// Tests task-log observation paths stay bounded to the raw header rather than rescanning logs.

package task

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// TestLogObservationFromSnapshotRereadsStrictRawHeader rejects a same-stat raw
// header replacement before a retained snapshot can be reused.
func TestLogObservationFromSnapshotRereadsStrictRawHeader(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "task.jsonl")
	original := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "bound"})
	if err := os.WriteFile(path, []byte(original+"\n{\"type\":\"assistant\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
		return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace([]byte(original), []byte(`"bound"`), []byte(`"other"`), 1)
	if len(changed) != len(original) {
		t.Fatal("test header mutation changed length")
	}
	if err := os.WriteFile(path, append(changed, []byte("\n{\"type\":\"assistant\"}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok := logObservationFromValidatedSnapshot(snapshot, path); ok {
		t.Fatal("retained snapshot accepted a raw header replacement with stable identity and stat")
	}
}

// TestLogObservationHeaderPassBound exercises the bounded reader core with
// large plain and zstd records after the header. The
// bounded scanner may fill its buffer, but must not validate or read the full
// raw log during an append validation.
func TestLogObservationHeaderPassBound(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Harness:     harness.Claude,
		Prompt:      "validated header",
	})
	record := bytes.Repeat([]byte("x"), 8<<20)
	for _, compressed := range []bool{false, true} {
		format := "plain"
		if compressed {
			format = "zstd"
		}
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task.jsonl")
			if compressed {
				path += ".zst"
			}
			file, err := os.Create(filepath.Clean(path))
			if err != nil {
				t.Fatal(err)
			}
			if compressed {
				enc, createErr := zstd.NewWriter(file)
				if createErr != nil {
					_ = file.Close()
					t.Fatal(createErr)
				}
				_, err = enc.Write(append(append([]byte{}, meta...), '\n'))
				if err == nil {
					_, err = enc.Write(append(record, '\n'))
				}
				err = errors.Join(err, enc.Close(), file.Close())
			} else {
				_, err = file.Write(append(append([]byte{}, meta...), '\n'))
				if err == nil {
					_, err = file.Write(append(record, '\n'))
				}
				err = errors.Join(err, file.Close())
			}
			if err != nil {
				t.Fatal(err)
			}

			opened, err := openPhysicalLogReader(path)
			if err != nil {
				t.Fatal(err)
			}
			counted := &countingReadCloser{reader: opened.reader, closeFn: opened.Close}
			reader := &physicalLogReader{file: opened.file, reader: counted, info: opened.info}
			if _, err := logObservationFromReader(path, reader); err != nil {
				_ = reader.Close()
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if counted.bytes > 64<<10 {
				t.Fatalf("logObservationForLog read %d bytes after a small header, want at most the bounded header scanner buffer", counted.bytes)
			}
		})
	}
}
