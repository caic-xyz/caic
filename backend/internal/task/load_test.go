// Tests for task loading and configuration resolution.

package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func setClaudeParser(tasks []*LoadedTask) {
	for _, lt := range tasks {
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
	}
}

func writeLogFile(t *testing.T, dir, name string, lines ...string) {
	data := make([]byte, 0, len(lines)*64)
	for _, l := range lines {
		data = append(data, l...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seqOf(lines ...string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for _, line := range lines {
			if !yield([]byte(line)) {
				return
			}
		}
	}
}

func writeCompressedLogFile(t *testing.T, dir, name string, lines iter.Seq[[]byte]) {
	if !isLogCompressed(name) {
		t.Fatalf("compressed test log name %q must end in %s", name, logCompressedExt)
	}
	out, err := os.OpenFile(filepath.Clean(filepath.Join(dir, name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	var writeErr error
	for line := range lines {
		if _, err := enc.Write(line); err != nil {
			writeErr = err
			break
		}
		if _, err := enc.Write([]byte("\n")); err != nil {
			writeErr = err
			break
		}
	}
	if err := errors.Join(writeErr, enc.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
}

func writePhysicalTestLog(t *testing.T, compressed bool, lines ...string) string {
	dir := t.TempDir()
	if compressed {
		path := filepath.Join(dir, "task.jsonl.zst")
		writeCompressedLogFile(t, dir, filepath.Base(path), seqOf(lines...))
		return path
	}
	path := filepath.Join(dir, "task.jsonl")
	writeLogFile(t, dir, filepath.Base(path), lines...)
	return path
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	switch m := v.(type) {
	case agent.MetaMessage:
		version = m.Version
	case *agent.MetaMessage:
		version = m.Version
	}
	if version == int(agent.LogVersionV2) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		delete(raw, "type")
		raw["t"] = json.RawMessage(`"caic_meta"`)
		b, err = json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	return string(b)
}

type countingLogReader struct {
	file *os.File
	size int64

	bytes    int64
	complete bool
}

type countingReadCloser struct {
	reader   io.Reader
	closeFn  func() error
	bytes    int64
	complete bool
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	if errors.Is(err, io.EOF) {
		r.complete = true
	}
	return n, err
}

func reopenWithProofReplay(t *testing.T, dir, path string, tk *Task, snapshot *ValidatedLogSnapshot) int64 {
	tk.SetLogPath(path)
	tk.SetLogValidationSnapshot(snapshot)
	w, err := (&LogStore{LogDir: dir}).Reopen(tk)
	if err != nil {
		t.Fatalf("Reopen after adopted scans: %v", err)
	}
	appendWriter, ok := w.(*taskLogWriter)
	if !ok {
		t.Fatalf("Reopen writer = %T, want *taskLogWriter", w)
	}
	proofBytes, err := appendWriter.file.Seek(0, io.SeekCurrent)
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close Reopen writer: %v", err)
	}
	return proofBytes
}

func (r *countingReadCloser) Close() error {
	return r.closeFn()
}

func (r *countingLogReader) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	r.bytes += int64(n)
	if errors.Is(err, io.EOF) && r.bytes >= r.size {
		r.complete = true
	}
	return n, err
}

func (r *countingLogReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.file.ReadAt(p, off)
	r.bytes += int64(n)
	return n, err
}

func (r *countingLogReader) Seek(offset int64, whence int) (int64, error) {
	return r.file.Seek(offset, whence)
}

func (r *countingLogReader) Close() error {
	return r.file.Close()
}

// claudeAssistant builds a Claude wire-format assistant NDJSON line from
// content blocks. Each block is a map with at minimum a "type" key.
func claudeAssistant(t *testing.T, blocks ...map[string]any) string {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": blocks,
		},
	}
	return mustJSON(t, msg)
}

// claudeInit builds a Claude wire-format system/init NDJSON line.
func claudeInit(t *testing.T, sessionID string) string {
	msg := map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
	}
	return mustJSON(t, msg)
}

func TestReadLogAuthority(t *testing.T) {
	t.Parallel()
	meta := func(t *testing.T, version int, h harness.Name) string {
		return mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     version,
			Prompt:      "task",
			Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
			Harness:     h,
		})
	}

	t.Run("PlainV1WithMatchingSegment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		writeLogFile(t, dir, "a.jsonl", "", meta(t, 1, harness.Claude), `{"type":"assistant"}`, meta(t, 1, harness.Claude))
		authority, err := readLogAuthority(path)
		if err != nil {
			t.Fatal(err)
		}
		if authority.Version != agent.LogVersionV1 || authority.Harness != harness.Claude {
			t.Fatalf("authority = %+v", authority)
		}
	})
	t.Run("CompressedV2", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl.zst")
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta(t, 2, harness.Codex)))
		authority, err := readLogAuthority(path)
		if err != nil {
			t.Fatal(err)
		}
		if authority.Version != agent.LogVersionV2 || authority.Harness != harness.Codex {
			t.Fatalf("authority = %+v", authority)
		}
	})

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{name: "MissingHeader", lines: []string{`{"type":"assistant"}`}},
		{name: "CorruptHeader", lines: []string{`{"type":"caic_meta"`}},
		{name: "MissingVersion", lines: []string{meta(t, 0, harness.Claude)}},
		{name: "FutureVersion", lines: []string{meta(t, 3, harness.Claude)}},
		{name: "ChangedVersion", lines: []string{meta(t, 1, harness.Claude), meta(t, 2, harness.Claude)}},
		{name: "ChangedHarness", lines: []string{meta(t, 1, harness.Claude), meta(t, 1, harness.Codex)}},
		{name: "WrongLaterDiscriminator", lines: []string{meta(t, 1, harness.Claude), `{"t":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`}},
		{name: "LaterDuplicateTypeMetaFirst", lines: []string{meta(t, 1, harness.Claude), `{"type":"caic_meta","type":"assistant","version":1,"prompt":"task","repos":[],"harness":"claude"}`}},
		{name: "LaterDuplicateTypeMetaLast", lines: []string{meta(t, 1, harness.Claude), `{"type":"assistant","type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`}},
		{name: "LaterDuplicateTMetaFirst", lines: []string{meta(t, 2, harness.Codex), `{"t":"caic_meta","t":"assistant","version":2,"prompt":"task","repos":[],"harness":"codex"}`}},
		{name: "LaterDuplicateTMetaLast", lines: []string{meta(t, 2, harness.Codex), `{"t":"assistant","t":"caic_meta","version":2,"prompt":"task","repos":[],"harness":"codex"}`}},
		{name: "DuplicateAuthorityKey", lines: []string{`{"type":"caic_meta","type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "a.jsonl")
			writeLogFile(t, dir, "a.jsonl", tc.lines...)
			if _, err := readLogAuthority(path); err == nil {
				t.Fatal("readLogAuthority error = nil")
			}
		})
	}
	t.Run("CompressedRejectsRawAuthorityKeys", func(t *testing.T) {
		t.Parallel()
		cases := [][]string{
			{`{"type":"caic_meta","t":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`},
			{`{"type":"caic_meta","version":1,"version":1,"prompt":"task","repos":[],"harness":"claude"}`},
			{`{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude","harness":"claude"}`},
		}
		for i, lines := range cases {
			dir := t.TempDir()
			path := filepath.Join(dir, fmt.Sprintf("case-%d.jsonl.zst", i))
			writeCompressedLogFile(t, dir, filepath.Base(path), seqOf(lines...))
			if _, err := readLogAuthority(path); err == nil {
				t.Fatalf("case %d: readLogAuthority error = nil", i)
			}
		}
	})
	t.Run("DuplicateOrdinaryKeyRemainsV1Compatible", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		writeLogFile(t, dir, "a.jsonl", meta(t, 1, harness.Claude), `{"type":"assistant","type":"assistant"}`)
		if _, err := readLogAuthority(path); err != nil {
			t.Fatalf("readLogAuthority error = %v, want ordinary duplicate accepted", err)
		}
	})
	t.Run("DuplicateNonAuthorityKeyRemainsV1Compatible", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		writeLogFile(t, dir, "a.jsonl", `{"type":"caic_meta","version":1,"prompt":"first","prompt":"last","repos":[],"harness":"claude"}`)
		if _, err := readLogAuthority(path); err != nil {
			t.Fatalf("readLogAuthority error = %v, want non-authority duplicate accepted", err)
		}
	})
	t.Run("V2RejectsUnknownHeaderField", func(t *testing.T) {
		t.Parallel()
		for _, compressed := range []bool{false, true} {
			t.Run(map[bool]string{false: "plain", true: "compressed"}[compressed], func(t *testing.T) {
				t.Parallel()
				meta := mustJSON(t, agent.MetaMessage{
					MessageType: "caic_meta",
					Version:     2,
					Prompt:      "task",
					Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
					Harness:     harness.Codex,
				})
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(meta), &fields); err != nil {
					t.Fatal(err)
				}
				fields["bogus"] = json.RawMessage(`true`)
				data, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "a.jsonl")
				if compressed {
					path += logCompressedExt
					writeCompressedLogFile(t, dir, filepath.Base(path), seqOf(string(data)))
				} else {
					writeLogFile(t, dir, filepath.Base(path), string(data))
				}
				if _, err := readLogAuthority(path); err == nil || !strings.Contains(err.Error(), "unknown v2 caic_meta fields") {
					t.Fatalf("readLogAuthority error = %v, want unknown-field error", err)
				}
			})
		}
	})
	t.Run("DeepNestedNativeRecordDoesNotUseGoroutineStack", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		depth := 200_000
		nested := `{"type":"assistant","nested":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "}"
		writeLogFile(t, dir, "a.jsonl", meta(t, 1, harness.Claude), nested)
		if _, err := readLogAuthority(path); err != nil {
			t.Fatalf("readLogAuthority error = %v, want deeply nested native record accepted", err)
		}
	})
	t.Run("RejectsTooDeepLaterMetadataRecord", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		deep := strings.Repeat("[", maxJSONNesting+1) + "0" + strings.Repeat("]", maxJSONNesting+1)
		segment := `{"type":"caic_meta","version":2,"prompt":"task","repos":[],"harness":"codex","nested":` + deep + "}"
		writeLogFile(t, dir, "a.jsonl", meta(t, 1, harness.Claude), segment)
		if _, err := readLogAuthority(path); err == nil || !strings.Contains(err.Error(), "nesting exceeds limit") {
			t.Fatalf("readLogAuthority error = %v, want nesting-limit error", err)
		}
	})
	t.Run("Unreadable", func(t *testing.T) {
		t.Parallel()
		if _, err := readLogAuthority(filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("readLogAuthority error = %v, want os.ErrNotExist", err)
		}
	})
}

func TestPhysicalFileIdentity(t *testing.T) {
	t.Parallel()
	file, err := os.CreateTemp(t.TempDir(), "identity")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identity := physicalFileIdentityFromFile(file, info)
	if !identity.Valid {
		t.Fatalf("physical identity = %+v, want valid identity", identity)
	}
}

func TestLoadLogs(t *testing.T) {
	t.Parallel()
	t.Run("ForkedFromTaskIDMetadata", func(t *testing.T) {
		t.Parallel()
		path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
			MessageType:      "caic_meta",
			Version:          1,
			Prompt:           "forked task",
			Repos:            []agent.MetaRepo{{Name: "r", Branch: "caic-1"}},
			Harness:          harness.Claude,
			ForkedFromTaskID: "3BL0EKDTO000",
		}))

		tasks, err := LoadLogs(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len(tasks) = %d, want 1", len(tasks))
		}
		if tasks[0].ForkedFromTaskID != "3BL0EKDTO000" {
			t.Fatalf("ForkedFromTaskID = %q, want 3BL0EKDTO000", tasks[0].ForkedFromTaskID)
		}
	})
	t.Run("CompressedSummaryRejectsChangedForkAuthority", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task.jsonl.zst")
		meta := mustJSON(t, agent.MetaMessage{
			MessageType:      "caic_meta",
			Version:          int(agent.LogVersionV1),
			Prompt:           "forked task",
			Harness:          harness.Claude,
			ForkedFromTaskID: "3BL0EKDTO000",
		})
		writeCompressedLogFile(t, dir, filepath.Base(path), seqOf(meta))
		if _, err := LoadLogs(dir); err != nil {
			t.Fatal(err)
		}
		summaryPath := logSummaryPath(path)
		data, err := os.ReadFile(summaryPath) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		var summary logSummary
		if err := json.Unmarshal(data, &summary); err != nil { //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
			t.Fatal(err)
		}
		summary.Task.ForkedFromTaskID = "forged-parent"
		data, err = json.Marshal(summary) //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(summaryPath, data, 0o600); err != nil {
			t.Fatal(err)
		}

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 || tasks[0].ForkedFromTaskID != "3BL0EKDTO000" {
			t.Fatalf("summary cache accepted forged fork authority: %#v", tasks)
		}
	})
	t.Run("LazySemanticScanRejectsChangedRawHeader", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task.jsonl")
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "original",
			Harness:     harness.Claude,
		})
		assistant := claudeAssistant(t, map[string]any{"type": "text", "text": "message"})
		writeLogFile(t, dir, filepath.Base(path), meta, assistant)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		changed := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "changed!",
			Harness:     harness.Claude,
		})
		writeLogFile(t, dir, filepath.Base(path), changed, assistant)

		err = tasks[0].LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
		if err == nil || !strings.Contains(err.Error(), "immutable header changed") {
			t.Fatalf("LoadMessagesWithResolver error = %v, want immutable header change", err)
		}
		if tasks[0].Msgs != nil || tasks[0].Prompt != "original" {
			t.Fatalf("changed semantic scan mutated inventory task: %#v", tasks[0])
		}
	})
	t.Run("LazySemanticScanAllowsAppendWithSameRawHeader", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task.jsonl")
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "original",
			Harness:     harness.Claude,
		})
		writeLogFile(t, dir, filepath.Base(path), meta)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(claudeAssistant(t, map[string]any{"type": "text", "text": "appended"}) + "\n")
		if closeErr := file.Close(); writeErr != nil || closeErr != nil {
			t.Fatalf("append task log = %v, %v", writeErr, closeErr)
		}
		if err := tasks[0].LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(tasks[0].Msgs) != 1 {
			t.Fatalf("appended semantic messages = %#v, want one message", tasks[0].Msgs)
		}
	})
	t.Run("PhysicalAuthority", func(t *testing.T) {
		t.Parallel()
		meta := func(version agent.LogVersion, h harness.Name) string {
			return mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(version),
				Prompt:      "task",
				Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
				Harness:     h,
			})
		}
		assistant := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		for _, compressed := range []bool{false, true} {
			label := "Plain"
			name := "authority.jsonl"
			if compressed {
				label = "Compressed"
				name += ".zst"
			}
			t.Run(label+"LeadingBlankLines", func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				lines := []string{"", "  ", meta(agent.LogVersionV1, harness.Claude), assistant, meta(agent.LogVersionV1, harness.Claude)}
				if compressed {
					writeCompressedLogFile(t, dir, name, seqOf(lines...))
				} else {
					writeLogFile(t, dir, name, lines...)
				}
				path := filepath.Join(dir, name)
				if _, err := loadLogHeader(path); err != nil {
					t.Fatalf("loadLogHeader: %v", err)
				}
				if _, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return claudecode.New().NewWire().ParseMessage, nil
				}); err != nil {
					t.Fatalf("loadLogFile: %v", err)
				}
			})
			for _, mismatch := range []struct {
				name string
				line string
			}{
				{name: "ChangedVersion", line: meta(agent.LogVersionV2, harness.Claude)},
				{name: "ChangedHarness", line: meta(agent.LogVersionV1, harness.Codex)},
			} {
				t.Run(label+mismatch.name, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					lines := []string{meta(agent.LogVersionV1, harness.Claude), assistant, mismatch.line}
					if compressed {
						writeCompressedLogFile(t, dir, name, seqOf(lines...))
					} else {
						writeLogFile(t, dir, name, lines...)
					}
					path := filepath.Join(dir, name)
					want := "authority changed"
					if mismatch.name == "ChangedVersion" {
						want = "wrong t discriminator"
					}
					if _, err := loadLogHeader(path); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("loadLogHeader error = %v, want %s", err, want)
					}
					if _, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return claudecode.New().NewWire().ParseMessage, nil
					}); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("loadLogFile error = %v, want %s", err, want)
					}
				})
			}
		}
	})
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "a.jsonl", meta, asst, trailer)

		// Non-jsonl file should be ignored.
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Prompt != "task1" {
			t.Errorf("Prompt = %q, want %q", tasks[0].Prompt, "task1")
		}
		if tasks[0].State != StatePurged {
			t.Errorf("State = %v, want %v", tasks[0].State, StatePurged)
		}
	})
	t.Run("V2ControlsMatchV1HeaderSemantics", func(t *testing.T) {
		t.Parallel()
		v1Meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "control task", Harness: harness.Codex,
		})
		v2Meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "control task", Harness: harness.Codex,
		})
		v1Lines := []string{
			v1Meta,
			mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "session-1", Model: "model-1", AgentVersion: "agent-1"}),
			mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "owner", ForgeRepo: "repo", ForgePR: 7}),
			mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "main.go", Added: 2, Deleted: 1}}, Ts: 2_000_000_000}),
			`{"type":"assistant","text":"conversation"}`,
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Title: "done", CostUSD: 1.25, Duration: 2.5, NumTurns: 3}),
		}
		v2Lines := []string{
			v2Meta,
			`{"t":"session","session_id":"session-1","model":"model-1","agent_version":"agent-1"}`,
			`{"t":"pr","forge_owner":"owner","forge_repo":"repo","forge_pr":7}`,
			`{"t":"diff_stat","diff_stat":[{"path":"main.go","added":2,"deleted":1}],"ts":2000000000}`,
			`{"t":"agent","ts":1.000,"msg":{"type":"assistant","text":"conversation"}}`,
			`{"t":"result","state":"purged","title":"done","cost_usd":1.25,"duration":2.5,"num_turns":3}`,
		}
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				load := func(t *testing.T, name string, lines []string) *LoadedTask {
					dir := t.TempDir()
					if compressed {
						name += logCompressedExt
						writeCompressedLogFile(t, dir, name, seqOf(lines...))
					} else {
						writeLogFile(t, dir, name, lines...)
					}
					loaded, err := loadLogHeader(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					return loaded
				}
				v1 := load(t, "v1.jsonl", v1Lines)
				v2 := load(t, "v2.jsonl", v2Lines)
				if v2.SessionID != v1.SessionID || v2.Model != v1.Model || v2.AgentVersion != v1.AgentVersion {
					t.Fatalf("session metadata v2 = (%q, %q, %q), v1 = (%q, %q, %q)", v2.SessionID, v2.Model, v2.AgentVersion, v1.SessionID, v1.Model, v1.AgentVersion)
				}
				if v2.ForgeOwner != v1.ForgeOwner || v2.ForgeRepo != v1.ForgeRepo || v2.ForgePR != v1.ForgePR {
					t.Fatalf("PR metadata v2 = (%q, %q, %d), v1 = (%q, %q, %d)", v2.ForgeOwner, v2.ForgeRepo, v2.ForgePR, v1.ForgeOwner, v1.ForgeRepo, v1.ForgePR)
				}
				if !v2.DiffCreated || v2.LastStateUpdateAt != v1.LastStateUpdateAt {
					t.Fatalf("diff metadata v2 = (%t, %v), v1 = (%t, %v)", v2.DiffCreated, v2.LastStateUpdateAt, v1.DiffCreated, v1.LastStateUpdateAt)
				}
				if v2.Result == nil || v1.Result == nil || v2.Result.State != v1.Result.State || v2.Result.CostUSD != v1.Result.CostUSD || v2.Result.Duration != v1.Result.Duration || v2.Result.NumTurns != v1.Result.NumTurns {
					t.Fatalf("result metadata v2 = %#v, v1 = %#v", v2.Result, v1.Result)
				}
				for _, loaded := range []*LoadedTask{v1, v2} {
					if loaded.Msgs != nil {
						t.Fatalf("inventory messages = %#v, want nil for lazy semantic loading", loaded.Msgs)
					}
					calls := 0
					if err := loaded.LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func(raw []byte) ([]agent.Message, error) {
							calls++
							if !json.Valid(raw) {
								return nil, errors.New("invalid test conversation")
							}
							return []agent.Message{&agent.TextMessage{Text: "conversation"}}, nil
						}, nil
					}); err != nil {
						t.Fatal(err)
					}
					if calls != 1 || len(loaded.Msgs) != 1 {
						t.Fatalf("lazy semantic load calls/messages = %d/%#v, want 1/one conversation", calls, loaded.Msgs)
					}
				}
				if compressed {
					cached, err := loadLogHeader(v2.path)
					if err != nil {
						t.Fatal(err)
					}
					if cached.SessionID != v2.SessionID || cached.AgentVersion != v2.AgentVersion || cached.ForgePR != v2.ForgePR || cached.DiffCreated != v2.DiffCreated || cached.Result == nil || cached.Result.State != v2.Result.State {
						t.Fatalf("cached v2 control metadata = %#v, want %#v", cached, v2)
					}
					if cached.Msgs != nil {
						t.Fatalf("cached inventory messages = %#v, want nil", cached.Msgs)
					}
				}
			})
		}
	})
	t.Run("V2NativeTailBackfillMatchesV1", func(t *testing.T) {
		t.Parallel()
		v1Meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "native metadata", Harness: harness.Claude})
		v2Meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "native metadata", Harness: harness.Claude})
		v1Lines := []string{
			v1Meta,
			`{"type":"system","subtype":"init","model":"native-model","claude_code_version":"native-version"}`,
			`{"type":"result","total_cost_usd":1.25,"duration_ms":2500,"num_turns":3,"usage":{"input_tokens":4}}`,
			`{"type":"caic_result","state":"purged"}`,
		}
		v2Lines := []string{
			v2Meta,
			`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","model":"native-model","claude_code_version":"native-version"}}`,
			`{"t":"agent","ts":2.000,"msg":{"type":"result","total_cost_usd":1.25,"duration_ms":2500,"num_turns":3,"usage":{"input_tokens":4}}}`,
			`{"t":"result","state":"purged"}`,
		}
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				load := func(t *testing.T, name string, lines []string) *LoadedTask {
					dir := t.TempDir()
					if compressed {
						name += logCompressedExt
						writeCompressedLogFile(t, dir, name, seqOf(lines...))
					} else {
						writeLogFile(t, dir, name, lines...)
					}
					loaded, err := loadLogHeader(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					return loaded
				}
				v1 := load(t, "v1.jsonl", v1Lines)
				v2 := load(t, "v2.jsonl", v2Lines)
				if v2.Model != v1.Model || v2.AgentVersion != v1.AgentVersion {
					t.Fatalf("native init metadata v2 = (%q, %q), v1 = (%q, %q)", v2.Model, v2.AgentVersion, v1.Model, v1.AgentVersion)
				}
				if v2.Result == nil || v1.Result == nil || v2.Result.CostUSD != v1.Result.CostUSD || v2.Result.Duration != v1.Result.Duration || v2.Result.NumTurns != v1.Result.NumTurns || v2.Result.Usage != v1.Result.Usage {
					t.Fatalf("native result backfill v2 = %#v, v1 = %#v", v2.Result, v1.Result)
				}
				if v2.Msgs != nil {
					t.Fatalf("inventory messages = %#v, want nil", v2.Msgs)
				}
			})
		}
	})
	t.Run("MalformedControlsFailClosed", func(t *testing.T) {
		t.Parallel()
		for _, version := range []agent.LogVersion{agent.LogVersionV1, agent.LogVersionV2} {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: int(version), Prompt: "control task", Harness: harness.Codex,
			})
			for _, tc := range []struct {
				name string
				v1   string
				v2   string
			}{
				{name: "session", v1: `{"type":"caic_session","session_id":1}`, v2: `{"t":"session","session_id":"session-1","bogus":true}`},
				{name: "PR", v1: `{"type":"caic_pr","forge_pr":"bad"}`, v2: `{"t":"pr","forge_pr":7,"bogus":true}`},
				{name: "diff", v1: `{"type":"caic_diff_stat","diff_stat":true}`, v2: `{"t":"diff_stat","diff_stat":[],"bogus":true}`},
				{name: "result", v1: `{"type":"caic_result","state":"purged","cost_usd":"bad"}`, v2: `{"t":"result","state":"purged","bogus":true}`},
			} {
				control := tc.v1
				if version == agent.LogVersionV2 {
					control = tc.v2
				}
				for _, compressed := range []bool{false, true} {
					format := "plain"
					if compressed {
						format = "zstd"
					}
					t.Run(fmt.Sprintf("v%d/%s/%s", version, format, tc.name), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, compressed, meta, control)
						if _, err := loadLogHeader(path); err == nil {
							t.Fatal("loadLogHeader accepted malformed control")
						}
					})
				}
			}
		}
	})

	t.Run("V2MalformedNativeMessagesFailClosed", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "native task", Harness: harness.Codex,
		})
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, meta, `{"t":"agent","ts":1700000000.123,"msg":{"type":"assistant"} trailing}`)
				if _, err := loadLogHeader(path); err == nil || !strings.Contains(err.Error(), "invalid native JSON value") {
					t.Fatalf("loadLogHeader error = %v, want malformed v2 native message rejection", err)
				}
				tasks, err := LoadLogs(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 0 {
					t.Fatalf("persistent inventory accepted malformed v2 native message: %#v", tasks)
				}
			})
		}
	})
	t.Run("LaunchConfigMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType:       "caic_meta",
			Version:           1,
			Prompt:            "task1",
			Repos:             []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
			Harness:           "claude",
			BaseImage:         "ghcr.io/caic/base:v1",
			ContainerPlatform: "linux/amd64",
			MaxCPUs:           6,
			CacheMounts:       []agent.MetaCacheMount{{Name: "npm", Description: "Node", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true, Shallow: true}},
			Mounts:            []agent.MetaMount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
		})
		writeLogFile(t, dir, "a.jsonl", meta)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.BaseImage != "ghcr.io/caic/base:v1" || lt.ContainerPlatform != "linux/amd64" || lt.MaxCPUs != 6 {
			t.Fatalf("launch config = image %q platform %q cpus %d", lt.BaseImage, lt.ContainerPlatform, lt.MaxCPUs)
		}
		if len(lt.CacheMounts) != 1 || lt.CacheMounts[0].Name != "npm" || !lt.CacheMounts[0].ReadOnly || !lt.CacheMounts[0].Shallow {
			t.Errorf("CacheMounts = %+v", lt.CacheMounts)
		}
		if len(lt.Mounts) != 1 || lt.Mounts[0].HostPath != "/host/work" || !lt.Mounts[0].ReadOnly {
			t.Errorf("Mounts = %+v", lt.Mounts)
		}
	})
	t.Run("DiffCreatedStickyAcrossEmptyTail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		withDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "a.go", Added: 3, Deleted: 1}}, Ts: 1})
		// A later empty diff (agent committed, working tree clean) must not clear it.
		emptyDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", Ts: 2})
		writeLogFile(t, dir, "a.jsonl", meta, withDiff, emptyDiff)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if !tasks[0].DiffCreated {
			t.Error("DiffCreated = false, want true after a non-empty diff followed by an empty one")
		}
	})
	t.Run("DiffCreatedFalseWithoutDiff", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		emptyDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", Ts: 1})
		writeLogFile(t, dir, "a.jsonl", meta, emptyDiff)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].DiffCreated {
			t.Error("DiffCreated = true, want false when only empty diffs were recorded")
		}
	})
	t.Run("ResultReasoningTokens", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", ReasoningOutputTokens: 123})
		writeLogFile(t, dir, "a.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Result == nil {
			t.Fatal("Result is nil")
		}
		if tasks[0].Result.Usage.ReasoningOutputTokens != 123 {
			t.Errorf("ReasoningOutputTokens = %d, want 123", tasks[0].Result.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("ValidCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octo", ForgeRepo: "repo", ForgePR: 7})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta, asst, prMsg, trailer))

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.Prompt != "compressed" {
			t.Errorf("Prompt = %q, want compressed", lt.Prompt)
		}
		if lt.State != StatePurged {
			t.Errorf("State = %v, want StatePurged", lt.State)
		}
		if lt.ForgePR != 7 {
			t.Errorf("ForgePR = %d, want 7", lt.ForgePR)
		}
		if !isLogCompressed(lt.LogPath()) {
			t.Errorf("LogPath = %q, want compressed path", lt.LogPath())
		}
	})
	t.Run("CompressedSummaryCache", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl.zst")
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "cached", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", ForkedFromTaskID: "3BL0EKDTO000"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Error: "persisted failure"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta, trailer))

		first, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 1 {
			t.Fatalf("len(first) = %d, want 1", len(first))
		}
		if first[0].LogVersion != agent.LogVersionV1 {
			t.Fatalf("LogVersion = %d, want 1", first[0].LogVersion)
		}
		if _, err := os.Stat(logSummaryPath(path)); err != nil {
			t.Fatalf("summary cache was not written: %v", err)
		}
		summaryData, err := os.ReadFile(logSummaryPath(path))
		if err != nil {
			t.Fatal(err)
		}
		var summary logSummary
		if err := json.Unmarshal(summaryData, &summary); err != nil { //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
			t.Fatal(err)
		}
		if summary.Version != logSummaryVersion {
			t.Fatalf("taskmeta version = %d, want %d", summary.Version, logSummaryVersion)
		}
		if summary.Task == nil || len(summary.Task.Msgs) != 0 {
			t.Fatalf("taskmeta retained message history: %#v", summary.Task)
		}
		if snapshot := first[0].ValidatedSnapshot(); snapshot == nil || summary.Proof != snapshot.cacheProof() {
			t.Fatalf("taskmeta proof = %#v, want shared snapshot proof %#v", summary.Proof, snapshot)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		second, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 1 {
			t.Fatalf("len(second) = %d, want 1", len(second))
		}
		if second[0].Prompt != "cached" || second[0].State != StatePurged || second[0].ForkedFromTaskID != "3BL0EKDTO000" {
			t.Fatalf("cached task = prompt %q state %v parent %q, want cached/purged/3BL0EKDTO000", second[0].Prompt, second[0].State, second[0].ForkedFromTaskID)
		}
		if second[0].Result == nil || second[0].Result.Err == nil || second[0].Result.Err.Error() != "persisted failure" {
			t.Fatalf("cached result error = %#v, want persisted failure", second[0].Result)
		}
		if snapshot := second[0].ValidatedSnapshot(); snapshot == nil || !snapshot.EOFValidated || snapshot.RawHeader != meta {
			t.Fatalf("inventory cache hit snapshot = %#v, want current summary-backed EOF proof", snapshot)
		}

		replacementDir := t.TempDir()
		var replacement []byte
		replacementPrompt := ""
		for _, prompt := range []string{"newval", "reload", "replcd", "freshx"} {
			candidateName := prompt + ".jsonl.zst"
			candidateMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: prompt, Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", ForkedFromTaskID: "3BL0EKDTO000"})
			writeCompressedLogFile(t, replacementDir, candidateName, seqOf(candidateMeta, trailer))
			candidate, readErr := os.ReadFile(filepath.Join(replacementDir, candidateName)) //nolint:gosec // path is test-controlled.
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(candidate) == int(info.Size()) {
				replacement = candidate
				replacementPrompt = prompt
				break
			}
		}
		if replacement == nil {
			t.Fatal("could not construct a same-size compressed replacement")
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil { //nolint:gosec // path is test-controlled.
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		rebuilt, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(rebuilt) != 1 || rebuilt[0].Prompt != replacementPrompt {
			t.Fatalf("same-size same-mtime rewrite = %+v, want rebuilt prompt %q", rebuilt, replacementPrompt)
		}
		if rebuilt[0].ValidatedSnapshot() == nil {
			t.Fatal("rebuilt compressed log has no validated snapshot")
		}
	})
	t.Run("CompressedSummaryRejectsReplacementBeforePublish", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl.zst")
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "physical", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta))
		tasks, err := LoadLogs(dir)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("LoadLogs = %d tasks, %v", len(tasks), err)
		}
		if err := os.Remove(logSummaryPath(path)); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		moved := path + ".moved"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(moved) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // path is test-controlled.
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := storeLogSummaryForFile(tasks[0], file, info); err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("storeLogSummaryForFile error = %v, want replacement", err)
		}
		if _, err := os.Stat(logSummaryPath(path)); !os.IsNotExist(err) {
			t.Fatalf("summary exists after replacement: %v", err)
		}
	})
	t.Run("CompressedSummaryAtomicWriteFailure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl.zst")
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "atomic", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
		writeCompressedLogFile(t, dir, filepath.Base(path), seqOf(meta))
		tasks, err := LoadLogs(dir)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("LoadLogs = %d tasks, %v", len(tasks), err)
		}
		summaryPath := logSummaryPath(path)
		before, err := os.ReadFile(summaryPath) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		tmp := summaryPath + ".tmp"
		if err := os.Mkdir(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := storeLogSummary(tasks[0]); err == nil {
			t.Fatal("storeLogSummary error = nil, want atomic temp-file failure")
		}
		after, err := os.ReadFile(summaryPath) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("summary changed after atomic write failure")
		}
	})
	t.Run("CompressedSummaryCacheRebuild", func(t *testing.T) {
		t.Parallel()
		for _, mutation := range []string{"missing", "corrupt", "old", "stale-size", "stale-mtime", "stale-authority", "stale-device", "stale-inode", "stale-prompt", "eof"} {
			t.Run(mutation, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := filepath.Join(dir, "a.jsonl.zst")
				meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 2, Prompt: "physical", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex})
				writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta))
				if tasks, err := LoadLogs(dir); err != nil || len(tasks) != 1 {
					t.Fatalf("initial LoadLogs = %d tasks, %v", len(tasks), err)
				}
				summaryPath := logSummaryPath(path)
				switch mutation {
				case "missing":
					if err := os.Remove(summaryPath); err != nil {
						t.Fatal(err)
					}
				case "corrupt":
					if err := os.WriteFile(summaryPath, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "old", "stale-size", "stale-mtime", "stale-authority", "stale-device", "stale-inode", "stale-prompt", "eof":
					data, err := os.ReadFile(summaryPath) //nolint:gosec // path is test-controlled.
					if err != nil {
						t.Fatal(err)
					}
					var summary logSummary
					if err := json.Unmarshal(data, &summary); err != nil { //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
						t.Fatal(err)
					}
					switch mutation {
					case "old":
						summary.Version--
					case "stale-size":
						summary.Proof.Size++
					case "stale-mtime":
						summary.Proof.ModTimeNs--
					case "stale-authority":
						summary.Proof.Harness = harness.Claude
					case "stale-device":
						summary.Proof.Device++
					case "stale-inode":
						summary.Proof.Inode++
					case "stale-prompt":
						summary.Task.Prompt = "tampered"
					case "eof":
						summary.Proof = CacheProof{}
					}
					data, err = json.Marshal(summary) //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(summaryPath, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}

				tasks, err := LoadLogs(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 || tasks[0].Prompt != "physical" || tasks[0].LogVersion != agent.LogVersionV2 || tasks[0].Harness != harness.Codex {
					t.Fatalf("rebuilt task = %+v", tasks)
				}
				data, err := os.ReadFile(summaryPath) //nolint:gosec // path is test-controlled.
				if err != nil {
					t.Fatal(err)
				}
				var rebuilt logSummary
				if err := json.Unmarshal(data, &rebuilt); err != nil { //nolint:musttag // LoadedTask is intentionally the direct sidecar projection.
					t.Fatal(err)
				}
				if rebuilt.Version != logSummaryVersion || rebuilt.Task.LogVersion != agent.LogVersionV2 || rebuilt.Task.Harness != harness.Codex {
					t.Fatalf("rebuilt summary = %+v", rebuilt)
				}
			})
		}
	})
	t.Run("PreferCompressedDuplicate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		plainMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plain", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		compressedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(compressedMeta, trailer))
		writeLogFile(t, dir, "a.jsonl", plainMeta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Prompt != "compressed" {
			t.Errorf("Prompt = %q, want compressed", tasks[0].Prompt)
		}
	})
	t.Run("ForTaskIDs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wantedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "wanted", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		unrelatedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "unrelated", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		writeLogFile(t, dir, "live1-repo-branch.jsonl", wantedMeta)
		writeLogFile(t, dir, "live10-repo-branch.jsonl", unrelatedMeta)

		tasks, err := LoadLogsForTaskIDs(dir, []string{"live1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].TaskID != "live1" || tasks[0].Prompt != "wanted" {
			t.Errorf("task = (%q, %q), want (live1, wanted)", tasks[0].TaskID, tasks[0].Prompt)
		}
	})
	t.Run("ForTaskIDsMissing", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadLogsForTaskIDs(t.TempDir(), []string{"missing"}); err == nil || !strings.Contains(err.Error(), "missing task logs") {
			t.Fatalf("LoadLogsForTaskIDs error = %v, want missing-log error", err)
		}
	})
	t.Run("ForTaskIDsInvalid", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeLogFile(t, dir, "broken-repo-branch.jsonl", `{"type":"assistant"}`)
		_, err := LoadLogsForTaskIDs(dir, []string{"broken"})
		if err == nil || !strings.Contains(err.Error(), "load task log") {
			t.Fatalf("LoadLogsForTaskIDs error = %v, want invalid-log error", err)
		}
	})
	t.Run("NotExist", func(t *testing.T) {
		t.Parallel()
		tasks, err := LoadLogs(filepath.Join(t.TempDir(), "nope"))
		if err != nil {
			t.Fatal(err)
		}
		if tasks != nil {
			t.Error("expected nil for nonexistent dir")
		}
	})
	t.Run("BadHeader", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeLogFile(t, dir, "bad.jsonl", `{"type":"not_meta"}`)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 0 {
			t.Errorf("len = %d, want 0", len(tasks))
		}
	})
	t.Run("MultipleFiles", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "first", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		asst1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		writeLogFile(t, dir, "a.jsonl", meta1, asst1)

		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "second", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		init2 := claudeInit(t, "sid-2")
		asst2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		writeLogFile(t, dir, "b.jsonl", meta2, init2, asst2)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 2 {
			t.Fatalf("len = %d, want 2", len(tasks))
		}
		// Sorted by StartedAt ascending.
		if tasks[0].Prompt != "first" {
			t.Errorf("tasks[0].Prompt = %q, want %q", tasks[0].Prompt, "first")
		}
		if tasks[1].Prompt != "second" {
			t.Errorf("tasks[1].Prompt = %q, want %q", tasks[1].Prompt, "second")
		}
		// Msgs are nil until LoadMessages is called.
		if tasks[0].Msgs != nil {
			t.Error("tasks[0].Msgs should be nil before LoadMessages")
		}
		setClaudeParser(tasks)
		for _, lt := range tasks {
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
		}
		// Each task has its own messages, not merged.
		// asst1 produces 1 TextMessage.
		if len(tasks[0].Msgs) != 1 {
			t.Errorf("tasks[0].Msgs len = %d, want 1", len(tasks[0].Msgs))
		}
		// init2 produces 1 InitMessage; asst2 produces 1 TextMessage = 2 total.
		if len(tasks[1].Msgs) != 2 {
			t.Errorf("tasks[1].Msgs len = %d, want 2", len(tasks[1].Msgs))
		}
	})
	t.Run("FeatureFlagsAllSet", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "feat task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
			Model: "model-1", Effort: "high",
			Tailscale: true, USB: true, Display: true, Sudo: true, GitHubToken: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "feat.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if !lt.Tailscale {
			t.Error("Tailscale = false, want true")
		}
		if !lt.USB {
			t.Error("USB = false, want true")
		}
		if !lt.Display {
			t.Error("Display = false, want true")
		}
		if lt.Model != "model-1" {
			t.Errorf("Model = %q, want model-1", lt.Model)
		}
		if lt.Effort != "high" {
			t.Errorf("Effort = %q, want high", lt.Effort)
		}
		if !lt.Sudo {
			t.Error("Sudo = false, want true")
		}
		if !lt.GitHubToken {
			t.Error("GitHubToken = false, want true")
		}
	})
	t.Run("FeatureFlagsOmitted", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "plain task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "plain.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.Tailscale {
			t.Error("Tailscale = true, want false")
		}
		if lt.USB {
			t.Error("USB = true, want false")
		}
		if lt.Display {
			t.Error("Display = true, want false")
		}
	})
	t.Run("FeatureFlagsPartial", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "usb only",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
			USB: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "partial.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.Tailscale {
			t.Error("Tailscale = true, want false")
		}
		if !lt.USB {
			t.Error("USB = false, want true")
		}
		if lt.Display {
			t.Error("Display = true, want false")
		}
	})
	t.Run("SessionMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "session task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		session := mustJSON(t, agent.MetaSessionMessage{
			MessageType:  "caic_session",
			SessionID:    "thread-1",
			Model:        "gpt-5.4",
			AgentVersion: "1.2.3",
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "session.jsonl", meta, session, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.SessionID != "thread-1" {
			t.Errorf("SessionID = %q, want thread-1", lt.SessionID)
		}
		if lt.Model != "gpt-5.4" {
			t.Errorf("Model = %q, want gpt-5.4", lt.Model)
		}
		if lt.AgentVersion != "1.2.3" {
			t.Errorf("AgentVersion = %q, want 1.2.3", lt.AgentVersion)
		}
	})
	t.Run("V1NativeSessionTypeIsNotTaskMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "session task", Harness: harness.Claude,
		})
		nativeSession := `{"type":"session","session_id":"native-session","version":"native"}`
		caicSession := mustJSON(t, agent.MetaSessionMessage{
			MessageType: "caic_session", SessionID: "caic-session", AgentVersion: "1.2.3",
		})
		path := filepath.Join(dir, "session.jsonl")
		writeLogFile(t, dir, filepath.Base(path), meta, nativeSession, caicSession)

		parsedNative := false
		parseNativeSession := func(line []byte) ([]agent.Message, error) {
			if len(line) == 0 {
				return nil, errors.New("empty native record")
			}
			if bytes.Contains(line, []byte(`"type":"session"`)) {
				parsedNative = true
			}
			return []agent.Message{&agent.TextMessage{Text: "native session record"}}, nil
		}
		lt, _, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
			if h != harness.Claude {
				t.Fatalf("resolver harness = %q, want claude", h)
			}
			return parseNativeSession, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !parsedNative {
			t.Fatal("v1 native session record did not reach the harness parser")
		}
		if lt.SessionID != "caic-session" || lt.AgentVersion != "1.2.3" {
			t.Fatalf("session metadata = (%q, %q), want (caic-session, 1.2.3)", lt.SessionID, lt.AgentVersion)
		}
	})
	t.Run("V2CaicSessionAliasesAreNotTaskMetadata", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 2, Prompt: "session task", Harness: harness.Claude,
		})
		caicSession := `{"t":"caic_session","session_id":"wrong-session","model":"wrong-model","version":"wrong-version"}`
		caicInit := `{"t":"caic_init","session_id":"wrong-init","model":"wrong-model","version":"wrong-version"}`
		path := writePhysicalTestLog(t, false, meta, caicSession, caicInit)

		_, _, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
			if h != harness.Claude {
				t.Fatalf("resolver harness = %q, want claude", h)
			}
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err == nil || !strings.Contains(err.Error(), "unknown top-level t") {
			t.Fatalf("v2 caic_session alias error = %v, want strict unknown-token rejection", err)
		}
		tasks, err := LoadLogs(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 0 {
			t.Fatalf("persistent inventory accepted malformed v2 controls: %#v", tasks)
		}
	})
	t.Run("AgentVersionMetadataWithoutSession", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "pi task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Pi,
		})
		session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", AgentVersion: "pi 1.2.3"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "pi.jsonl", meta, session, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].AgentVersion != "pi 1.2.3" {
			t.Errorf("AgentVersion = %q, want pi 1.2.3", tasks[0].AgentVersion)
		}
	})
	t.Run("LoadSessionMetadataScansBeyondTail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "long task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "thread-old"})
		large := `{"text":"` + strings.Repeat("x", 70<<10) + `"}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "long.jsonl", meta, session, large, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return codex.New("", nil).NewWire().ParseMessage, nil
		})
		if lt.SessionID != "thread-old" {
			t.Fatalf("SessionID = %q after authority scan, want thread-old", lt.SessionID)
		}
		if err := lt.LoadSessionMetadata(); err != nil {
			t.Fatal(err)
		}
		if lt.SessionID != "thread-old" {
			t.Errorf("SessionID = %q, want thread-old", lt.SessionID)
		}
	})
	t.Run("LoadSessionMetadataEnforcesAuthority", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "session task", Harness: harness.Claude,
		})
		mismatch := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 2, Prompt: "session task", Harness: harness.Claude,
		})
		session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "session-1"})
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format+" missing header", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, session)
				if _, _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				}); err == nil || !strings.Contains(err.Error(), "invalid first log header") {
					t.Fatalf("error = %v, want invalid first header", err)
				}
			})
			t.Run(format+" mixed authority after metadata", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, meta, session, mismatch)
				if _, _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				}); err == nil || !strings.Contains(err.Error(), "wrong t discriminator") {
					t.Fatalf("error = %v, want wrong t discriminator", err)
				}
			})
			t.Run(format+" leading empty lines", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, "", "  ", meta, session)
				lt, _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if lt.SessionID != "session-1" {
					t.Fatalf("SessionID = %q, want session-1", lt.SessionID)
				}
			})
		}
	})
	t.Run("LoadSessionMetadataScansLegacyInitMessage", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "legacy codex task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		init := `{"method":"thread/started","params":{"thread":{"id":"thread-from-started","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "legacy-codex.jsonl", meta, init, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return codex.New("", nil).NewWire().ParseMessage, nil
		})
		if err := lt.LoadSessionMetadata(); err != nil {
			t.Fatal(err)
		}
		if lt.SessionID != "thread-from-started" {
			t.Errorf("SessionID = %q, want thread-from-started", lt.SessionID)
		}
	})
	t.Run("LegacyCaicInitSessionMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "legacy task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.OpenCode,
		})
		init := `{"type":"caic_init","session_id":"ses-legacy","model":"m","version":"v"}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "legacy.jsonl", meta, init, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.SessionID != "ses-legacy" {
			t.Errorf("SessionID = %q, want ses-legacy", lt.SessionID)
		}
		if lt.Model != "m" || lt.AgentVersion != "v" {
			t.Errorf("model/version = %q/%q, want m/v", lt.Model, lt.AgentVersion)
		}
	})
	t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		// Old session: agent enters plan mode and writes a plan file.
		planWrite := claudeAssistant(t, map[string]any{
			"type":  "tool_use",
			"id":    "tu1",
			"name":  "Write",
			"input": map[string]any{"file_path": "/home/user/.claude/plans/p.md", "content": "the plan"},
		})
		// context_cleared written by RestartSession before starting new session.
		cleared := mustJSON(t, agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"})
		// New session header + assistant message (no plan tools).
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst2 := claudeAssistant(t, map[string]any{"type": "text", "text": "done"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "task.jsonl", meta, planWrite, cleared, meta2, asst2, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		// After restore, plan state must be empty because context_cleared resets it.
		tk := &Task{InitialPrompt: agent.Prompt{Text: lt.Prompt}}
		tk.SetState(StateRunning)
		tk.RestoreMessages(lt.Msgs)
		snap := tk.Snapshot()
		if snap.InPlanMode {
			t.Error("InPlanMode = true, want false")
		}
		if snap.PlanContent != "" {
			t.Errorf("PlanContent = %q, want empty", snap.PlanContent)
		}
	})
	t.Run("PRHeaderOnly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octocat", ForgeRepo: "hello", ForgePR: 42})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "1-r-caic-1.jsonl", meta, prMsg, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.ForgeOwner != "octocat" {
			t.Errorf("ForgeOwner = %q, want %q", lt.ForgeOwner, "octocat")
		}
		if lt.ForgeRepo != "hello" {
			t.Errorf("ForgeRepo = %q, want %q", lt.ForgeRepo, "hello")
		}
		if lt.ForgePR != 42 {
			t.Errorf("ForgePR = %d, want 42", lt.ForgePR)
		}
	})
	t.Run("PRFullParse", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-2"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "done"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "org", ForgeRepo: "repo", ForgePR: 99})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "2-r-caic-2.jsonl", meta, asst, prMsg, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		// Header-only parse should find PR in tail.
		if lt.ForgePR != 99 {
			t.Errorf("ForgePR = %d, want 99 (header parse)", lt.ForgePR)
		}
		// Full parse via LoadMessages should also find it.
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if lt.ForgePR != 99 {
			t.Errorf("ForgePR = %d, want 99 (full parse)", lt.ForgePR)
		}
	})
	t.Run("PROutsideTailWindow", func(t *testing.T) {
		t.Parallel()
		// caic_pr is early in the file, followed by >64 KiB of messages.
		// Full parser traversal derives its metadata even though tail messages
		// remain bounded.
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-3"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "widget", ForgePR: 77})

		// Build lines: header, caic_pr, then enough assistant messages
		// to place caic_pr outside the retained tail window.
		lines := make([]string, 0, 83)
		lines = append(lines, meta, prMsg)
		bigText := string(make([]byte, 1024)) // 1 KiB of null bytes per message
		for range 80 {                        // 80 KiB of filler
			lines = append(lines, claudeAssistant(t, map[string]any{"type": "text", "text": bigText}))
		}
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		lines = append(lines, trailer)
		writeLogFile(t, dir, "3-r-caic-3.jsonl", lines...)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		// The full traversal derives PR metadata before tail messages are retained.
		if lt.ForgePR != 77 {
			t.Fatalf("ForgePR = %d, want 77", lt.ForgePR)
		}
		if lt.ForgeOwner != "acme" {
			t.Fatalf("ForgeOwner = %q, want acme", lt.ForgeOwner)
		}
		if lt.ForgeRepo != "widget" {
			t.Fatalf("ForgeRepo = %q, want widget", lt.ForgeRepo)
		}
		// Full parse via LoadMessages retains the same PR metadata.
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if lt.ForgePR != 77 {
			t.Errorf("ForgePR = %d after LoadMessages, want 77", lt.ForgePR)
		}
		if lt.ForgeOwner != "acme" {
			t.Errorf("ForgeOwner = %q, want %q", lt.ForgeOwner, "acme")
		}
		if lt.ForgeRepo != "widget" {
			t.Errorf("ForgeRepo = %q, want %q", lt.ForgeRepo, "widget")
		}
	})
}

func TestLoadSemanticLogSnapshot(t *testing.T) {
	t.Parallel()
	t.Run("LoadsNativeSessionMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "task", Harness: harness.Codex,
		})
		threadStarted := `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"thread-1","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
		writeLogFile(t, dir, "task.jsonl", meta, threadStarted)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := tasks[0].LoadSessionMetadataWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return codex.New("", nil).NewWire().ParseMessage, nil
		}); err != nil {
			t.Fatal(err)
		}
		if tasks[0].SessionID != "thread-1" {
			t.Errorf("SessionID = %q, want thread-1", tasks[0].SessionID)
		}
	})
	t.Run("SkipsUnparseableNativeRecord", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "task", Harness: harness.Claude,
		})
		valid := claudeAssistant(t, map[string]any{"type": "text", "text": "kept"})
		writeLogFile(t, dir, "task.jsonl", meta, `{"type":"assistant"`, valid)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := tasks[0].LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(tasks[0].Msgs) != 1 {
			t.Fatalf("Msgs = %d, want 1", len(tasks[0].Msgs))
		}
	})
	t.Run("RejectsIncompleteLaterMetaCandidate", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			header string
			record string
		}{
			{
				name:   "v1_type",
				header: `{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`,
				record: `{"type":"caic_meta"`,
			},
			{
				name:   "v1_t",
				header: `{"type":"caic_meta","version":1,"prompt":"task","repos":[],"harness":"claude"}`,
				record: `{"t":"caic_meta"`,
			},
			{
				name:   "v2_type",
				header: `{"t":"caic_meta","version":2,"prompt":"task","repos":[],"harness":"claude"}`,
				record: `{"type":"caic_meta"`,
			},
			{
				name:   "v2_t",
				header: `{"t":"caic_meta","version":2,"prompt":"task","repos":[],"harness":"claude"}`,
				record: `{"t":"caic_meta"`,
			},
		} {
			for _, record := range []struct {
				name string
				line string
			}{
				{name: "incomplete", line: tc.record},
				{name: "trailing", line: tc.record + `} trailing`},
			} {
				t.Run(tc.name+"_"+record.name, func(t *testing.T) {
					t.Parallel()
					for _, compressed := range []bool{false, true} {
						format := "plain"
						if compressed {
							format = "zstd"
						}
						t.Run(format, func(t *testing.T) {
							t.Parallel()
							path := writePhysicalTestLog(t, compressed, tc.header, record.line)
							_, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
								return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
							})
							if err == nil || !strings.Contains(err.Error(), "invalid log segment header") {
								t.Fatalf("loadSemanticLogSnapshot error = %v, want invalid segment header", err)
							}
						})
					}
				})
			}
		}
	})
	t.Run("PlainOrZstd", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "snapshot",
			Harness:     harness.Claude,
		})
		native := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				name := "snapshot" + logPlainExt
				if compressed {
					name = "snapshot" + logCompressedExt
				}
				if compressed {
					writeCompressedLogFile(t, dir, name, seqOf(meta, native))
				} else {
					writeLogFile(t, dir, name, meta, native)
				}

				var events []string
				factory := func(got harness.Name) (func([]byte) ([]agent.Message, error), error) {
					events = append(events, "factory:"+string(got))
					return func(line []byte) ([]agent.Message, error) {
						events = append(events, "native")
						return []agent.Message{&agent.TextMessage{Text: string(line)}}, nil
					}, nil
				}
				if len(events) != 0 {
					t.Fatalf("factory events before snapshot = %v, want none", events)
				}
				snapshot, err := loadSemanticLogSnapshot(filepath.Join(dir, name), factory)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := events, []string{"factory:claude", "native"}; !slices.Equal(got, want) {
					t.Fatalf("construction events = %v, want %v", got, want)
				}
				if len(snapshot.Messages) != 2 || snapshot.Messages[0].Message.Type() != "caic_meta" || snapshot.Messages[1].Message.Type() != "text" {
					t.Fatalf("snapshot messages = %#v, want bootstrap and one text message", snapshot.Messages)
				}
				if !snapshot.EOFValidated || snapshot.RawHeader != meta {
					t.Fatalf("snapshot proof = %#v, want validated exact header", snapshot)
				}
			})
		}
	})

	t.Run("Parallel", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "parallel", Harness: harness.Claude})
		writeLogFile(t, dir, "parallel.jsonl", meta, `{"type":"assistant","message":{"content":[]}}`)
		factory := func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) {
				return []agent.Message{&agent.TextMessage{Text: "parsed"}}, nil
			}, nil
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				snapshot, err := loadSemanticLogSnapshot(filepath.Join(dir, "parallel.jsonl"), factory)
				if err != nil {
					t.Errorf("loadSemanticLogSnapshot: %v", err)
					return
				}
				if len(snapshot.Messages) != 2 {
					t.Errorf("snapshot messages = %#v, want bootstrap and parsed message", snapshot.Messages)
				}
			})
		}
		wg.Wait()
	})

	t.Run("V2AgentRecords", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType:      "caic_meta",
			Version:          int(agent.LogVersionV2),
			Prompt:           "v2 snapshot",
			Harness:          harness.Codex,
			ForkedFromTaskID: "3BL0EKDTO000",
		})
		native := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`
		agentRecord := `{"t":"agent","ts":1700000000.123,"msg":` + native + `}`
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				name := "v2" + logPlainExt
				if compressed {
					name = "v2" + logCompressedExt
					writeCompressedLogFile(t, dir, name, seqOf(meta, agentRecord, meta))
				} else {
					writeLogFile(t, dir, name, meta, agentRecord, meta)
				}

				var factoryCalls, nativeCalls int
				factory := func(got harness.Name) (func([]byte) ([]agent.Message, error), error) {
					factoryCalls++
					if got != harness.Codex {
						t.Fatalf("factory harness = %q, want codex", got)
					}
					return func(line []byte) ([]agent.Message, error) {
						nativeCalls++
						if string(line) != native {
							t.Fatalf("native payload = %q, want %q", line, native)
						}
						return []agent.Message{&agent.TextMessage{Text: "hello"}}, nil
					}, nil
				}
				snapshot, err := loadSemanticLogSnapshot(filepath.Join(dir, name), factory)
				if err != nil {
					t.Fatal(err)
				}
				if factoryCalls != 1 || nativeCalls != 1 {
					t.Fatalf("factory calls = %d, native calls = %d, want 1/1", factoryCalls, nativeCalls)
				}
				if len(snapshot.Messages) != 3 {
					t.Fatalf("snapshot messages = %#v, want bootstrap, agent message, and segment", snapshot.Messages)
				}
				for _, index := range []int{0, 2} {
					parsed, ok := snapshot.Messages[index].Message.(*agent.MetaMessage)
					if !ok || parsed.ForkedFromTaskID != "3BL0EKDTO000" {
						t.Fatalf("metadata record %d = %#v, want forked_from_task_id", index, snapshot.Messages[index].Message)
					}
				}
				wantTime := time.Unix(1700000000, 123000000).UTC()
				if !snapshot.Messages[0].ProducerTime.IsZero() || !snapshot.Messages[1].ProducerTime.Equal(wantTime) || !snapshot.Messages[2].ProducerTime.IsZero() {
					t.Fatalf("producer times = %v, %v, %v, want zero/%v/zero", snapshot.Messages[0].ProducerTime, snapshot.Messages[1].ProducerTime, snapshot.Messages[2].ProducerTime, wantTime)
				}
			})
		}
	})

	t.Run("CompletesOnePlainOrZstdPass", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV2),
			Prompt:      "scan count",
			Harness:     harness.Codex,
		})
		native := `{"type":"assistant","message":{"content":[]}}`
		agentRecord := `{"t":"agent","ts":1700000000.123,"msg":` + native + `}`
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				name := "count" + logPlainExt
				if compressed {
					name = "count" + logCompressedExt
					writeCompressedLogFile(t, dir, name, seqOf(meta, agentRecord))
				} else {
					writeLogFile(t, dir, name, meta, agentRecord)
				}
				path := filepath.Join(dir, name)
				file, err := os.Open(filepath.Clean(path))
				if err != nil {
					t.Fatal(err)
				}
				info, err := file.Stat()
				if err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				var source io.Reader = file
				closeFn := file.Close
				if compressed {
					decoder, decodeErr := zstd.NewReader(file)
					if decodeErr != nil {
						_ = file.Close()
						t.Fatal(decodeErr)
					}
					source = decoder
					closeFn = func() error {
						decoder.Close()
						return file.Close()
					}
				}
				counted := &countingReadCloser{reader: source, closeFn: closeFn}
				reader := &physicalLogReader{file: file, reader: counted, info: info}
				nativeCalls := 0
				snapshot, err := loadSemanticLogSnapshotFromReader(path, reader, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) {
						nativeCalls++
						return []agent.Message{&agent.TextMessage{Text: "parsed"}}, nil
					}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !counted.complete || counted.bytes == 0 {
					t.Fatalf("source pass = bytes %d complete %t, want one complete pass", counted.bytes, counted.complete)
				}
				if nativeCalls != 1 || len(snapshot.Messages) != 2 {
					t.Fatalf("native calls = %d, messages = %d, want 1/2", nativeCalls, len(snapshot.Messages))
				}
			})
		}
	})
}

func TestTaskAdoptionReadAmplification(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "adopt",
		Harness:     harness.Claude,
	})
	segment := meta
	control := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", Ts: 1})

	for _, tc := range []struct {
		name            string
		topLevelSession bool
		lines           func(*testing.T) []string
	}{
		{
			name:            "top-level caic session",
			topLevelSession: true,
			lines: func(t *testing.T) []string {
				session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "adopted-session", AgentVersion: "2.1.0"})
				message := claudeAssistant(t, map[string]any{"type": "text", "text": "favorable adoption"})
				return []string{meta, session, message, control, segment}
			},
		},
		{
			name: "many small lines",
			lines: func(t *testing.T) []string {
				lines := []string{meta}
				message := claudeAssistant(t, map[string]any{"type": "text", "text": "small delta"})
				for i := range 8192 {
					lines = append(lines, message)
					if i%1024 == 0 {
						lines = append(lines, control, segment)
					}
				}
				return lines
			},
		},
		{
			name: "large tool output",
			lines: func(t *testing.T) []string {
				large := claudeAssistant(t, map[string]any{"type": "text", "text": strings.Repeat("tool output ", 256<<10)})
				return []string{meta, large, control, segment, large}
			},
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
				lines := tc.lines(t)
				tk := &Task{ID: ksid.NewID(), Harness: harness.Claude}
				name := taskLogFileName(tk)
				if compressed {
					name += logCompressedExt
					writeCompressedLogFile(t, dir, name, seqOf(lines...))
				} else {
					writeLogFile(t, dir, name, lines...)
				}
				path := filepath.Join(dir, name)

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
						logicalBytes += counted.bytes
						if counted.complete {
							completePasses++
						}
					}
				}

				headerReader, noteHeader := openCounted()
				lt, err := loadLogHeaderFromReader(path, headerReader, false, false)
				if err != nil {
					_ = headerReader.Close()
					t.Fatal(err)
				}
				if err := headerReader.Close(); err != nil {
					t.Fatal(err)
				}
				noteHeader()
				if tc.topLevelSession {
					if lt.SessionID != "adopted-session" || lt.AgentVersion != "2.1.0" {
						t.Fatalf("header scan session metadata = (%q, %q), want adopted-session/2.1.0", lt.SessionID, lt.AgentVersion)
					}
				} else {
					if lt.SessionID != "" || lt.AgentVersion != "" {
						t.Fatalf("header scan found top-level session metadata: (%q, %q)", lt.SessionID, lt.AgentVersion)
					}

					sessionReader, noteSession := openCounted()
					session, _, err := loadSemanticSessionMetadataFromReader(path, sessionReader, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
						if h != harness.Claude {
							t.Fatalf("resolver harness = %q, want %q", h, harness.Claude)
						}
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					noteSession()
					if session.SessionID != "" || session.AgentVersion != "" {
						t.Fatalf("parser-empty session metadata = (%q, %q), want empty", session.SessionID, session.AgentVersion)
					}
				}

				messageReader, noteMessages := openCounted()
				messageSnapshot, err := loadSemanticLogSnapshotFromReader(path, messageReader, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
					if h != harness.Claude {
						t.Fatalf("resolver harness = %q, want %q", h, harness.Claude)
					}
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				noteMessages()
				if err := applySemanticSnapshot(lt, semanticLoadedTask(messageSnapshot), messageSnapshot.validationProof(), true); err != nil {
					t.Fatal(err)
				}
				if lt.Msgs != nil {
					t.Fatalf("parser-empty message scan = %#v, want nil", lt.Msgs)
				}
				if err := lt.LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					t.Fatal("parser-empty semantic scan was repeated")
					return nil, errors.New("parser-empty semantic scan repeated")
				}); err != nil {
					t.Fatal(err)
				}
				var reopenProofBytes int64
				if !compressed {
					// Adoption gives Reopen the completed semantic proof, so its append
					// validation remains a bounded header/identity read rather than a
					// fourth complete pass. Measure that production append descriptor,
					// not an injected reader.
					reopenProofBytes = reopenWithProofReplay(t, dir, path, tk, messageSnapshot)
				}

				wantCompletePasses := 3
				if tc.topLevelSession {
					wantCompletePasses = 2 // inventory/header and semantic message scans
				}
				if completePasses != wantCompletePasses {
					t.Errorf("complete passes = %d, want exactly %d", completePasses, wantCompletePasses)
				}
				logicalSize := int64(len(strings.Join(lines, "\n")) + 1)
				if logicalBytes != int64(wantCompletePasses)*logicalSize {
					t.Errorf("logical bytes = %d, want exactly %d complete passes of %d", logicalBytes, wantCompletePasses, logicalSize)
				}
				if !compressed && (reopenProofBytes == 0 || reopenProofBytes > min(int64(64<<10), logicalSize)) {
					t.Errorf("Reopen proof bytes = %d, want bounded header/identity tail in (0, %d]", reopenProofBytes, min(int64(64<<10), logicalSize))
				}
			})
		}
	}
}

func TestCompressTerminalLogs(t *testing.T) {
	t.Parallel()
	t.Run("Compresses", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", RuntimeName: "docker"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "t.jsonl", meta, asst, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := loadSemanticLogSnapshot(tasks[0].LogPath(), func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks[0].setValidatedSnapshot(snapshot)
		if err := CompressTerminalLogs(tasks); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "t.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("plain log stat err = %v, want os.ErrNotExist", err)
		}
		compressedPath := filepath.Join(dir, "t.jsonl.zst")
		if _, err := os.Stat(compressedPath); err != nil {
			t.Fatal(err)
		}
		if !isLogCompressed(tasks[0].LogPath()) {
			t.Fatalf("LogPath = %q, want compressed path", tasks[0].LogPath())
		}
		compressedSnapshot := tasks[0].ValidatedSnapshot()
		if compressedSnapshot == nil || !compressedSnapshot.EOFValidated || len(compressedSnapshot.Messages) != len(snapshot.Messages) {
			got := 0
			if compressedSnapshot != nil {
				got = len(compressedSnapshot.Messages)
			}
			t.Fatalf("compressed snapshot messages = %d, want EOF-validated %d", got, len(snapshot.Messages))
		}
		if _, err := tasks[0].CacheProofForLog(compressedPath); err != nil {
			t.Fatalf("compressed replay proof after validation: %v", err)
		}
		reloaded, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(reloaded) != 1 {
			t.Fatalf("reloaded len = %d, want 1", len(reloaded))
		}
		if reloaded[0].RuntimeName != "docker" {
			t.Fatalf("reloaded RuntimeName = %q, want docker", reloaded[0].RuntimeName)
		}
		if _, err := os.Stat(logSummaryPath(compressedPath)); err != nil {
			t.Fatalf("summary cache after compressed scan: %v", err)
		}
		setClaudeParser(tasks)
		if err := tasks[0].LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if len(tasks[0].Msgs) != 1 {
			t.Errorf("Msgs len = %d, want 1", len(tasks[0].Msgs))
		}
	})

	t.Run("IgnoresCrashTemporaryOutput", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tmp := compressedLogTempPath(filepath.Join(dir, "t.jsonl"))
		if IsLogName(filepath.Base(tmp)) {
			t.Fatalf("temporary compressed name %q is in the task-log namespace", filepath.Base(tmp))
		}
		if err := os.WriteFile(tmp, []byte("interrupted compression"), 0o600); err != nil {
			t.Fatal(err)
		}
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatalf("LoadLogs with crash temporary output = %v", err)
		}
		if len(tasks) != 0 {
			t.Fatalf("LoadLogs with crash temporary output = %d tasks, want 0", len(tasks))
		}
	})

	t.Run("KeepsSourceWhenTemporaryAuthorityValidationFails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Harness: harness.Claude})
		path := filepath.Join(dir, "t.jsonl")
		writeLogFile(t, dir, filepath.Base(path), meta)
		snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		invalid := *snapshot
		invalid.Authority.Version = agent.LogVersionV2
		if _, _, err := compressLogFileWithSnapshot(path, &invalid); err == nil || !strings.Contains(err.Error(), "authority differs") {
			t.Fatalf("compressLogFileWithSnapshot error = %v, want temporary authority validation failure", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("plain log stat = %v, want source preserved", err)
		}
		if _, err := os.Stat(compressedLogPath(path)); !os.IsNotExist(err) {
			t.Fatalf("compressed log stat = %v, want no promoted output", err)
		}
	})

	t.Run("RejectsStaleSnapshot", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		path := filepath.Join(dir, "t.jsonl")
		writeLogFile(t, dir, filepath.Base(path), meta, trailer)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(`{"type":"assistant"}` + "\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := CompressTerminalLogs(tasks); err == nil || !strings.Contains(err.Error(), "no current validated snapshot") {
			t.Fatalf("CompressTerminalLogs error = %v, want stale-snapshot error", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("plain log stat = %v, want source preserved", err)
		}
		if _, err := os.Stat(compressedLogPath(path)); !os.IsNotExist(err) {
			t.Fatalf("compressed log stat = %v, want no compressed output", err)
		}
	})
}

func TestCompressLogFileTransactionFailures(t *testing.T) {
	t.Parallel()
	const sourceContents = "task log contents\n"
	newSource := func(t *testing.T) (string, string, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte(sourceContents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path, compressedLogPath(path), compressedLogTempPath(path)
	}
	assertSourcePreserved := func(t *testing.T, path string) {
		t.Helper()
		got, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatalf("source read = %v, want preserved source", err)
		}
		if string(got) != sourceContents {
			t.Fatalf("source contents = %q, want %q", got, sourceContents)
		}
	}
	assertNotExist := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stat %q = %v, want os.ErrNotExist", path, err)
		}
	}
	newOps := func() compressionFileOps {
		return compressionFileOps{stat: os.Stat, rename: os.Rename, remove: os.Remove}
	}

	t.Run("FinalSourceValidation", func(t *testing.T) {
		t.Parallel()
		path, dst, tmp := newSource(t)
		ops := newOps()
		promoted := false
		ops.rename = func(oldPath, newPath string) error {
			err := os.Rename(oldPath, newPath)
			promoted = err == nil
			return err
		}
		ops.stat = func(name string) (os.FileInfo, error) {
			if name == path && promoted {
				return nil, errors.New("forced final source validation failure")
			}
			return os.Stat(name)
		}

		if _, _, err := compressLogFileWithSnapshotAndOps(path, nil, ops); err == nil || !strings.Contains(err.Error(), "forced final source validation failure") {
			t.Fatalf("compression error = %v, want final source validation failure", err)
		}
		assertSourcePreserved(t, path)
		assertNotExist(t, dst)
		assertNotExist(t, tmp)
	})

	t.Run("DestinationValidation", func(t *testing.T) {
		t.Parallel()
		path, dst, tmp := newSource(t)
		ops := newOps()
		destinationStats := 0
		ops.stat = func(name string) (os.FileInfo, error) {
			if name == dst {
				destinationStats++
				if destinationStats == 1 {
					return nil, errors.New("forced destination validation failure")
				}
			}
			return os.Stat(name)
		}

		if _, _, err := compressLogFileWithSnapshotAndOps(path, nil, ops); err == nil || !strings.Contains(err.Error(), "forced destination validation failure") {
			t.Fatalf("compression error = %v, want destination validation failure", err)
		}
		assertSourcePreserved(t, path)
		assertNotExist(t, dst)
		assertNotExist(t, tmp)
	})

	t.Run("DestinationIdentity", func(t *testing.T) {
		t.Parallel()
		path, dst, tmp := newSource(t)
		ops := newOps()
		destinationStats := 0
		const replacement = "replacement log"
		var promoted *os.File
		ops.stat = func(name string) (os.FileInfo, error) {
			if name == dst {
				destinationStats++
				if destinationStats == 1 {
					var err error
					promoted, err = os.Open(dst) //nolint:gosec // path is test-controlled.
					if err != nil {
						return nil, err
					}
					if err := os.Remove(dst); err != nil {
						return nil, err
					}
					if err := os.WriteFile(dst, []byte(replacement), 0o600); err != nil {
						return nil, err
					}
				}
			}
			return os.Stat(name)
		}

		if _, _, err := compressLogFileWithSnapshotAndOps(path, nil, ops); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("compression error = %v, want destination identity failure", err)
		}
		if err := promoted.Close(); err != nil {
			t.Fatal(err)
		}
		assertSourcePreserved(t, path)
		got, err := os.ReadFile(dst) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != replacement {
			t.Fatalf("replacement destination = %q, want %q", got, replacement)
		}
		assertNotExist(t, tmp)
	})

	t.Run("SourceRemoval", func(t *testing.T) {
		t.Parallel()
		path, dst, tmp := newSource(t)
		ops := newOps()
		ops.remove = func(name string) error {
			if name == path {
				return errors.New("forced source removal failure")
			}
			return os.Remove(name)
		}

		if _, _, err := compressLogFileWithSnapshotAndOps(path, nil, ops); err == nil || !strings.Contains(err.Error(), "forced source removal failure") {
			t.Fatalf("compression error = %v, want source removal failure", err)
		}
		assertSourcePreserved(t, path)
		assertNotExist(t, dst)
		assertNotExist(t, tmp)
	})
}

func TestTsToTime(t *testing.T) {
	t.Parallel()
	// 1735689600.5 = 2025-01-01T00:00:00.5Z (exact in float64).
	ts := 1735689600.5
	got := tsToTime(ts)
	if got.Year() != 2025 || got.Month() != time.January || got.Day() != 1 {
		t.Errorf("tsToTime(%v) = %v", ts, got)
	}
	if got.Nanosecond() != 500000000 {
		t.Errorf("tsToTime(%v).Nanosecond() = %d, want 500000000", ts, got.Nanosecond())
	}
	if got.Location() != time.UTC {
		t.Error("tsToTime should return UTC")
	}
}

func TestLoadedTask(t *testing.T) {
	t.Parallel()
	t.Run("replay_proofs_and_scans_require_snapshot_append_authority", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task.jsonl")
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "authority", Harness: harness.Claude})
		first := claudeAssistant(t, map[string]any{"type": "text", "text": "first"})
		second := claudeAssistant(t, map[string]any{"type": "text", "text": "second"})
		writeLogFile(t, dir, filepath.Base(path), meta, first)
		tasks, err := LoadLogs(dir)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("LoadLogs = %d tasks, %v", len(tasks), err)
		}
		lt := tasks[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})
		initial := lt.ValidatedSnapshot()
		if initial == nil {
			t.Fatal("inventory load did not retain a validation snapshot")
		}
		if _, err := lt.CacheProofForLog(path); err != nil {
			t.Fatalf("initial replay proof: %v", err)
		}

		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(second + "\n")
		if closeErr := file.Close(); writeErr != nil || closeErr != nil {
			t.Fatalf("append raw log = %v, %v", writeErr, closeErr)
		}
		proof, err := lt.CacheProofForLog(path)
		if err != nil || proof.Size <= initial.Size {
			t.Fatalf("append replay proof = %+v, %v; want larger valid proof", proof, err)
		}
		if _, err := lt.ScanMessagesWithContext(t.Context(), func(agent.ParsedMessage) error { return nil }); err != nil {
			t.Fatalf("append replay scan: %v", err)
		}
		retained := lt.ValidatedSnapshot()
		if retained == nil || retained.Size != proof.Size {
			t.Fatalf("retained append snapshot = %#v, want size %d", retained, proof.Size)
		}

		forgedFirst := claudeAssistant(t, map[string]any{"type": "text", "text": "bogus"})
		third := claudeAssistant(t, map[string]any{"type": "text", "text": "third"})
		writeLogFile(t, dir, filepath.Base(path), meta, forgedFirst, second, third)
		if _, err := lt.ScanMessagesWithContext(t.Context(), func(agent.ParsedMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "immutable prefix changed") {
			t.Fatalf("replay scan error = %v, want immutable-prefix rejection", err)
		}
		if got := lt.ValidatedSnapshot(); got != retained {
			t.Fatal("immutable-prefix rejection replaced retained snapshot")
		}
		writeLogFile(t, dir, filepath.Base(path), meta, first, second)

		contents, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil { //nolint:gosec // path is test-controlled.
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
		if _, err := lt.CacheProofForLog(path); !errors.Is(err, ErrRetainedSnapshotMismatch) {
			t.Fatalf("same-size mutation proof error = %v, want retained-snapshot mismatch", err)
		}
		if _, err := lt.ScanMessagesWithContext(t.Context(), func(agent.ParsedMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "outside append growth") {
			t.Fatalf("replay scan error = %v, want append-growth rejection", err)
		}
		if got := lt.ValidatedSnapshot(); got != retained {
			t.Fatal("rejected replay scan replaced retained snapshot")
		}

		changedHeader := strings.Replace(meta, "{", "{ ", 1)
		writeLogFile(t, dir, filepath.Base(path), changedHeader, first, second)
		if _, err := lt.CacheProofForLog(path); !errors.Is(err, ErrRetainedSnapshotMismatch) {
			t.Fatalf("changed-header proof error = %v, want retained-snapshot mismatch", err)
		}
		if _, err := lt.ScanMessagesWithContext(t.Context(), func(agent.ParsedMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "immutable header changed") {
			t.Fatalf("replay scan error = %v, want immutable-header rejection", err)
		}
	})
	t.Run("StreamMessages", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		pr := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "o", ForgeRepo: "r", ForgePR: 5})
		a2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "waiting"})
		writeLogFile(t, dir, "t.jsonl", meta, a1, pr, a2, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages() {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m.Message)
		}
		if len(streamed) == 0 {
			t.Fatal("no messages streamed")
		}
		// Streaming must yield exactly the conversation messages a full load
		// produces — control records (caic_meta/pr/result) filtered out.
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if len(streamed) != len(lt.Msgs) {
			t.Fatalf("streamed %d messages, full load %d", len(streamed), len(lt.Msgs))
		}
	})

	t.Run("StreamMessagesIncludesProvisioningLogs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		setupLog := mustJSON(t, map[string]string{"type": "caic_log", "line": "creating runtime"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "failed"})
		writeLogFile(t, dir, "t.jsonl", meta, setupLog, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)

		var streamed []agent.Message
		for m, e := range tasks[0].StreamMessages() {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m.Message)
		}
		if len(streamed) != 1 {
			t.Fatalf("streamed %d messages, want 1", len(streamed))
		}
		log, ok := streamed[0].(*agent.LogMessage)
		if !ok || log.Line != "creating runtime" {
			t.Fatalf("message = %#v, want provisioning log", streamed[0])
		}
		if err := tasks[0].LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if len(tasks[0].Msgs) != 1 {
			t.Fatalf("loaded %d messages, want 1", len(tasks[0].Msgs))
		}
		loaded, ok := tasks[0].Msgs[0].(*agent.LogMessage)
		if !ok || loaded.Line != "creating runtime" {
			t.Fatalf("loaded message = %#v, want provisioning log", tasks[0].Msgs[0])
		}
	})

	t.Run("ScanMessagesIncludesReplayControls", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "replay controls", Harness: harness.Claude})
		writeLogFile(t, dir, "t.jsonl", meta,
			`{"t":"exit","exit_code":2,"error":"failed"}`,
			`{"t":"log","line":"provisioning"}`,
			`{"t":"context_cleared"}`,
		)
		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		var replayed []string
		if _, err := lt.ScanMessagesWithContext(t.Context(), func(parsed agent.ParsedMessage) error {
			replayed = append(replayed, parsed.Message.Type())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"caic_exit", "log", "system"}; !slices.Equal(replayed, want) {
			t.Fatalf("replay controls = %q, want %q", replayed, want)
		}
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		loaded := make([]string, 0, len(lt.Msgs))
		for _, message := range lt.Msgs {
			loaded = append(loaded, message.Type())
		}
		if !slices.Equal(replayed, loaded) {
			t.Fatalf("replay controls = %q, live semantic controls = %q", replayed, loaded)
		}
	})

	t.Run("StreamMessagesCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		a2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "waiting"})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(meta, a1, a2, trailer))

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages() {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m.Message)
		}
		if len(streamed) != 2 {
			t.Fatalf("streamed %d messages, want 2", len(streamed))
		}
	})

	t.Run("StreamMessagesEnforcesAuthority", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "stream task", Harness: harness.Claude,
		})
		mismatch := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "stream task", Harness: harness.Codex,
		})
		message := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		for _, compressed := range []bool{false, true} {
			format := "plain"
			if compressed {
				format = "zstd"
			}
			t.Run(format+" missing header", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, message)
				lt := &LoadedTask{path: path, Harness: harness.Claude}
				lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return claudecode.New().NewWire().ParseMessage, nil
				})
				var gotErr error
				for _, err := range lt.StreamMessages() {
					gotErr = errors.Join(gotErr, err)
				}
				if gotErr == nil || !strings.Contains(gotErr.Error(), "invalid first log header") {
					t.Fatalf("error = %v, want invalid first header", gotErr)
				}
			})
			t.Run(format+" mixed authority", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, meta, message, mismatch)
				lt := &LoadedTask{path: path, Harness: harness.Claude}
				lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return claudecode.New().NewWire().ParseMessage, nil
				})
				var gotErr error
				var messages int
				for msg, err := range lt.StreamMessages() {
					gotErr = errors.Join(gotErr, err)
					if msg.Message != nil {
						messages++
					}
				}
				if messages == 0 {
					t.Fatal("message before mismatched header was not streamed")
				}
				if gotErr == nil || !strings.Contains(gotErr.Error(), "authority changed") {
					t.Fatalf("error = %v, want authority changed", gotErr)
				}
			})
			t.Run(format+" leading empty lines", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, "", "  ", meta, message)
				lt := &LoadedTask{path: path, Harness: harness.Claude}
				lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return claudecode.New().NewWire().ParseMessage, nil
				})
				var messages int
				for msg, err := range lt.StreamMessages() {
					if err != nil {
						t.Fatal(err)
					}
					if msg.Message != nil {
						messages++
					}
				}
				if messages == 0 {
					t.Fatal("no messages streamed")
				}
			})
		}
	})

	t.Run("Primary", func(t *testing.T) {
		t.Parallel()
		t.Run("NoRepos", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{}
			if lt.Primary() != nil {
				t.Error("Primary() should be nil for no-repo task")
			}
		})
		t.Run("WithRepos", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Repos: []RepoMount{{Name: "a/b", Branch: "caic-0"}}}
			if p := lt.Primary(); p == nil || p.Name != "a/b" {
				t.Errorf("Primary() = %+v, want a/b", p)
			}
		})
	})

	t.Run("LoadMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("AlreadyLoaded", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Msgs: []agent.Message{&agent.TextMessage{Text: "cached"}}}
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs mutated when already loaded")
			}
		})
		t.Run("NoPath", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{}
			lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			})
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("NoParser", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{path: "/does/not/exist.jsonl"}
			if err := lt.LoadMessages(); err == nil {
				t.Fatal("expected error when no parser is set")
			}
		})
		t.Run("LoadLogFileNoParser", func(t *testing.T) {
			t.Parallel()
			_, err := loadSemanticLogSnapshot("/does/not/exist.jsonl", nil)
			if err == nil {
				t.Fatal("expected error when resolver is nil")
			}
			if !strings.Contains(err.Error(), "resolver is nil") {
				t.Errorf("error = %q, want resolver nil error", err.Error())
			}
		})
		t.Run("StreamNoParser", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{path: "/does/not/exist.jsonl"}
			var gotErr bool
			for _, e := range lt.StreamMessages() {
				if e != nil {
					gotErr = true
				}
			}
			if !gotErr {
				t.Error("StreamMessages without parser should yield an error")
			}
		})
	})

	t.Run("LoadMessagesTail", func(t *testing.T) {
		t.Parallel()
		t.Run("SmallFileFullLoad", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", CostUSD: 0.42})
			writeLogFile(t, dir, "t.jsonl", meta, a1, trailer)

			tasks, err := LoadLogs(dir)
			if err != nil {
				t.Fatal(err)
			}
			lt := tasks[0]
			setClaudeParser(tasks)
			if err := lt.LoadMessagesTail(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs len = %d, want 1", len(lt.Msgs))
			}
			if lt.Result == nil || lt.Result.CostUSD != 0.42 {
				t.Errorf("Result = %+v", lt.Result)
			}
		})
		t.Run("TailOnlyWhenSizeExceedsThreshold", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})

			name := filepath.Join(dir, "big.jsonl")
			f, err := os.OpenFile(filepath.Clean(name), os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(meta + "\n")
			filler := strings.Repeat("x", 1024)
			for range (maxTailLoadBytes / 1024) + 5 {
				a := claudeAssistant(t, map[string]any{"type": "text", "text": filler})
				_, _ = f.WriteString(a + "\n")
			}
			_, _ = f.WriteString(trailer + "\n")
			_ = f.Close()

			lt := &LoadedTask{
				path:    name,
				Harness: "claude",
				LogSize: maxTailLoadBytes + 1,
			}
			parse := claudecode.New().NewWire().ParseMessage
			calls := 0
			if err := lt.LoadMessagesTailWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return func(line []byte) ([]agent.Message, error) {
					calls++
					return parse(line)
				}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if lt.State != StatePurged {
				t.Errorf("State = %v, want StatePurged", lt.State)
			}
			if lt.Result == nil {
				t.Fatal("Result not restored from tail")
			}
			if want := int(maxTailLoadBytes/1024) + 5; calls != want {
				t.Errorf("parsed %d records, want every one of %d physical native records", calls, want)
			}
		})
		t.Run("CompressedTailKeepsBoundedRecentLines", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			oldMsg := claudeAssistant(t, map[string]any{"type": "text", "text": "old output"})
			recentMsg := claudeAssistant(t, map[string]any{"type": "text", "text": "recent output"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeCompressedLogFile(t, dir, "compressed.jsonl.zst", seqOf(meta, oldMsg, recentMsg, trailer))

			path := filepath.Join(dir, "compressed.jsonl.zst")
			snapshot, err := loadSemanticTailSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			}, int64(len(recentMsg)+len(trailer)+2))
			if err != nil {
				t.Fatal(err)
			}
			lt := semanticLoadedTask(snapshot)
			if lt.Result == nil || lt.State != StatePurged {
				t.Fatalf("Result = %+v, State = %v", lt.Result, lt.State)
			}
			if len(lt.Msgs) != 1 {
				t.Fatalf("Msgs len = %d, want 1", len(lt.Msgs))
			}
			msg, ok := lt.Msgs[0].(*agent.TextMessage)
			if !ok {
				t.Fatalf("Msgs[0] = %T, want *agent.TextMessage", lt.Msgs[0])
			}
			if msg.Text != "recent output" {
				t.Errorf("Text = %q, want recent output", msg.Text)
			}
		})
		t.Run("AlreadyLoaded", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Msgs: []agent.Message{&agent.TextMessage{Text: "cached"}}}
			lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			})
			if err := lt.LoadMessagesTail(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs mutated when already loaded")
			}
		})
	})
}

func TestParseState(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want State
	}{
		{"failed", StateFailed},
		{"crashed", StateCrashed},
		{"stopped", StateStopped},
		{"purged", StatePurged},
		{"terminated", StatePurged}, // backward compat
		{"unknown", StateFailed},
	} {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := parseState(tt.in); got != tt.want {
				t.Errorf("parseState(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestExportDiscussionUsesValidatedPhysicalReader(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "export this",
		Harness:     harness.Claude,
	})
	for _, compressed := range []bool{false, true} {
		format := "plain"
		if compressed {
			format = "zstd"
		}
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, compressed, meta, `{"type":"text","text":"visible"}`)
			markdown, err := ExportDiscussion(path, func(got harness.Name) (func([]byte) ([]agent.Message, error), error) {
				if got != harness.Claude {
					t.Fatalf("resolver harness = %q, want claude", got)
				}
				return func(line []byte) ([]agent.Message, error) {
					var raw struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(line, &raw); err != nil {
						return nil, err
					}
					return []agent.Message{&agent.TextMessage{Text: raw.Text}}, nil
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(markdown, "**Prompt**: export this") || !strings.Contains(markdown, "## Assistant\n\nvisible") {
				t.Fatalf("exported discussion = %q", markdown)
			}
		})
	}
}

func TestSemanticSessionAndTailParseEveryNativeRecord(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "parse all"})
	path := writePhysicalTestLog(t, false, meta,
		`{"type":"assistant","n":1}`, `{"type":"assistant","n":2}`, `{"type":"assistant","n":3}`)
	resolver := func(calls *int) NativeParserResolver {
		return func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
			if h != harness.Claude {
				t.Fatalf("resolver harness = %q, want claude", h)
			}
			return func([]byte) ([]agent.Message, error) {
				*calls++
				return []agent.Message{&agent.InitMessage{SessionID: "native", Version: "v"}}, nil
			}, nil
		}
	}
	sessionCalls := 0
	if _, _, err := loadSemanticSessionMetadata(path, resolver(&sessionCalls)); err != nil {
		t.Fatal(err)
	}
	if sessionCalls != 3 {
		t.Fatalf("session native parser calls = %d, want every 3 physical records", sessionCalls)
	}
	tailCalls := 0
	if _, err := loadSemanticTailSnapshot(path, resolver(&tailCalls), 1<<20); err != nil {
		t.Fatal(err)
	}
	if tailCalls != 3 {
		t.Fatalf("tail native parser calls = %d, want every 3 physical records", tailCalls)
	}

	v2Meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV2), Harness: harness.Claude, Prompt: "strict"})
	v2Path := writePhysicalTestLog(t, false, v2Meta,
		`{"t":"caic_session","session_id":"session","agent_version":"v"}`, `{"t":"unknown"}`)
	if _, _, err := loadSemanticSessionMetadata(v2Path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
		return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
	}); err == nil || !strings.Contains(err.Error(), "unknown top-level t") {
		t.Fatalf("v2 session scan error = %v, want later strict parser rejection", err)
	}
}

func TestV1ProductionReadPassMatrix(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "pass count",
		Harness:     harness.Claude,
	})
	for _, tc := range []struct {
		name            string
		nativeLine      string
		topLevelSession bool
	}{
		{
			name:            "top-level-caic-session",
			nativeLine:      mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "top-level-session", AgentVersion: "top-level-version"}),
			topLevelSession: true,
		},
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
				tk := &Task{ID: ksid.NewID(), Harness: harness.Claude}
				name := taskLogFileName(tk)
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
				lt, err = loadLogHeaderFromReader(path, headerReader, false, false)
				if err != nil {
					closeHeader()
					t.Fatal(err)
				}
				closeHeader()
				nativeCalls := 0
				if tc.topLevelSession {
					if lt.SessionID != "top-level-session" || lt.AgentVersion != "top-level-version" {
						t.Fatalf("header scan session metadata = (%q, %q), want top-level-session/top-level-version", lt.SessionID, lt.AgentVersion)
					}
				} else {
					if lt.SessionID != "" || lt.AgentVersion != "" {
						t.Fatalf("header scan treated native record as task session metadata: (%q, %q)", lt.SessionID, lt.AgentVersion)
					}

					sessionReader, closeSession := openCounted()
					session, _, err := loadSemanticSessionMetadataFromReader(path, sessionReader, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
						if h != harness.Claude {
							t.Fatalf("resolver harness = %q, want %q", h, harness.Claude)
						}
						return func([]byte) ([]agent.Message, error) {
							nativeCalls++
							return []agent.Message{&agent.InitMessage{SessionID: "native-session", Version: "native"}}, nil
						}, nil
					})
					if err != nil {
						closeSession()
						t.Fatal(err)
					}
					closeSession()
					if session.SessionID != "native-session" || nativeCalls != 1 {
						t.Fatalf("session/native calls = (%q, %d), want native-session/1", session.SessionID, nativeCalls)
					}
				}

				messageReader, closeMessages := openCounted()
				messageSnapshot, err := loadSemanticLogSnapshotFromReader(path, messageReader, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
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
				var reopenProofBytes int64
				if !compressed {
					// Adoption passes its completed EOF proof to the task before a
					// later cleanup Reopen. Reopen must use only a bounded current
					// header/identity check rather than adding a fourth full pass.
					// Reopen after proving the same raw header.
					reopenProofBytes = reopenWithProofReplay(t, dir, path, tk, messageSnapshot)
				}
				wantNativeCalls := 2    // one session and one message scan
				wantCompletePasses := 3 // header, native/no-top session, and semantic scan
				if tc.topLevelSession {
					wantNativeCalls = 0
					wantCompletePasses = 2 // header/inventory and semantic scans
				}
				if nativeCalls != wantNativeCalls {
					t.Fatalf("native parser calls = %d, want %d", nativeCalls, wantNativeCalls)
				}
				if completePasses != wantCompletePasses {
					t.Errorf("complete passes = %d, want %d", completePasses, wantCompletePasses)
				}
				if logicalBytes < int64(completePasses)*logicalSize {
					t.Errorf("logical bytes = %d, less than %d completed passes of %d", logicalBytes, completePasses, logicalSize)
				}
				if logicalBytes > 3*logicalSize+min(int64(65536), logicalSize) {
					t.Errorf("logical bytes = %d, exceeds three complete passes plus bounded tail (%d)", logicalBytes, 3*logicalSize+min(int64(65536), logicalSize))
				}
				if !compressed && (reopenProofBytes == 0 || reopenProofBytes > min(int64(64<<10), logicalSize)) {
					t.Errorf("Reopen proof bytes = %d, want bounded header/identity tail in (0, %d]", reopenProofBytes, min(int64(64<<10), logicalSize))
				}
			})
		}
	}
}
