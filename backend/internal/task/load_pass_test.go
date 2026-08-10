// Tests v1 production task-log read-pass bounds for plain and zstd logs.

package task

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// TestV1ProductionReadPassMatrix covers v1 task-log read-pass bounds across
// plain and zstd production scan paths.
func TestV1ProductionReadPassMatrix(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "pass count",
		Harness:     harness.Claude,
	})
	for _, tc := range []struct {
		name       string
		nativeLine string
	}{
		{
			name:       "native-session",
			nativeLine: `{"type":"session","session_id":"native-session","version":"native"}`,
		},
		{
			name:       "no-top-level-session",
			nativeLine: `{"jsonrpc":"2.0","method":"session/started","params":{"id":"native-session"}}`,
		},
	} {
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				name := "task.jsonl"
				if compressed {
					name += ".zst"
					writeCompressedLogFile(t, dir, name, seqOf(meta, tc.nativeLine))
				} else {
					writeLogFile(t, dir, name, meta, tc.nativeLine)
				}
				path := filepath.Join(dir, name)
				logicalSize := int64(len(meta) + len(tc.nativeLine) + 2)
				var logicalBytes int64
				completePasses := 0

				openCounted := func() (*physicalLogReader, func()) {
					file, err := os.Open(filepath.Clean(path))
					if err != nil {
						t.Fatal(err)
					}
					info, err := file.Stat()
					if err != nil {
						_ = file.Close()
						t.Fatal(err)
					}
					if !compressed {
						counted := &countingLogReader{file: file, size: info.Size()}
						return &physicalLogReader{file: file, reader: counted, info: info}, func() {
							_ = counted.Close()
							logicalBytes += counted.bytes
							if counted.complete {
								completePasses++
							}
						}
					}
					decoder, err := zstd.NewReader(file)
					if err != nil {
						_ = file.Close()
						t.Fatal(err)
					}
					counted := &countingReadCloser{reader: decoder, closeFn: func() error {
						decoder.Close()
						return file.Close()
					}}
					return &physicalLogReader{file: file, reader: counted, info: info}, func() {
						_ = counted.Close()
						logicalBytes += counted.bytes
						if counted.complete {
							completePasses++
						}
					}
				}

				headerReader, closeHeader := openCounted()
				var lt *LoadedTask
				var err error
				if compressed {
					lt, err = loadCompressedLogHeaderFromReader(path, headerReader, false, false)
				} else {
					tailSource, ok := headerReader.reader.(io.ReaderAt)
					if !ok {
						closeHeader()
						t.Fatal("plain physical reader does not support ReadAt")
					}
					lt, err = loadPlainLogHeader(path, headerReader, headerReader.reader, tailSource)
				}
				if err != nil {
					closeHeader()
					t.Fatal(err)
				}
				closeHeader()
				if lt.SessionID != "" || lt.AgentVersion != "" {
					t.Fatalf("header scan treated native record as task session metadata: (%q, %q)", lt.SessionID, lt.AgentVersion)
				}

				messageReader, closeMessages := openCounted()
				nativeCalls := 0
				_, err = loadSemanticLogSnapshotFromReader(path, messageReader, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
					if h != harness.Claude {
						t.Fatalf("resolver harness = %q, want %q", h, harness.Claude)
					}
					return func(line []byte) ([]agent.Message, error) {
						nativeCalls++
						return []agent.Message{&agent.TextMessage{Text: string(line)}}, nil
					}, nil
				})
				if err != nil {
					closeMessages()
					t.Fatal(err)
				}
				closeMessages()
				if nativeCalls != 1 {
					t.Fatalf("native parser calls = %d, want 1", nativeCalls)
				}

				if !compressed {
					appendReader, closeAppend := openCounted()
					appendSource, ok := appendReader.reader.(io.ReadSeeker)
					if !ok {
						closeAppend()
						t.Fatal("plain physical reader does not support seeking")
					}
					if err := validateRawLogAppend(appendSource, appendReader.file, path, &Task{Harness: harness.Claude}); err != nil {
						closeAppend()
						t.Fatal(err)
					}
					closeAppend()
				}

				if completePasses > 3 {
					t.Errorf("complete passes = %d, want at most 3", completePasses)
				}
				if logicalBytes > 3*logicalSize+min(int64(65536), logicalSize) {
					t.Errorf("logical bytes = %d, exceeds three complete passes plus bounded tail (%d)", logicalBytes, 3*logicalSize+min(int64(65536), logicalSize))
				}
			})
		}
	}
}
