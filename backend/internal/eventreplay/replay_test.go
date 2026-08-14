// Tests for compact EventMessage replay sidecars.

package eventreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/logproof"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// localCacheProof is a test-only proof seam. Eventreplay tests exercise cache
// comparison without coupling to task's physical-log scanner; server tests keep
// one integration path through task.CacheProofForLog.
func localCacheProof(path string) (logproof.CacheProof, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return logproof.CacheProof{}, err
	}
	header, _, _ := bytes.Cut(data, []byte("\n"))
	var authority struct {
		Version int          `json:"version"`
		Harness harness.Name `json:"harness"`
	}
	if err := json.Unmarshal(header, &authority); err != nil {
		return logproof.CacheProof{}, err
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
		Version:   agent.LogVersion(authority.Version),
		Harness:   authority.Harness,
		RawHeader: string(header),
	}, nil
}

func TestCacheWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "task.jsonl")
	if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := NewCacheWriter(logPath, localCacheProof)
	if err != nil {
		t.Fatal(err)
	}
	cache.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"rebuilt"}}`))
	if err := cache.CommitContext(t.Context(), logPath); err != nil {
		t.Fatal(err)
	}
	replay, ok := OpenReplay(logPath, localCacheProof)
	if !ok {
		t.Fatal("rebuilt cache was not committed")
	}
	defer replay.Close()
	out := &flushBuffer{}
	idx := 0
	if replay.WriteSSE(out, out, &idx) != SSEComplete || idx != 1 || !bytes.Contains(out.Bytes(), []byte("rebuilt")) {
		t.Fatalf("replay output = %q, frames = %d", out.String(), idx)
	}
}

func TestRegenerateReplay(t *testing.T) {
	t.Parallel()

	writeRawLog := func(t *testing.T, path string) {
		if err := os.WriteFile(path, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertNoReplayArtifacts := func(t *testing.T, logPath string) {
		if _, err := os.Stat(CachePath(logPath)); !os.IsNotExist(err) {
			t.Fatalf("published unsafe cache: %v", err)
		}
		matches, err := filepath.Glob(CachePath(logPath) + ".*")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("stale replay artifacts = %v", matches)
		}
	}

	t.Run("discards unsafe derived artifacts/source mutation", func(t *testing.T) {
		t.Parallel()
		logPath := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, logPath)
		proof, err := localCacheProof(logPath)
		if err != nil {
			t.Fatal(err)
		}
		err = RegenerateReplay(t.Context(), logPath, localCacheProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextMessage{Text: "before mutation"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			if err := os.WriteFile(logPath, append([]byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), []byte("{\"type\":\"ignored\"}\n")...), 0o600); err != nil {
				return logproof.CacheProof{}, err
			}
			return proof, nil
		})
		if err == nil {
			t.Fatal("RegenerateReplay succeeded after source mutation")
		}
		assertNoReplayArtifacts(t, logPath)
	})

	t.Run("discards unsafe derived artifacts/cancellation removes pending spool", func(t *testing.T) {
		t.Parallel()
		logPath := filepath.Join(t.TempDir(), "task.jsonl")
		writeRawLog(t, logPath)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		err := RegenerateReplay(ctx, logPath, localCacheProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextDeltaMessage{Text: strings.Repeat("x", maxPendingReplayBytes+1)}}); err != nil {
				return logproof.CacheProof{}, err
			}
			cancel()
			return localCacheProof(logPath)
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RegenerateReplay error = %v, want context cancellation", err)
		}
		assertNoReplayArtifacts(t, logPath)
	})
}

func TestValidateReplayEventLine(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(v1.EventMessage{Kind: v1.EventKindText, Ts: 1, Text: &v1.EventText{Text: "valid"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(valid); got != `{"kind":"text","ts":1,"text":{"text":"valid"}}` {
		t.Fatalf("valid cache event encoding = %q", got)
	}
	for _, tc := range []struct {
		name  string
		line  string
		valid bool
	}{
		{name: "valid", line: string(valid), valid: true},
		{name: "null", line: `null`},
		{name: "array", line: `[]`},
		{name: "unknown kind", line: `{"kind":"future","ts":1,"text":{"text":"no"}}`},
		{name: "missing payload", line: `{"kind":"text","ts":1}`},
		{name: "mismatched payload", line: `{"kind":"text","ts":1,"textDelta":{"text":"no"}}`},
		{name: "multiple payloads", line: `{"kind":"text","ts":1,"text":{"text":"yes"},"textDelta":{"text":"no"}}`},
		{name: "missing required top-level field", line: `{"kind":"text","text":{"text":"yes"}}`},
		{name: "missing required nested field", line: `{"kind":"text","ts":1,"text":{}}`},
		{name: "null required top-level string", line: `{"kind":null,"ts":1,"text":{"text":"yes"}}`},
		{name: "null required top-level number", line: `{"kind":"text","ts":null,"text":{"text":"yes"}}`},
		{name: "null required nested text", line: `{"kind":"text","ts":1,"text":{"text":null}}`},
		{name: "null required nested tool result string", line: `{"kind":"toolResult","ts":1,"toolResult":{"toolUseID":null,"duration":1}}`},
		{name: "null required nested tool result number", line: `{"kind":"toolResult","ts":1,"toolResult":{"toolUseID":"id","duration":null}}`},
		{name: "unknown top-level field", line: `{"kind":"text","ts":1,"text":{"text":"yes"},"extra":true}`},
		{name: "unknown nested field", line: `{"kind":"text","ts":1,"text":{"text":"yes","extra":true}}`},
		{name: "duplicate top-level key", line: `{"kind":"text","kind":"text","ts":1,"text":{"text":"yes"}}`},
		{name: "duplicate nested key", line: `{"kind":"text","ts":1,"text":{"text":"yes","text":"forged"}}`},
		{name: "duplicate arbitrary input key", line: `{"kind":"toolUse","ts":1,"toolUse":{"toolUseID":"id","name":"tool","input":{"arg":1,"arg":2}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateReplayEventLine([]byte(tc.line))
			if (err == nil) != tc.valid {
				t.Fatalf("validateReplayEventLine(%s) error = %v, valid = %t", tc.line, err, tc.valid)
			}
		})
	}
}

func TestReadCacheHeaderStrictSchema(t *testing.T) {
	t.Parallel()
	proof := logproof.CacheProof{
		Device:    1,
		Inode:     2,
		Size:      3,
		ModTimeNs: 4,
		Version:   agent.LogVersionV1,
		Harness:   harness.Claude,
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
			_, ok := readCacheHeader(newReplayRecordReader(strings.NewReader(tc.line+"\n")), proof)
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

func TestReplayDiskFilterCompactionEquivalence(t *testing.T) {
	t.Parallel()

	type eventSignature struct {
		kind  v1.EventKind
		tool  string
		delta string
	}
	replay := func(t *testing.T, messages []agent.Message) []eventSignature {
		t.Helper()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cache, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		filter := newReplayDiskFilter(cache, harness.Claude, time.Now())
		for _, message := range messages {
			if err := filter.pushContext(t.Context(), agent.ParsedMessage{Message: message}); err != nil {
				t.Fatal(err)
			}
		}
		if err := filter.flushContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		filter.close()
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		opened, ok := OpenReplay(logPath, localCacheProof)
		if !ok {
			t.Fatal("compacted cache was not publishable")
		}
		defer opened.Close()
		out := &flushBuffer{}
		idx := 0
		if opened.WriteSSE(out, out, &idx) != SSEComplete {
			t.Fatal("compacted cache did not write")
		}
		var got []eventSignature
		for line := range bytes.SplitSeq(out.Bytes(), []byte("\n")) {
			data, ok := bytes.CutPrefix(line, []byte("data: "))
			if !ok {
				continue
			}
			var event v1.EventMessage
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			signature := eventSignature{kind: event.Kind}
			if event.ToolOutputDelta != nil {
				signature.tool = event.ToolOutputDelta.ToolUseID
				signature.delta = event.ToolOutputDelta.Delta
			}
			if event.ToolResult != nil {
				signature.tool = event.ToolResult.ToolUseID
			}
			got = append(got, signature)
		}
		return got
	}

	pending := &agent.PendingUserActionMessage{MessageType: agent.PendingUserActionMessageType}
	for _, tc := range []struct {
		name string
		in   []agent.Message
		want []eventSignature
	}{
		{
			name: "interleaved tool IDs retain only unmatched deltas",
			in: []agent.Message{
				&agent.ToolOutputDeltaMessage{ToolUseID: "a", Delta: "a1"},
				&agent.ToolOutputDeltaMessage{ToolUseID: "b", Delta: "b1"},
				&agent.ToolResultMessage{ToolUseID: "b"},
				&agent.ToolOutputDeltaMessage{ToolUseID: "a", Delta: "a2"},
				&agent.ToolResultMessage{ToolUseID: "a"},
			},
			want: []eventSignature{
				{kind: v1.EventKindToolOutputDelta, tool: "a", delta: "a1"},
				{kind: v1.EventKindToolResult, tool: "b"},
				{kind: v1.EventKindToolResult, tool: "a"},
			},
		},
		{
			name: "pending user action is a compaction boundary",
			in: []agent.Message{
				&agent.ToolOutputDeltaMessage{ToolUseID: "a", Delta: "before"},
				pending,
				&agent.ToolResultMessage{ToolUseID: "a"},
			},
			want: []eventSignature{
				{kind: v1.EventKindToolOutputDelta, tool: "a", delta: "before"},
				{kind: v1.EventKindToolResult, tool: "a"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := replay(t, tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("compacted events = %#v, want %#v", got, tc.want)
			}
		})
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
	cache, err := NewCacheWriter(logPath, localCacheProof)
	if err != nil {
		t.Fatal(err)
	}
	cache.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"first"}}`))
	cache.WriteEventData([]byte(`{"kind":"text","ts":2,"text":{"text":"second"}}`))
	if err := cache.CommitContext(t.Context(), logPath); err != nil {
		t.Fatal(err)
	}
	replay, ok := OpenReplay(logPath, localCacheProof)
	if !ok {
		t.Fatal("OpenReplay = false")
	}
	defer replay.Close()
	out := &failingReplayWriter{limit: 1}
	idx := 0
	if result := replay.WriteSSE(out, out, &idx); result != SSEPartial || out.Len() != 1 {
		t.Fatalf("WriteSSE = (%v, %d bytes), want partial publication", result, out.Len())
	}
}

func TestReplayCacheAuthorityAndPublication(t *testing.T) {
	t.Parallel()

	writeRawLog := func(t *testing.T, path, prompt string) {
		t.Helper()
		data := []byte(`{"type":"caic_meta","version":1,"harness":"claude","prompt":"` + prompt + `"}` + "\n")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCache := func(t *testing.T, path, text string) {
		t.Helper()
		w, err := NewCacheWriter(path, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"` + text + `"}}`))
		if err := w.CommitContext(t.Context(), path); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("raw_header_is_authoritative_even_when_file_identity_is_preserved", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "one")
		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatal(err)
		}
		writeCache(t, logPath, "cached")
		writeRawLog(t, logPath, "two") // Same byte length and open-file identity.
		if err := os.Chtimes(logPath, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); ok {
			replay.Close()
			t.Fatal("cache accepted after raw header authority changed")
		}
	})

	t.Run("truncated_sidecar_publishes_no_sse_prefix", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		proof, err := localCacheProof(logPath)
		if err != nil {
			t.Fatal(err)
		}
		header, err := json.Marshal(CacheHeader{Version: CacheVersion, Proof: proof})
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(CachePath(logPath))
		if err != nil {
			t.Fatal(err)
		}
		enc, err := zstd.NewWriter(out)
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := enc.Write(append(header, '\n')); err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Write([]byte(`{"kind":"text","ts":1,"text":{"text":"prefix"}}`)); err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); ok {
			replay.Close()
			t.Fatal("truncated replay opened for publication")
		}
	})

	t.Run("malformed_event_body_misses_then_rebuilds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		writeCache(t, logPath, "cached")
		encoded, err := os.ReadFile(CachePath(logPath))
		if err != nil {
			t.Fatal(err)
		}
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := decoder.DecodeAll(encoded, nil)
		decoder.Close()
		if err != nil {
			t.Fatal(err)
		}
		header, _, ok := bytes.Cut(body, []byte{'\n'})
		if !ok {
			t.Fatal("cache has no header terminator")
		}
		out, err := os.Create(CachePath(logPath))
		if err != nil {
			t.Fatal(err)
		}
		encoder, err := zstd.NewWriter(out)
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := encoder.Write(append(header, '\n')); err != nil {
			t.Fatal(err)
		}
		if _, err := encoder.Write([]byte("null\n")); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(encoder.Close(), out.Close()); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); ok {
			replay.Close()
			t.Fatal("malformed replay EventMessage opened for publication")
		}
		if err := RegenerateReplay(t.Context(), logPath, localCacheProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextMessage{Text: "rebuilt"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			return localCacheProof(logPath)
		}); err != nil {
			t.Fatal(err)
		}
		replay, ok := OpenReplay(logPath, localCacheProof)
		if !ok {
			t.Fatal("replay was not rebuilt after malformed EventMessage")
		}
		t.Cleanup(replay.Close)
		sseOut := &flushBuffer{}
		idx := 0
		if replay.WriteSSE(sseOut, sseOut, &idx) != SSEComplete || !bytes.Contains(sseOut.Bytes(), []byte("rebuilt")) {
			t.Fatalf("rebuilt replay = %q, index = %d", sseOut.String(), idx)
		}
	})

	t.Run("eventful_header_only_truncation_misses_then_rebuilds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		writeCache(t, logPath, "cached")

		encoded, err := os.ReadFile(CachePath(logPath))
		if err != nil {
			t.Fatal(err)
		}
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := decoder.DecodeAll(encoded, nil)
		decoder.Close()
		if err != nil {
			t.Fatal(err)
		}
		header, _, ok := bytes.Cut(body, []byte{'\n'})
		if !ok {
			t.Fatal("eventful cache has no header terminator")
		}
		var cacheHeader CacheHeader
		if err := json.Unmarshal(header, &cacheHeader); err != nil {
			t.Fatal(err)
		}
		if cacheHeader.Empty {
			t.Fatal("eventful cache header incorrectly declares an empty body")
		}
		out, err := os.Create(CachePath(logPath))
		if err != nil {
			t.Fatal(err)
		}
		encoder, err := zstd.NewWriter(out)
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := encoder.Write(append(header, '\n')); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(encoder.Close(), out.Close()); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); ok {
			replay.Close()
			t.Fatal("eventful cache truncated to its header was accepted")
		}

		if err := RegenerateReplay(t.Context(), logPath, localCacheProof, func(_ context.Context, yield func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			if err := yield(agent.ParsedMessage{Message: &agent.TextMessage{Text: "rebuilt"}}); err != nil {
				return logproof.CacheProof{}, err
			}
			return localCacheProof(logPath)
		}); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); !ok {
			t.Fatal("eventful replay was not rebuilt after header-only truncation")
		} else {
			replay.Close()
		}
	})

	t.Run("complete_empty_regenerated_replay_is_publishable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		if err := RegenerateReplay(t.Context(), logPath, localCacheProof, func(context.Context, func(agent.ParsedMessage) error) (logproof.CacheProof, error) {
			return localCacheProof(logPath)
		}); err != nil {
			t.Fatal(err)
		}
		replay, ok := OpenReplay(logPath, localCacheProof)
		if !ok {
			t.Fatal("complete empty regenerated replay was not publishable")
		}
		defer replay.Close()
		out := &flushBuffer{}
		idx := 0
		if replay.WriteSSE(out, out, &idx) != SSEComplete || idx != 0 || out.Len() != 0 {
			t.Fatalf("complete empty regenerated replay = %q, index = %d", out.String(), idx)
		}
	})

	t.Run("cache_is_not_published_before_commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"partial"}}`))
		if _, err := os.Stat(CachePath(logPath)); !os.IsNotExist(err) {
			t.Fatalf("uncommitted cache publication = %v, want no sidecar", err)
		}
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		if replay, ok := OpenReplay(logPath, localCacheProof); !ok {
			t.Fatal("committed cache was not published")
		} else {
			replay.Close()
		}
	})

	t.Run("large_cached_replay_flushes_in_bounded_chunks", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		writeRawLog(t, logPath, "test")
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"` + string(bytes.Repeat([]byte("x"), 70<<10)) + `"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		replay, ok := OpenReplay(logPath, localCacheProof)
		if !ok {
			t.Fatal("OpenReplay = false")
		}
		defer replay.Close()
		out := &flushBuffer{}
		idx := 0
		if replay.WriteSSE(out, out, &idx) != SSEComplete {
			t.Fatal("WriteSSE = false")
		}
		if idx != 1 || out.flushes < 2 {
			t.Fatalf("replay frames/flushes = %d/%d, want 1/at least 2", idx, out.flushes)
		}
	})
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
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"ok"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir, localCacheProof)
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

		removed, err := PruneStaleCaches(dir, localCacheProof)
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
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"old"}}`))
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

		removed, err := PruneStaleCaches(dir, localCacheProof)
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
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`null`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		removed, err := PruneStaleCaches(dir, localCacheProof)
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
		w, err := NewCacheWriter(logPath, localCacheProof)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"cached"}}`))
		if err := w.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("not a log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := PruneStaleCaches(dir, localCacheProof)
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

		removed, err := PruneStaleCaches(dir, localCacheProof)
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
