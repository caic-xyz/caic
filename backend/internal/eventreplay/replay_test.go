// Tests for proof-bound replay sidecar storage.

package eventreplay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/logproof"
)

const CacheVersion = 6

var testFormat = Format{
	Version: CacheVersion,
	ValidateLine: func(line []byte) error {
		if !json.Valid(line) {
			return errors.New("invalid test record")
		}
		return nil
	},
}

// localCacheProof is a test-only proof seam. Eventreplay tests exercise cache
// comparison without coupling to task's physical-log scanner; server tests keep
// one integration path through task.CacheProofForLog.
func localCacheProof(path string) (logproof.CacheProof, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return logproof.CacheProof{}, err
	}
	header, _, _ := bytes.Cut(data, []byte("\n"))
	if !json.Valid(header) {
		return logproof.CacheProof{}, errors.New("invalid test log header")
	}
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return logproof.CacheProof{}, err
	}
	return logproof.CacheProof{
		Device:    1,
		Inode:     1,
		Size:      info.Size(),
		ModTimeNs: info.ModTime().UnixNano(),
		RawHeader: string(header),
	}, nil
}

func TestCacheWriter(t *testing.T) {
	t.Parallel()
	t.Run("writes data", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cache, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		cache.WriteData([]byte(`{"kind":"text","ts":1,"text":{"text":"rebuilt"}}`))
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		replay, ok := OpenReplay(logPath, localCacheProof, testFormat)
		if !ok {
			t.Fatal("rebuilt cache was not committed")
		}
		t.Cleanup(replay.Close)
		out := &flushBuffer{}
		idx := 0
		if replay.WriteSSE(out, out, &idx) != SSEComplete || idx != 1 || !bytes.Contains(out.Bytes(), []byte("rebuilt")) {
			t.Fatalf("replay output = %q, frames = %d", out.String(), idx)
		}
	})
	t.Run("appends spool", func(t *testing.T) {
		t.Parallel()
		logPath := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(logPath, []byte(`{"type":"caic_meta"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cache, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		spool, err := cache.NewSpool()
		if err != nil {
			t.Fatal(err)
		}
		discarded := false
		t.Cleanup(func() {
			if !discarded {
				if err := spool.Discard(); err != nil {
					t.Error(err)
				}
			}
		})
		if _, err := spool.Write([]byte(`{"kind":"text","ts":1,"text":{"text":"spooled"}}` + "\n")); err != nil {
			t.Fatal(err)
		}
		if err := cache.AppendSpool(t.Context(), spool); err != nil {
			t.Fatal(err)
		}
		if err := spool.Discard(); err != nil {
			t.Fatal(err)
		}
		discarded = true
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		replay, ok := OpenReplay(logPath, localCacheProof, testFormat)
		if !ok {
			t.Fatal("spooled cache was not committed")
		}
		t.Cleanup(replay.Close)
		out := &flushBuffer{}
		idx := 0
		if replay.WriteSSE(out, out, &idx) != SSEComplete || !bytes.Contains(out.Bytes(), []byte("spooled")) {
			t.Fatalf("spooled replay = %q, index = %d", out.String(), idx)
		}
	})
}

func TestOpenTrustedReplay(t *testing.T) {
	t.Parallel()
	newCache := func(t *testing.T, body ...string) string {
		logPath := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(logPath, []byte(`{"type":"caic_meta"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cache, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range body {
			cache.WriteData([]byte(line))
		}
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		return logPath
	}

	t.Run("skips body validation after cold admission", func(t *testing.T) {
		t.Parallel()
		logPath := newCache(t, `{"kind":"first"}`, `{"kind":"second"}`)
		validations := 0
		format := Format{Version: CacheVersion, ValidateLine: func(line []byte) error {
			validations++
			if !json.Valid(line) {
				return errors.New("invalid test record")
			}
			return nil
		}}
		cold, ok := OpenReplay(logPath, localCacheProof, format)
		if !ok {
			t.Fatal("cold OpenReplay = false")
		}
		identity := cold.CacheIdentity()
		cold.Close()
		if validations != 2 {
			t.Fatalf("cold validations = %d, want 2", validations)
		}
		proof, err := localCacheProof(logPath)
		if err != nil {
			t.Fatal(err)
		}
		validations = 0
		warm, ok := OpenTrustedReplay(logPath, proof, localCacheProof, format, identity)
		if !ok {
			t.Fatal("warm OpenTrustedReplay = false")
		}
		t.Cleanup(warm.Close)
		out := &flushBuffer{}
		idx := 0
		if warm.WriteSSE(out, out, &idx) != SSEComplete || idx != 2 {
			t.Fatalf("warm WriteSSE = (%q, %d), want two records", out.String(), idx)
		}
		if validations != 0 {
			t.Fatalf("warm validations = %d, want 0", validations)
		}
	})

	t.Run("replacement falls back to cold validation", func(t *testing.T) {
		t.Parallel()
		logPath := newCache(t, `{"kind":"original"}`)
		cold, ok := OpenReplay(logPath, localCacheProof, testFormat)
		if !ok {
			t.Fatal("initial OpenReplay = false")
		}
		identity := cold.CacheIdentity()
		cold.Close()
		cache, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		cache.WriteData([]byte(`{"kind":"replacement"}`))
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		proof, err := localCacheProof(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenTrustedReplay(logPath, proof, localCacheProof, testFormat, identity); ok {
			replay.Close()
			t.Fatal("trusted open accepted replaced cache")
		}
		replay, ok := OpenReplayWithProof(logPath, proof, localCacheProof, testFormat)
		if !ok {
			t.Fatal("cold open rejected replacement")
		}
		t.Cleanup(replay.Close)
	})

	t.Run("rechecks raw proof before streaming", func(t *testing.T) {
		t.Parallel()
		logPath := newCache(t, `{"kind":"record"}`)
		cold, ok := OpenReplay(logPath, localCacheProof, testFormat)
		if !ok {
			t.Fatal("initial OpenReplay = false")
		}
		identity := cold.CacheIdentity()
		cold.Close()
		proof, err := localCacheProof(logPath)
		if err != nil {
			t.Fatal(err)
		}
		changedProof := proof
		changedProof.Size++
		if replay, ok := OpenTrustedReplay(logPath, proof, func(string) (logproof.CacheProof, error) {
			return changedProof, nil
		}, testFormat, identity); ok {
			replay.Close()
			t.Fatal("trusted open accepted changed raw proof")
		}
	})
}

func TestReadCacheHeaderStrictSchema(t *testing.T) {
	t.Parallel()
	proof := logproof.CacheProof{
		Device:    1,
		Inode:     2,
		Size:      3,
		ModTimeNs: 4,
		RawHeader: `{"type":"caic_meta"}`,
	}
	valid, err := json.Marshal(CacheHeader{
		Version: CacheVersion,
		Proof:   proof,
	})
	if err != nil {
		t.Fatal(err)
	}
	versionField := fmt.Sprintf(`"v":%d`, CacheVersion)
	for _, tc := range []struct {
		name  string
		line  string
		valid bool
	}{
		{name: "valid", line: string(valid), valid: true},
		{name: "duplicate key", line: strings.Replace(string(valid), versionField, versionField+","+versionField, 1)},
		{name: "unknown key", line: strings.TrimSuffix(string(valid), "}") + `,"unknown":true}`},
		{name: "bad scalar type", line: strings.Replace(string(valid), versionField, `"v":"5"`, 1)},
		{name: "null required scalar", line: strings.Replace(string(valid), versionField, `"v":null`, 1)},
		{name: "trailing data", line: string(valid) + ` {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok := readCacheHeader(newReplayRecordReader(strings.NewReader(tc.line+"\n")), proof, testFormat)
			if ok != tc.valid {
				t.Fatalf("readCacheHeader(%s) valid = %t, want %t", tc.line, ok, tc.valid)
			}
		})
	}
}

func TestReplayRecordReaderRejectsOversizedRecord(t *testing.T) {
	t.Parallel()
	line, err := newReplayRecordReader(strings.NewReader(strings.Repeat("x", maxReplayRecordBytes+1))).ReadRecord()
	if !errors.Is(err, errReplayRecordTooLarge) || line != nil {
		t.Fatalf("ReadRecord oversized = %q, %v; want nil, size-limit error", line, err)
	}
}

type flushBuffer struct {
	bytes.Buffer

	flushes int
}

func (b *flushBuffer) Flush() {
	b.flushes++
}

type failingReplayWriter struct {
	bytes.Buffer

	limit int
}

func (w *failingReplayWriter) Flush() {}

func (w *failingReplayWriter) Write(data []byte) (int, error) {
	remaining := w.limit - w.Len()
	if remaining <= 0 {
		return 0, errors.New("SSE connection closed")
	}
	if len(data) > remaining {
		_, _ = w.Buffer.Write(data[:remaining])
		return remaining, errors.New("SSE connection closed")
	}
	return w.Buffer.Write(data)
}

func TestReplayWriteSSEReportsPartialPublication(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "task.jsonl")
	if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
	if err != nil {
		t.Fatal(err)
	}
	cache.WriteData([]byte(`{"kind":"text","ts":1,"text":{"text":"first"}}`))
	cache.WriteData([]byte(`{"kind":"text","ts":2,"text":{"text":"second"}}`))
	if err := cache.CommitContext(t.Context(), logPath); err != nil {
		t.Fatal(err)
	}
	replay, ok := OpenReplay(logPath, localCacheProof, testFormat)
	if !ok {
		t.Fatal("OpenReplay = false")
	}
	t.Cleanup(replay.Close)
	out := &failingReplayWriter{limit: 1}
	idx := 0
	if result := replay.WriteSSE(out, out, &idx); result != SSEPartial || out.Len() != 1 {
		t.Fatalf("WriteSSE = (%v, %d bytes), want partial publication", result, out.Len())
	}
}

func TestPruneStaleCaches(t *testing.T) {
	t.Parallel()

	t.Run("valid_kept", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteData([]byte(`{"kind":"text","ts":1,"text":{"text":"ok"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("removed = %d, want 0", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); err != nil {
			t.Fatalf("valid cache removed: %v", err)
		}
	})

	t.Run("orphan_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cachePath := filepath.Join(dir, "orphan.events.zst")
		if err := os.WriteFile(cachePath, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
			t.Fatalf("cache still exists: %v", err)
		}
	})

	t.Run("stale_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteData([]byte(`{"kind":"text","ts":1,"text":{"text":"old"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n{\"type\":\"ignored\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(logPath, future, future); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); !os.IsNotExist(err) {
			t.Fatalf("stale cache still exists: %v", err)
		}
	})

	t.Run("invalid_event_body_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteData([]byte(`invalid`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d, want invalid event sidecar removed", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); !os.IsNotExist(err) {
			t.Fatalf("invalid cache still exists: %v", err)
		}
	})

	t.Run("unverifiable_raw_authority_is_retained", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath, filepath.Join(filepath.Dir(logPath), ".replay-tmp"), localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteData([]byte(`{"kind":"text","ts":1,"text":{"text":"cached"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("not a log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("removed = %d, want proof failure retained", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); err != nil {
			t.Fatalf("unverifiable cache was pruned: %v", err)
		}
	})

	t.Run("temp_files_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		paths := []string{
			filepath.Join(dir, "task.events.zst.123.body"),
			filepath.Join(dir, "task.events.zst.456.tmp"),
			filepath.Join(dir, "task.events.zst.789.pending"),
			filepath.Join(dir, "unrelated.tmp"),
		}
		for _, path := range paths {
			if err := os.WriteFile(path, []byte("temp"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		removed, err := PruneStaleCaches(dir, localCacheProof, testFormat)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 3 {
			t.Fatalf("removed = %d, want 3", removed)
		}
		for _, path := range paths[:3] {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("temp file still exists: %s: %v", path, err)
			}
		}
		if _, err := os.Stat(paths[3]); err != nil {
			t.Fatalf("unrelated temp file removed: %v", err)
		}
	})
}
