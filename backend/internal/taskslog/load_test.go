// Tests for task loading and configuration resolution.

package taskslog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

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

func TestLoadSemanticTail(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "tail"})
	lines := []string{
		`{"type":"assistant","text":"first"}`,
		`{"type":"assistant","text":"second"}`,
		`{"type":"assistant","text":"third"}`,
	}
	trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Title: "done"})
	for _, compressed := range []bool{false, true} {
		format := "plain"
		if compressed {
			format = "zstd"
		}
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, compressed, append(append([]string{meta}, lines...), trailer)...)
			calls := 0
			loaded, err := loadSemanticTail(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
				if h != harness.Claude {
					t.Fatalf("resolver harness = %q, want %q", h, harness.Claude)
				}
				return func(raw []byte) ([]agent.Message, error) {
					calls++
					return []agent.Message{&agent.TextMessage{Text: string(raw)}}, nil
				}, nil
			}, int64(len(lines[2])+len(trailer)+2))
			if err != nil {
				t.Fatal(err)
			}
			if calls != len(lines) {
				t.Fatalf("native parser calls = %d, want %d", calls, len(lines))
			}
			if len(loaded.Msgs) != 1 {
				t.Fatalf("retained messages = %#v, want only third", loaded.Msgs)
			}
			text, ok := loaded.Msgs[0].(*agent.TextMessage)
			if !ok || !strings.Contains(text.Text, "third") {
				t.Fatalf("retained message = %#v, want third text", loaded.Msgs[0])
			}
			if loaded.State != StatePurged || loaded.Result == nil || loaded.Title != "done" {
				t.Fatalf("tail metadata = state %q result %#v title %q, want purged result and done", loaded.State, loaded.Result, loaded.Title)
			}
		})
	}
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

func TestStoreLoad(t *testing.T) {
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

		tasks, err := storeFor(filepath.Dir(path)).Load()
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
		tasks, err := storeFor(dir).Load()
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
				if _, err := loadSemanticLog(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
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
					if _, err := loadSemanticLog(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
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

		tasks, err := storeFor(dir).Load()
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
				tasks, err := storeFor(filepath.Dir(path)).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

	t.Run("PreferPlainSourceAfterInterruptedCompression", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		plainMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plain", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		compressedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(compressedMeta, trailer))
		writeLogFile(t, dir, "a.jsonl", plainMeta, trailer)

		tasks, err := storeFor(dir).Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Prompt != "plain" {
			t.Errorf("Prompt = %q, want plain", tasks[0].Prompt)
		}
	})
	t.Run("ForTaskIDs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wantedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "wanted", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		unrelatedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "unrelated", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		writeLogFile(t, dir, "live1-repo-branch.jsonl", wantedMeta)
		writeLogFile(t, dir, "live10-repo-branch.jsonl", unrelatedMeta)

		tasks, err := storeFor(dir).LoadForTaskIDs([]string{"live1"})
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
		if _, err := storeFor(t.TempDir()).LoadForTaskIDs([]string{"missing"}); err == nil || !strings.Contains(err.Error(), "missing task logs") {
			t.Fatalf("LoadForTaskIDs error = %v, want missing-log error", err)
		}
	})
	t.Run("ForTaskIDsInvalid", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeLogFile(t, dir, "broken-repo-branch.jsonl", `{"type":"assistant"}`)
		_, err := storeFor(dir).LoadForTaskIDs([]string{"broken"})
		if err == nil || !strings.Contains(err.Error(), "load task log") {
			t.Fatalf("LoadForTaskIDs error = %v, want invalid-log error", err)
		}
	})
	t.Run("NotExist", func(t *testing.T) {
		t.Parallel()
		tasks, err := storeFor(filepath.Join(t.TempDir(), "nope")).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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
		lt, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
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

		_, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
			if h != harness.Claude {
				t.Fatalf("resolver harness = %q, want claude", h)
			}
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err == nil || !strings.Contains(err.Error(), "unknown top-level t") {
			t.Fatalf("v2 caic_session alias error = %v, want strict unknown-token rejection", err)
		}
		tasks, err := storeFor(filepath.Dir(path)).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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
				if _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				}); err == nil || !strings.Contains(err.Error(), "invalid first log header") {
					t.Fatalf("error = %v, want invalid first header", err)
				}
			})
			t.Run(format+" mixed authority after metadata", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, meta, session, mismatch)
				if _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
					return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
				}); err == nil || !strings.Contains(err.Error(), "wrong t discriminator") {
					t.Fatalf("error = %v, want wrong t discriminator", err)
				}
			})
			t.Run(format+" leading empty lines", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, "", "  ", meta, session)
				lt, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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
		// The context-clear marker remains present for Task to apply during restoration.
		found := false
		for _, message := range lt.Msgs {
			if marker, ok := message.(*agent.SystemMessage); ok && marker.Subtype == "context_cleared" {
				found = true
			}
		}
		if !found {
			t.Error("context-cleared marker missing from loaded messages")
		}
	})
	t.Run("PRHeaderOnly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octocat", ForgeRepo: "hello", ForgePR: 42})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "1-r-caic-1.jsonl", meta, prMsg, trailer)

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

func TestStoreLoadManyFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	line := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "task",
		Harness:     harness.Claude,
	})
	for i := range 512 {
		writeLogFile(t, dir, fmt.Sprintf("task-%03d.jsonl", i), line)
	}

	loaded, err := storeFor(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 512 {
		t.Fatalf("loaded %d task logs, want 512", len(loaded))
	}
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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
		tasks, err := storeFor(dir).Load()
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

		tasks, err := storeFor(dir).Load()
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
