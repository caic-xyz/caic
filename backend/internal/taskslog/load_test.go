// Tests for task loading and configuration resolution.

package taskslog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

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

func TestScanPhysicalLogHeaderOnlyAndEOFValidation(t *testing.T) {
	t.Parallel()
	path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "header only",
		Harness:     harness.Claude,
	}))

	t.Run("HeaderOnly", func(t *testing.T) {
		t.Parallel()
		var got agent.MetaMessage
		err := scanPhysicalLog(path, false, func(_ os.FileInfo, _ *physicalLogScanner, meta agent.MetaMessage) error {
			got = meta
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Prompt != "header only" {
			t.Errorf("header prompt = %q, want %q", got.Prompt, "header only")
		}
	})

	t.Run("RequiresEOF", func(t *testing.T) {
		t.Parallel()
		err := scanPhysicalLog(path, true, func(_ os.FileInfo, _ *physicalLogScanner, _ agent.MetaMessage) error {
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "did not reach EOF") {
			t.Fatalf("scanPhysicalLog error = %v, want EOF validation error", err)
		}
	})
}

// readLogAuthority scans a physical log and returns its authority. Used
// only by tests: production loads determine authority via the scanner.
func readLogAuthority(path string) (authority logAuthority, retErr error) {
	retErr = scanPhysicalLog(path, false, func(_ os.FileInfo, scanner *physicalLogScanner, _ agent.MetaMessage) error {
		for scanner.Scan() {
		}
		authority = scanner.authority
		return scanner.Err()
	})
	return authority, retErr
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
	t.Run("V2RejectsDuplicateNonAuthorityHeaderKey", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a.jsonl")
		writeLogFile(t, dir, "a.jsonl", `{"t":"caic_meta","version":2,"prompt":"first","prompt":"last","repos":[],"harness":"codex"}`)
		if _, err := readLogAuthority(path); !errors.Is(err, errDuplicateRawKey) || !strings.Contains(err.Error(), `"prompt"`) {
			t.Fatalf("readLogAuthority error = %v, want duplicate prompt error", err)
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
				if _, err := readLogAuthority(path); err == nil || !strings.Contains(err.Error(), `json: unknown field "bogus"`) {
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

func TestNeedsV1InventoryParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{typ: "assistant"},
		{typ: "result", want: true},
		{typ: "system", want: true},
		{typ: "caic_result", want: true},
		{typ: agent.PendingUserActionMessageType, want: true},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()
			if got := needsV1InventoryParse(tc.typ); got != tc.want {
				t.Errorf("needsV1InventoryParse(%q) = %t, want %t", tc.typ, got, tc.want)
			}
		})
	}
}

func TestDecodeDiscriminatorProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		line     string
		version  agent.LogVersion
		wantTyp  string
		wantCand bool
		wantErr  bool
	}{
		{
			name:    "V1 canonical non-meta",
			line:    `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
			version: agent.LogVersionV1,
			wantTyp: "assistant",
		},
		{
			name:     "V1 canonical meta",
			line:     `{"type":"caic_meta","version":1,"harness":"claude"}`,
			version:  agent.LogVersionV1,
			wantTyp:  "caic_meta",
			wantCand: true,
		},
		{
			name:    "V2 canonical non-meta",
			line:    `{"t":"system","ts":1.5,"model":"claude"}`,
			version: agent.LogVersionV2,
			wantTyp: "system",
		},
		{
			name:     "V2 canonical meta",
			line:     `{"t":"caic_meta","v":2}`,
			version:  agent.LogVersionV2,
			wantTyp:  "caic_meta",
			wantCand: true,
		},
		{
			name:     "V1 reordered keys still probed",
			line:     `{"prompt":"x","type":"caic_meta"}`,
			version:  agent.LogVersionV1,
			wantTyp:  "caic_meta",
			wantCand: true,
		},
		{
			name:     "V1 whitespace before value",
			line:     `{"type": "caic_meta"}`,
			version:  agent.LogVersionV1,
			wantTyp:  "caic_meta",
			wantCand: true,
		},
		{
			name:    "V1 escaped value falls through",
			line:    `{"type":"a\"b","x":1}`,
			version: agent.LogVersionV1,
			wantTyp: "a\"b",
		},
		{
			name:    "V2 line keyed by type falls through",
			line:    `{"type":"system","t":"agent"}`,
			version: agent.LogVersionV2,
			wantTyp: "",
		},
		{
			name:     "V1 meta type with later t",
			line:     `{"type":"caic_meta","t":"system"}`,
			version:  agent.LogVersionV1,
			wantTyp:  "caic_meta",
			wantCand: true,
		},
		{
			name:    "non-object line",
			line:    `42`,
			version: agent.LogVersionV1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typ, cand, err := decodeDiscriminatorProbe([]byte(tc.line), tc.version)
			if (err != nil) != tc.wantErr {
				t.Fatalf("decodeDiscriminatorProbe error = %v, wantErr %t", err, tc.wantErr)
			}
			if typ != tc.wantTyp || cand != tc.wantCand {
				t.Errorf("decodeDiscriminatorProbe = (%q, %t), want (%q, %t)", typ, cand, tc.wantTyp, tc.wantCand)
			}
		})
	}
}

// inventoryJSON renders LoadedTask's JSON projection for testing comparison,
// sidestepping time.Time DeepEqual location sensitivity.
func inventoryJSON(t *testing.T, lt *LoadedTask) []byte {
	data, err := json.Marshal(lt)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadLogHeader(t *testing.T) {
	t.Parallel()
	writeMetaLog := func(t *testing.T, dir, prompt string) string {
		name := "task-a.jsonl"
		path := filepath.Join(dir, name)
		writeLogFile(t, dir, name,
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: prompt, Harness: "claude",
				Repos: []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}}}),
			mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "sess-1", Model: "claude-sonnet-4-6", AgentVersion: "2.1.0"}),
			mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "org", ForgeRepo: "repo", ForgePR: 7}),
			mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "main.go", Added: 4, Deleted: 1}}, Ts: 1767225600.5}),
			claudeAssistant(t, map[string]any{"type": "text", "text": "hello"}),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Title: "done", CostUSD: 1.5,
				Duration: 2.5, NumTurns: 2, InputTokens: 100, OutputTokens: 200,
				DiffStat: agent.DiffStat{{Path: "main.go", Added: 4, Deleted: 1}}}),
		)
		return path
	}

	t.Run("RoundTripServesCache", func(t *testing.T) {
		t.Parallel()
		path := writeMetaLog(t, t.TempDir(), "task1")
		first, err := loadLogHeader(testLogger(), path, true)
		if err != nil {
			t.Fatal(err)
		}
		// Guard against a vacuous round-trip: the fresh scan must have populated
		// the complex fields the header cache is expected to preserve.
		if len(first.Repos) != 1 || first.Repos[0].Name != "org/repo" {
			t.Fatalf("fixture did not populate Repos: %+v", first.Repos)
		}
		if first.SessionID != "sess-1" || first.Model != "claude-sonnet-4-6" || first.AgentVersion != "2.1.0" {
			t.Fatalf("fixture did not populate session metadata: %+v", first)
		}
		if first.ForgePR != 7 {
			t.Fatalf("fixture did not populate ForgePR: %d", first.ForgePR)
		}
		if first.LastTrailer == nil || len(first.LastTrailer.DiffStat) != 1 || first.LastTrailer.Usage.InputTokens != 100 {
			t.Fatalf("fixture did not populate Result projection: %+v", first.LastTrailer)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Rewrite the log with different content of identical length and the
		// same mtime: a fresh scan would see the new prompt, the cache must not.
		data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		rewritten := bytes.Replace(data, []byte(`"prompt":"task1"`), []byte(`"prompt":"zork1"`), 1)
		if len(rewritten) != len(data) {
			t.Fatalf("test bug: replacement changed length")
		}
		if err := os.WriteFile(path, rewritten, 0o600); err != nil { //nolint:gosec // path is test-controlled.
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		cached, err := loadLogHeader(testLogger(), path, true)
		if err != nil {
			t.Fatal(err)
		}
		if cached.Prompt != "task1" {
			t.Fatalf("cache served freshly scanned prompt %q, want cached %q", cached.Prompt, "task1")
		}
		if !bytes.Equal(inventoryJSON(t, cached), inventoryJSON(t, first)) {
			t.Errorf("cached projection diverged from first scan:\n cached=%s\n first =%s", inventoryJSON(t, cached), inventoryJSON(t, first))
		}
	})

	t.Run("InvalidatesOnLogChange", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task-a.jsonl")
		writeLogFile(t, dir, "task-a.jsonl",
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Harness: "claude"}),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "running"}),
		)
		if _, err := loadLogHeader(testLogger(), path, true); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		result := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", CostUSD: 9.99, NumTurns: 7})
		if _, err := f.WriteString(result + "\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadLogHeader(testLogger(), path, true)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != StatePurged || loaded.LastTrailer == nil || loaded.LastTrailer.CostUSD != 9.99 {
			t.Fatalf("stale cache after log change: state=%v result=%+v", loaded.State, loaded.LastTrailer)
		}
	})

	t.Run("FallsBackOnBadHeaderCache", func(t *testing.T) {
		t.Parallel()
		oversized := make([]byte, headerCacheMaxBytes+1)
		cases := []struct {
			name string
			body []byte
		}{
			{"CorruptJSON", []byte(`{"version":1,`)},
			{"WrongVersion", []byte(mustJSON(t, headerCache{Version: 999, Task: &LoadedTask{TaskID: "forged"}}))},
			{"Oversized", oversized},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writeMetaLog(t, t.TempDir(), "task1")
				if err := os.WriteFile(logHeaderCachePath(path), tc.body, 0o600); err != nil {
					t.Fatal(err)
				}
				loaded, err := loadLogHeader(testLogger(), path, true)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.TaskID == "forged" {
					t.Fatal("forged header cache content was served")
				}
				// The bad header cache entry must have been replaced by a valid one.
				again, err := loadLogHeader(testLogger(), path, true)
				if err != nil {
					t.Fatal(err)
				}
				if again.Prompt != "task1" {
					t.Fatalf("header cache was not rewritten: prompt=%q", again.Prompt)
				}
			})
		}
	})

	t.Run("ResultErrSurvivesRoundTrip", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task-a.jsonl")
		writeLogFile(t, dir, "task-a.jsonl",
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Harness: "claude"}),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "failed", Error: "boom"}),
		)
		if _, err := loadLogHeader(testLogger(), path, true); err != nil {
			t.Fatal(err)
		}
		cached, ok := readHeaderCache(path)
		if !ok {
			t.Fatal("readHeaderCache failed after successful scan")
		}
		if cached.LastTrailer == nil || cached.LastTrailer.Err == nil || cached.LastTrailer.Err.Error() != "boom" {
			t.Fatalf("cache lost result error: %+v", cached.LastTrailer)
		}
	})

	t.Run("OrphanHeaderCacheNotEnumeratedAsLog", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeMetaLog(t, dir, "task1")
		if err := os.WriteFile(filepath.Join(dir, "task-b.header.json"), []byte(`{"version":1,"task":{"task_id":"forged"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 || tasks[0].Prompt != "task1" {
			t.Fatalf("tasks = %+v, want only the written log", tasks)
		}
	})

	t.Run("CompressionSharesHeaderCache", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeMetaLog(t, dir, "task1")
		if _, err := loadLogHeader(testLogger(), path, true); err != nil {
			t.Fatal(err)
		}
		entry := logHeaderCachePath(path)
		if _, err := os.Stat(entry); err != nil {
			t.Fatalf("expected header cache after scan: %v", err)
		}
		store := NewStore(testLogger(), dir)
		compressed, err := store.compressPath(path)
		if err != nil {
			t.Fatal(err)
		}
		// The header cache is keyed by the log's base name, so the plain and
		// compressed forms share one entry; compression leaves it in place and
		// both paths map to the same file.
		if got := logHeaderCachePath(compressed); got != entry {
			t.Fatalf("header cache path: plain=%q compressed=%q", entry, got)
		}
		if _, err := os.Stat(entry); err != nil {
			t.Fatalf("shared header cache should survive compression: %v", err)
		}
		// Compression changed the log size, so the stale header cache entry is
		// rejected and a full scan rewrites it with the same projection.
		loaded, err := loadLogHeader(testLogger(), compressed, true)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Prompt != "task1" {
			t.Fatalf("compressed load prompt = %q", loaded.Prompt)
		}
	})

	t.Run("LiveTaskNotCached", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		name := "task-live.jsonl"
		path := filepath.Join(dir, name)
		writeLogFile(t, dir, name,
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "live", Harness: "claude"}),
			claudeAssistant(t, map[string]any{"type": "text", "text": "working"}),
		)
		if _, err := loadLogHeader(testLogger(), path, true); err != nil {
			t.Fatal(err)
		}
		// A live (non-terminal) log must not get a header cache: it is not the
		// compressed set and changes on every append.
		if _, err := os.Stat(logHeaderCachePath(path)); !os.IsNotExist(err) {
			t.Fatalf("live task must not be cached: header cache exists (%v)", err)
		}
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
	t.Run("StreamMessages", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		pr := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "o", ForgeRepo: "r", ForgePR: 5})
		a2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "waiting"})
		writeLogFile(t, dir, "t.jsonl", meta, a1, pr, a2, trailer)

		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages(t.Context()) {
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

	t.Run("StreamMessagesCancellation", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Harness: harness.Claude})
		first := claudeAssistant(t, map[string]any{"type": "text", "text": "first"})
		second := claudeAssistant(t, map[string]any{"type": "text", "text": "second"})
		writeLogFile(t, dir, "t.jsonl", meta, first, second)

		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var messages int
		var gotErr error
		for message, err := range tasks[0].StreamMessages(ctx) {
			if err != nil {
				gotErr = err
				continue
			}
			messages++
			cancel()
			if message.Message == nil {
				t.Fatal("streamed nil message")
			}
		}
		if messages != 1 {
			t.Fatalf("streamed %d messages, want 1", messages)
		}
		if !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("stream error = %v, want context.Canceled", gotErr)
		}
	})

	t.Run("StreamMessagesStopsWhenConsumerReturns", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Harness: harness.Claude})
		writeLogFile(t, dir, "t.jsonl", meta,
			`{"type":"assistant","message":{"content":[]}}`,
			`{"type":"assistant","message":{"content":[]}}`,
		)

		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		tasks[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) {
				calls++
				return []agent.Message{&agent.TextMessage{Text: "message"}}, nil
			}, nil
		})
		for _, err := range tasks[0].StreamMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if calls != 1 {
			t.Fatalf("native parser calls = %d, want 1", calls)
		}
	})

	t.Run("StreamMessagesIncludesProvisioningLogs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		setupLog := mustJSON(t, map[string]string{"type": "caic_log", "line": "creating runtime"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "failed"})
		writeLogFile(t, dir, "t.jsonl", meta, setupLog, trailer)

		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)

		var streamed []agent.Message
		for m, e := range tasks[0].StreamMessages(t.Context()) {
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

	t.Run("StreamMessagesIncludesReplayControls", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "replay controls", Harness: harness.Claude})
		writeLogFile(t, dir, "t.jsonl", meta,
			`{"t":"exit","exit_code":2,"error":"failed"}`,
			`{"t":"log","line":"provisioning"}`,
			`{"t":"context_cleared"}`,
		)
		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		var replayed []string
		for parsed, err := range lt.StreamMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			replayed = append(replayed, parsed.Message.Type())
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

		st := NewStore(testLogger(), dir)
		st.cutoff, st.maxSettledPerRepo = time.Time{}, 0
		tasks, err := st.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages(t.Context()) {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m.Message)
		}
		if len(streamed) != 2 {
			t.Fatalf("streamed %d messages, want 2", len(streamed))
		}
	})

	t.Run("BackwardMessagesCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "backward task", Harness: harness.Claude})
		first := claudeAssistant(t, map[string]any{"type": "text", "text": "first"})
		second := claudeAssistant(t, map[string]any{"type": "text", "text": "second"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(meta, first, second, trailer))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)

		var texts []string
		for parsed, err := range tasks[0].BackwardMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			text, ok := parsed.Message.(*agent.TextMessage)
			if ok {
				texts = append(texts, text.Text)
			}
		}
		if want := []string{"second", "first"}; !slices.Equal(texts, want) {
			t.Fatalf("backward text messages = %q, want %q", texts, want)
		}
	})

	t.Run("BackwardMessagesStopsBeforeOlderTurn", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "backward suffix", Harness: harness.Claude})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(
			meta,
			`{"kind":"bad"}`,
			`{"kind":"result","text":"old result"}`,
			`{"kind":"text","text":"new text"}`,
			`{"kind":"result","text":"new result"}`,
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"}),
		))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		badParses := 0
		tasks[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func(line []byte) ([]agent.Message, error) {
				var record struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					return nil, err
				}
				switch record.Kind {
				case "bad":
					badParses++
					return nil, errors.New("older turn is invalid")
				case "result":
					return []agent.Message{&agent.ResultMessage{Result: record.Text}}, nil
				case "text":
					return []agent.Message{&agent.TextMessage{Text: record.Text}}, nil
				default:
					return nil, nil
				}
			}, nil
		})

		var got []string
		for parsed, err := range tasks[0].BackwardMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			switch message := parsed.Message.(type) {
			case *agent.ResultMessage:
				got = append(got, message.Result)
			case *agent.TextMessage:
				got = append(got, message.Text)
			}
			if len(got) == 2 {
				break
			}
		}
		if want := []string{"new result", "new text"}; !slices.Equal(got, want) {
			t.Fatalf("backward newest turn = %q, want %q", got, want)
		}
		if badParses != 0 {
			t.Fatalf("older invalid record parsed %d times, want 0", badParses)
		}
	})

	t.Run("BackwardMessagesStopsAfterStableRecord", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "backward stable", Harness: harness.Claude})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(
			meta,
			`{"kind":"bad"}`,
			`{"kind":"text","text":"latest"}`,
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"}),
		))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		badParses := 0
		tasks[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func(line []byte) ([]agent.Message, error) {
				var record struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					return nil, err
				}
				if record.Kind == "bad" {
					badParses++
					return nil, errors.New("older record is invalid")
				}
				if record.Kind == "text" {
					return []agent.Message{&agent.TextMessage{Text: record.Text}}, nil
				}
				return nil, nil
			}, nil
		})

		var got string
		for parsed, err := range tasks[0].BackwardMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			if text, ok := parsed.Message.(*agent.TextMessage); ok {
				got = text.Text
				break
			}
		}
		if got != "latest" {
			t.Fatalf("latest stable text = %q, want latest", got)
		}
		if badParses != 0 {
			t.Fatalf("older invalid record parsed %d times, want 0", badParses)
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
				for _, err := range lt.StreamMessages(t.Context()) {
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
				for msg, err := range lt.StreamMessages(t.Context()) {
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
				for msg, err := range lt.StreamMessages(t.Context()) {
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
			_, err := loadSemanticLog("/does/not/exist.jsonl", nil)
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
			for _, e := range lt.StreamMessages(t.Context()) {
				if e != nil {
					gotErr = true
				}
			}
			if !gotErr {
				t.Error("StreamMessages without parser should yield an error")
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

func TestExportDiscussionReadsPlainAndCompressedLogs(t *testing.T) {
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
