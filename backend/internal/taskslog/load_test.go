// Tests for task loading and configuration resolution.

package taskslog

import (
	"bufio"
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

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tokens"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func setClaudeParser(tasks []*LoadedTask) {
	for _, lt := range tasks {
		lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return claudecode.New().NewWire(), nil
		}))
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

func mustJSON(t testing.TB, v any) string {
	var data []byte
	var err error
	switch m := v.(type) {
	case agent.MetaMessage:
		return mustJSON(t, &m)
	case agent.MetaSessionMessage:
		return mustJSON(t, &m)
	case agent.MetaPRMessage:
		return mustJSON(t, &m)
	case agent.MetaResultMessage:
		return mustJSON(t, &m)
	case agent.DiffStatMessage:
		return mustJSON(t, &m)
	case agent.SystemMessage:
		return mustJSON(t, &m)
	case agent.LogMessage:
		return mustJSON(t, &m)
	case agent.TurnCommitSnapshotMessage:
		return mustJSON(t, &m)
	case agent.PendingUserActionMessage:
		return mustJSON(t, &m)
	case agent.Message:
		data, err = agent.MarshalLogMessage(m)
	default:
		data, err = json.Marshal(v)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// nativeLogLine frames a native output payload in its physical task-log record.
func nativeLogLine(line string) string {
	return `{"t":"agent","ts":1.000,"msg":` + line + `}`
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
	return nativeLogLine(mustJSON(t, msg))
}

// claudeInit builds a Claude wire-format system/init NDJSON line.
func claudeInit(t *testing.T, sessionID string) string {
	return nativeLogLine(mustJSON(t, map[string]any{"type": "system", "subtype": "init", "session_id": sessionID}))
}

func TestScanPhysicalLogHeaderOnlyAndEOFValidation(t *testing.T) {
	t.Parallel()
	path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV3),
		Prompt:      "header only",
		Harness:     string(harness.Claude),
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
	const header = `{"t":"caic_meta","version":3,"prompt":"task","repos":[],"harness":"claude"}`
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, "", header, nativeLogLine(`{"type":"assistant"}`), header)
				authority, err := readLogAuthority(path)
				if err != nil {
					t.Fatal(err)
				}
				if authority.Harness != harness.Claude {
					t.Fatalf("authority=%+v", authority)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name      string
			lines     []string
			errorText string
		}{
			{"missing header", []string{nativeLogLine(`{"type":"assistant"}`)}, "missing version"},
			{"malformed header", []string{`{"t":"caic_meta"`}, "unexpected end of JSON input"},
			{"missing version", []string{`{"t":"caic_meta","prompt":"task","harness":"claude"}`}, "missing version"},
			{"v1 header", []string{`{"type":"caic_meta","version":1,"prompt":"task","harness":"claude"}`}, "unsupported log version 1"},
			{"v2 header", []string{`{"t":"caic_meta","version":2,"prompt":"task","harness":"claude"}`}, "unsupported log version 2"},
			{"future header", []string{`{"t":"caic_meta","version":4,"prompt":"task","harness":"claude"}`}, "unsupported log version 4"},
			{"changed harness", []string{header, `{"t":"caic_meta","version":3,"prompt":"task","harness":"codex"}`}, `harness changed from "claude" to "codex"`},
			{"changed version", []string{header, `{"t":"caic_meta","version":2,"prompt":"task","harness":"claude"}`}, "unsupported log version 2"},
			{"duplicate prompt", []string{`{"t":"caic_meta","version":3,"prompt":"first","prompt":"last","harness":"claude"}`}, `duplicate raw key "prompt"`},
			{"duplicate discriminator", []string{header, `{"t":"caic_meta","t":"agent","version":3,"prompt":"task","harness":"claude"}`}, `duplicate raw key "t"`},
			{"unknown field", []string{`{"t":"caic_meta","version":3,"prompt":"task","harness":"claude","unknown":true}`}, `unknown field "unknown"`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, compressed := range []bool{false, true} {
					t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, compressed, tc.lines...)
						if _, err := readLogAuthority(path); err == nil || !strings.Contains(err.Error(), tc.errorText) {
							t.Fatalf("invalid authority error = %v, want %q", err, tc.errorText)
						}
					})
				}
			})
		}
	})
	t.Run("deep native record", func(t *testing.T) {
		t.Parallel()
		deep := strings.Repeat(`{"k":`, maxJSONNesting+2) + `1` + strings.Repeat(`}`, maxJSONNesting+2)
		path := writePhysicalTestLog(t, false, header, nativeLogLine(`{"type":"assistant","data":`+deep+`}`))
		if _, err := readLogAuthority(path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		if _, err := readLogAuthority(filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing file error=%v", err)
		}
	})
}

func TestDecodeDiscriminatorProbe(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, line string
			candidate  bool
		}{
			{"agent", `{"t":"agent","ts":1.000,"msg":{"type":"assistant"}}`, false},
			{"input", `{"t":"input","ts":1.000,"msg":{"type":"user"}}`, false},
			{"header", `{"t":"caic_meta","version":3}`, true},
			{"reordered header", `{"prompt":"task","t":"caic_meta"}`, true},
			{"whitespace", `{"t": "caic_meta"}`, true},
			{"escaped value", `{"t":"a\"b"}`, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				candidate, err := decodeDiscriminatorProbe([]byte(tc.line))
				if err != nil {
					t.Fatal(err)
				}
				if candidate != tc.candidate {
					t.Fatalf("header candidate=%t,want %t", candidate, tc.candidate)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		deep := strings.Repeat(`{"k":`, maxJSONNesting+2) + `1` + strings.Repeat(`}`, maxJSONNesting+2)
		if _, err := decodeDiscriminatorProbe([]byte(`{"t":` + deep + `}`)); !errors.Is(err, errJSONNesting) {
			t.Fatalf("nesting error=%v", err)
		}
	})
}

// inventoryJSON renders LoadedTask's JSON projection for testing comparison,
// sidestepping time.Time DeepEqual location sensitivity.
func inventoryJSON(t *testing.T, lt *LoadedTask) []byte {
	data, err := json.Marshal(headerTaskToData(lt))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadLogHeader(t *testing.T) {
	t.Parallel()
	t.Run("RecordedClaudeReasoningLifecycle", func(t *testing.T) {
		t.Parallel()
		// This retained trace reports 58, 237 and 43 thinking tokens in usage
		// events, while its final native result omits output_tokens_details.
		f, err := os.Open("../agent/claudecode/testdata/read-edit-bash.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		var lines []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		// A failed revival retains the completed run's native accounting and
		// remains terminal, so the same projection must survive the cache.
		lines = append(lines, `{"t":"result","state":"failed","title":"reviving"}`)
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, lines...)
				var first []byte
				for pass := range 2 {
					loaded, err := loadLogHeader(testLogger(), path, true)
					if err != nil {
						t.Fatal(err)
					}
					if loaded.LastTrailer == nil || loaded.LastTrailer.Usage.ReasoningOutputTokens != 338 {
						t.Fatalf("recorded reasoning accounting = %+v, want 338", loaded.LastTrailer)
					}
					before := inventoryJSON(t, loaded)
					if pass == 0 {
						first = before
					} else if !bytes.Equal(before, first) {
						t.Fatalf("cached inventory changed metadata:\nfirst=%s\ncached=%s", first, before)
					}
					if _, ok := readHeaderCache(path); !ok {
						t.Fatal("terminal inventory did not populate a usable cache")
					}
					if err := loaded.LoadMessagesWithResolver(agent.Backends{harness.Claude: claudecode.New()}); err != nil {
						t.Fatal(err)
					}
					if after := inventoryJSON(t, loaded); !bytes.Equal(before, after) {
						t.Errorf("lazy load changed recorded accounting:\nbefore=%s\nafter=%s", before, after)
					}
				}
			})
		}
	})
	t.Run("NativeMetadataLifecycle", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, state, accounting string
			cost                    float64
			duration                time.Duration
			turns                   int
			usage                   agent.Usage
		}{
			{"interrupted revival", "running", "", 1.25, 2500 * time.Millisecond, 3,
				agent.Usage{InputTokens: 100, OutputTokens: 40, CacheCreationInputTokens: 12,
					CacheWrites:          tokens.CacheWrites{{TTL: 5 * time.Minute, Tokens: 4}, {TTL: time.Hour, Tokens: 8}},
					CacheReadInputTokens: 14, ReasoningOutputTokens: 9, CacheTTLSeconds: 300}},
			{"failed revival", "failed", "", 1.25, 2500 * time.Millisecond, 3,
				agent.Usage{InputTokens: 100, OutputTokens: 40, CacheCreationInputTokens: 12,
					CacheWrites:          tokens.CacheWrites{{TTL: 5 * time.Minute, Tokens: 4}, {TTL: time.Hour, Tokens: 8}},
					CacheReadInputTokens: 14, ReasoningOutputTokens: 9, CacheTTLSeconds: 300}},
			{"authoritative trailer", "purged", `,"cost_usd":2.5,"duration":5,"num_turns":6,"input_tokens":200,"output_tokens":80,"cache_creation_input_tokens":20,"cache_creation_1h_input_tokens":20,"cache_read_input_tokens":28,"reasoning_output_tokens":18`,
				2.5, 5 * time.Second, 6, agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 20,
					CacheWrites: tokens.CacheWrites{{TTL: time.Hour, Tokens: 20}}, CacheReadInputTokens: 28, ReasoningOutputTokens: 18}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, compressed := range []bool{false, true} {
					t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, compressed,
							`{"t":"caic_meta","version":3,"prompt":"task","title":"original","repos":[{"name":"org/repo","branch":"caic-0"}],"harness":"claude","model":"requested","effort":"medium","started_at":"2026-01-01T00:00:00Z","forge_issue":5,"owner_id":"owner","parent_task_id":"parent","forked_from_task_id":"fork","caic_mcp":true}`,
							nativeLogLine(`{"type":"system","subtype":"init","session_id":"root","model":"claude-opus-x","effort":"high","claude_code_version":"2.1.0"}`),
							nativeLogLine(`{"type":"system","subtype":"init","session_id":"child","parent_tool_use_id":"tool-child","model":"child-model","effort":"low","claude_code_version":"child-version"}`),
							nativeLogLine(`{"type":"result","subtype":"success","duration_ms":1000,"num_turns":1,"total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":4}}`),
							nativeLogLine(`{"type":"result","subtype":"success","duration_ms":2500,"num_turns":3,"total_cost_usd":1.25,"usage":{"input_tokens":100,"output_tokens":40,"cache_creation_input_tokens":12,"cache_creation":{"ephemeral_1h_input_tokens":8,"ephemeral_5m_input_tokens":4},"cache_read_input_tokens":14,"output_tokens_details":{"thinking_tokens":9}}}`),
							nativeLogLine(`{"type":"result","subtype":"success","parent_tool_use_id":"tool-child","duration_ms":9000,"num_turns":9,"total_cost_usd":50,"usage":{"input_tokens":900,"output_tokens":400}}`),
							`{"t":"pr","forge_owner":"org","forge_repo":"repo","forge_pr":7}`,
							`{"t":"diff_stat","ts":1767225600.5,"diff_stat":[{"path":"main.go","added":4,"deleted":1}]}`,
							`{"t":"result","state":"purged","cost_usd":1.25}`,
							fmt.Sprintf(`{"t":"result","state":%q,"title":"reviving","diff_stat":[{"path":"main.go","added":4,"deleted":1}],"disk_used_bytes":456,"error":"launch failed","agent_result":"completed","startup_failure":{"harness":"claude","phase":"launch","cause":"interrupted"}%s}`, tc.state, tc.accounting),
						)
						stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
						if err := os.Chtimes(path, stamp, stamp); err != nil {
							t.Fatal(err)
						}
						var expected []byte
						for pass := range 2 { // Second load reopens the terminal header cache.
							loaded, err := loadLogHeader(testLogger(), path, true)
							if err != nil {
								t.Fatal(err)
							}
							if loaded.SessionID != "root" || loaded.ReportedModel != "claude-opus-x" || loaded.ReportedEffort != "high" || loaded.AgentVersion != "2.1.0" {
								t.Fatalf("root session metadata lost: %+v", loaded)
							}
							if loaded.State != task.State(tc.state) || loaded.Title != "reviving" || loaded.ForgeOwner != "org" || loaded.ForgeRepo != "repo" || loaded.ForgePR != 7 || !loaded.DiffCreated || !loaded.LastStateUpdateAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 500000000, time.UTC)) {
								t.Fatalf("control metadata lost: %+v", loaded)
							}
							if loaded.Prompt != "task" || loaded.RequestedModel != "requested" || loaded.RequestedEffort != "medium" || loaded.OwnerID != "owner" || loaded.ParentTaskID != "parent" || loaded.ForkedFromTaskID != "fork" || !loaded.CaicMCP || loaded.ForgeIssue != 5 || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "org/repo" {
								t.Fatalf("header metadata lost: %+v", loaded)
							}
							r := loaded.LastTrailer
							if r == nil || r.CostUSD != tc.cost || r.Duration != tc.duration || r.NumTurns != tc.turns || !r.Usage.Equal(tc.usage) {
								t.Fatalf("completed-run accounting lost: %+v, want usage %+v", r, tc.usage)
							}
							if r.State != task.State(tc.state) || r.DiskUsedBytes == nil || *r.DiskUsedBytes != 456 || r.Err == nil || r.Err.Error() != "launch failed" || r.AgentResult != "completed" || r.StartupFailure == nil || *r.StartupFailure != (v3.StartupFailure{Harness: "claude", Phase: "launch", Cause: "interrupted"}) || len(r.DiffStat) != 1 || r.DiffStat[0].Path != "main.go" || r.DiffStat[0].LinesAdded != 4 || r.DiffStat[0].LinesDeleted != 1 {
								t.Fatalf("latest trailer metadata lost: %+v", r)
							}
							if loaded.Timeline != nil {
								t.Fatal("inventory materialized the conversation")
							}
							before := inventoryJSON(t, loaded)
							if pass == 0 {
								expected = before
							} else if !bytes.Equal(before, expected) {
								t.Fatalf("reopened inventory differs:\nfirst=%s\nreopened=%s", expected, before)
							}
							if tc.state != "running" {
								if _, ok := readHeaderCache(path); !ok {
									t.Fatal("terminal inventory did not populate a usable header cache")
								}
							}
							if err := loaded.LoadMessagesWithResolver(agent.Backends{harness.Claude: claudecode.New()}); err != nil {
								t.Fatal(err)
							}
							if after := inventoryJSON(t, loaded); !bytes.Equal(after, before) {
								t.Errorf("lazy load changed metadata:\nbefore=%s\nafter =%s", before, after)
							}
						}
					})
				}
			})
		}
	})
	writeMetaLog := func(t *testing.T, dir, prompt string) string {
		name := "task-a.jsonl"
		path := filepath.Join(dir, name)
		writeLogFile(t, dir, name,
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: prompt, Harness: "claude", RequestedModel: "requested",
				Repos: []v3.MetaRepo{{Name: "org/repo", Branch: "caic-0"}}}),
			mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "sess-1", ReportedModel: "claude-sonnet-4-6", AgentVersion: "2.1.0"}),
			mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "org", ForgeRepo: "repo", ForgePR: 7}),
			mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 4, LinesDeleted: 1}}, Ts: 1767225600.5}),
			claudeAssistant(t, map[string]any{"type": "text", "text": "hello"}),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Title: "done", CostUSD: 1.5,
				Duration: 2.5, NumTurns: 2, InputTokens: 100, OutputTokens: 200,
				DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 4, LinesDeleted: 1}}, DiskUsedBytes: new(int64(456))}),
		)
		return path
	}

	t.Run("AutoCompactThreshold", func(t *testing.T) {
		t.Parallel()
		for _, h := range []harness.Name{harness.Claude, harness.Codex} {
			t.Run(string(h), func(t *testing.T) {
				t.Parallel()
				for _, compressed := range []bool{false, true} {
					t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, compressed, mustJSON(t, agent.MetaMessage{Version: 3, Harness: string(h), Prompt: "test"}),
							`{"t":"session","session_id":"session","agent_version":"1.0","auto_compact_token_limit":244800}`,
							`{"t":"result","state":"purged"}`)
						resolver := backends.Default("", nil)
						semantic, err := loadSemanticTask(path, resolver)
						if err != nil {
							t.Fatal(err)
						}
						if semantic.AutoCompactTokenLimit != 244800 {
							t.Fatalf("semantic auto-compact threshold = %d, want 244800", semantic.AutoCompactTokenLimit)
						}
						for range 2 {
							loaded, err := loadLogHeader(testLogger(), path, true)
							if err != nil {
								t.Fatal(err)
							}
							if loaded.AutoCompactTokenLimit != 244800 {
								t.Fatalf("restored auto-compact threshold = %d, want 244800", loaded.AutoCompactTokenLimit)
							}
							before := inventoryJSON(t, loaded)
							if err := loaded.LoadMessagesWithResolver(resolver); err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(inventoryJSON(t, loaded), before) {
								t.Fatal("lazy load changed auto-compact metadata")
							}
						}
					})
				}
			})
		}
	})

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
		if first.SessionID != "sess-1" || first.RequestedModel != "requested" || first.ReportedModel != "claude-sonnet-4-6" || first.AgentVersion != "2.1.0" {
			t.Fatalf("fixture did not populate session metadata: %+v", first)
		}
		if first.ForgePR != 7 {
			t.Fatalf("fixture did not populate ForgePR: %d", first.ForgePR)
		}
		if first.LastTrailer == nil || len(first.LastTrailer.DiffStat) != 1 || first.LastTrailer.Usage.InputTokens != 100 {
			t.Fatalf("fixture did not populate Result projection: %+v", first.LastTrailer)
		}
		if first.LastTrailer.DiskUsedBytes == nil || *first.LastTrailer.DiskUsedBytes != 456 {
			t.Fatalf("DiskUsedBytes = %v, want 456", first.LastTrailer.DiskUsedBytes)
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

	t.Run("AcceptsStandaloneCommitSnapshot", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		snapshot, err := agent.MarshalLogMessage(agent.NewTurnCommitSnapshotMessage([]v3.RepositoryCommit{{
			RepositoryPath: "/home/user/src/repo",
			BranchName:     "caic-1",
			CommitHash:     "1111111111111111111111111111111111111111",
		}}, false, nil))
		if err != nil {
			t.Fatal(err)
		}
		trailer, err := agent.MarshalLogMessage(&agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "task.jsonl")
		writeLogFile(t, dir, filepath.Base(path),
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Prompt: "snapshot", Harness: string(harness.Codex)}),
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"thread"}}}}`,
			string(snapshot),
			string(trailer),
		)
		loaded, err := loadLogHeader(testLogger(), path, true)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != task.StatePurged {
			t.Fatalf("state = %s, want %s", loaded.State, task.StatePurged)
		}
	})

	t.Run("InvalidatesOnLogChange", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "task-a.jsonl")
		writeLogFile(t, dir, "task-a.jsonl",
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Harness: "claude"}),
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
		if loaded.State != task.StatePurged || loaded.LastTrailer == nil || loaded.LastTrailer.CostUSD != 9.99 {
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
			{"CorruptJSON", []byte(`{"version":3,`)},
			{"WrongVersion", []byte(`{"version":999,"task":{"task_id":"forged"}}`)},
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
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Harness: "claude"}),
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
		if err := os.WriteFile(filepath.Join(dir, "task-b.header.json"), []byte(`{"version":3,"task":{"task_id":"forged"}}`), 0o600); err != nil {
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
			mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "live", Harness: "claude"}),
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

func TestLoadRelayBoundaries(t *testing.T) {
	t.Parallel()
	meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "boundary scan", Harness: string(harness.OpenCode)})
	t.Run("unmarked repeated native records", func(t *testing.T) {
		t.Parallel()
		native := `{"t":"agent","ts":1.000,"msg":{"kind":"empty"}}`
		path := writePhysicalTestLog(t, false, meta, native, native)
		records, generation, err := LoadRelayBoundaries(path)
		if err != nil || generation != "" || len(records) != 2 {
			t.Fatalf("boundaries = %+v, generation = %q, error = %v", records, generation, err)
		}
		for i, record := range records {
			if want := int64((i + 1) * (len(native) + 1)); record.RelayEnd != want {
				t.Fatalf("boundary %d = %+v, want offset %d", i, record, want)
			}
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed_%v", compressed), func(t *testing.T) {
				t.Parallel()
				old := `{"t":"relay_generation","generation":"old"}`
				current := `{"t":"relay_generation","generation":"current"}`
				native := `{"t":"agent","ts":1.000,"msg":{"untyped":true}}`
				input := `{"t":"input","ts":2.000,"msg":{"client":"request"}}`
				host := mustJSON(t, &agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "host"})
				path := writePhysicalTestLog(t, compressed, meta, old, native, current, native, input, host, native)
				records, generation, err := LoadRelayBoundaries(path)
				if err != nil {
					t.Fatal(err)
				}
				if generation != "current" || len(records) != 3 {
					t.Fatalf("generation = %q, records = %+v", generation, records)
				}
				want := []int64{int64(len(current) + 1), int64(len(current) + len(native) + 2), int64(len(current) + 2*len(native) + 3)}
				for i, record := range records {
					if record.Generation != "current" || record.RelayEnd != want[i] {
						t.Fatalf("record %d = %+v, want current generation offset %d", i, record, want[i])
					}
				}
				if records[1].Fingerprint == ([32]byte{}) || records[2].Fingerprint != records[1].Fingerprint {
					t.Fatalf("native fingerprints = %x, %x", records[1].Fingerprint, records[2].Fingerprint)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{
			`{"t":"agent","ts":1.000,"msg":{"invalid":}}`,
			`{"t":"input","ts":1,"msg":{"client":"request"}}`,
			`{"t":"relay_generation","generation":""}`,
			`{"t":"result","state":123}`,
		} {
			t.Run(line, func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, true, meta, line)
				if records, generation, err := LoadRelayBoundaries(path); err == nil || records != nil || generation != "" {
					t.Fatalf("boundaries = %+v, generation = %q, err = %v", records, generation, err)
				}
			})
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

func TestV3DirectionalNativeLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const input = `{"type":"control_response","response":{"request_id":"ask-1","answer":"Identity only"}}`
	const output = `{"type":"assistant","text":"thanks"}`
	writeLogFile(t, dir, "task.jsonl",
		mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV3),
			Prompt:      "task",
			Harness:     string(harness.Claude),
		}),
		`{"t":"input","ts":1.000,"msg":`+input+`}`,
		`{"t":"agent","ts":2.000,"msg":`+output+`}`,
	)
	tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("loaded tasks = %d, want 1", len(tasks))
	}
	var native [][]byte
	var directions []string
	if err := tasks[0].LoadMessagesWithResolver(newTestWireResolver(func(h harness.Name) (agent.WireFormat, error) {
		if h != harness.Claude {
			return nil, fmt.Errorf("harness = %q, want %q", h, harness.Claude)
		}
		return &agenttest.Wire{
			Input: func(data []byte) ([]agent.Message, error) {
				directions = append(directions, "input")
				native = append(native, bytes.Clone(data))
				return nil, nil
			},
			Output: func(data []byte) ([]agent.Message, error) {
				directions = append(directions, "output")
				native = append(native, bytes.Clone(data))
				return nil, nil
			},
		}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if len(native) != 2 || string(native[0]) != input || string(native[1]) != output {
		t.Fatalf("native timeline = %q, want input then output", native)
	}
	if !slices.Equal(directions, []string{"input", "output"}) {
		t.Fatalf("parser directions = %v", directions)
	}
}

func TestLoadedTask(t *testing.T) {
	t.Parallel()
	t.Run("StreamMessages", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "stream task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
		if len(streamed) != len(lt.Timeline) {
			t.Fatalf("streamed %d messages, full load %d", len(streamed), len(lt.Timeline))
		}
	})

	t.Run("StreamMessagesCancellation", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "stream task", Harness: string(harness.Claude)})
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
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "stream task", Harness: string(harness.Claude)})
		writeLogFile(t, dir, "t.jsonl", meta,
			nativeLogLine(`{"type":"assistant","message":{"content":[]}}`),
			nativeLogLine(`{"type":"assistant","message":{"content":[]}}`),
		)

		tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		tasks[0].SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) {
				calls++
				return []agent.Message{&agent.TextMessage{Text: "message"}}, nil
			}}, nil
		}))

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
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "stream task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		setupLog := mustJSON(t, agent.LogMessage{Line: "creating runtime"})
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
		if len(tasks[0].Timeline) != 1 {
			t.Fatalf("loaded %d messages, want 1", len(tasks[0].Timeline))
		}
		loaded, ok := tasks[0].Timeline[0].Message.(*agent.LogMessage)
		if !ok || loaded.Line != "creating runtime" {
			t.Fatalf("loaded message = %#v, want provisioning log", tasks[0].Timeline[0])
		}
	})

	t.Run("StreamMessagesIncludesReplayControls", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Prompt: "replay controls", Harness: string(harness.Claude)})
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
		lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return nil, nil }}, nil
		}))

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
		loaded := make([]string, 0, len(lt.Timeline))
		for _, entry := range lt.Timeline {
			loaded = append(loaded, entry.Message.Type())
		}
		if !slices.Equal(replayed, loaded) {
			t.Fatalf("replay controls = %q, live semantic controls = %q", replayed, loaded)
		}
	})

	t.Run("StreamMessagesCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "stream task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "backward task", Harness: string(harness.Claude)})
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
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "backward suffix", Harness: string(harness.Claude)})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(
			meta,
			nativeLogLine(`{"kind":"bad"}`),
			nativeLogLine(`{"kind":"result","text":"old result"}`),
			nativeLogLine(`{"kind":"text","text":"new text"}`),
			nativeLogLine(`{"kind":"result","text":"new result"}`),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"}),
		))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		badParses := 0
		tasks[0].SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func(line []byte) ([]agent.Message, error) {
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
			}}, nil
		}))

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

	t.Run("BackwardMessagesKeepsLatestCommitSnapshot", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "backward snapshot", Harness: string(harness.Claude)})
		snapshot := mustJSON(t, agent.NewTurnCommitSnapshotMessage([]v3.RepositoryCommit{{
			RepositoryPath: "/home/user/src/repo",
			BranchName:     "caic-1",
			CommitHash:     "1111111111111111111111111111111111111111",
		}}, false, nil))
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(
			meta,
			nativeLogLine(`{"kind":"result","text":"old result"}`),
			nativeLogLine(`{"kind":"text","text":"new text"}`),
			nativeLogLine(`{"kind":"result","text":"new result"}`),
			snapshot,
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"}),
		))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("loaded tasks = %d, want 1", len(tasks))
		}
		tasks[0].SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func(line []byte) ([]agent.Message, error) {
				var record struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					return nil, err
				}
				switch record.Kind {
				case "result":
					return []agent.Message{&agent.ResultMessage{Result: record.Text}}, nil
				case "text":
					return []agent.Message{&agent.TextMessage{Text: record.Text}}, nil
				default:
					return nil, nil
				}
			}}, nil
		}))

		var got []string
		for parsed, err := range tasks[0].BackwardMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			switch message := parsed.Message.(type) {
			case *agent.TurnCommitSnapshotMessage:
				got = append(got, "snapshot:"+message.RepositoryCommits[0].CommitHash)
			case *agent.ResultMessage:
				got = append(got, message.Result)
			case *agent.TextMessage:
				got = append(got, message.Text)
			}
			if len(got) == 3 {
				break
			}
		}
		if want := []string{"snapshot:1111111111111111111111111111111111111111", "new result", "new text"}; !slices.Equal(got, want) {
			t.Fatalf("backward newest turn = %q, want %q", got, want)
		}
	})

	t.Run("BackwardMessagesStopsAfterStableRecord", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "backward stable", Harness: string(harness.Claude)})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(
			meta,
			nativeLogLine(`{"kind":"bad"}`),
			nativeLogLine(`{"kind":"text","text":"latest"}`),
			mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"}),
		))

		store := NewStore(testLogger(), dir)
		store.cutoff, store.maxSettledPerRepo = time.Time{}, 0
		tasks, err := store.LoadSettled()
		if err != nil {
			t.Fatal(err)
		}
		badParses := 0
		tasks[0].SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func(line []byte) ([]agent.Message, error) {
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
			}}, nil
		}))

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
			MessageType: "caic_meta", Version: 3, Prompt: "stream task", Harness: string(harness.Claude),
		})
		mismatch := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 3, Prompt: "stream task", Harness: string(harness.Codex),
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
				lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
					return claudecode.New().NewWire(), nil
				}))

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
				lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
					return claudecode.New().NewWire(), nil
				}))

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
				if gotErr == nil || !strings.Contains(gotErr.Error(), "harness changed") {
					t.Fatalf("error = %v, want harness changed", gotErr)
				}
			})
			t.Run(format+" leading empty lines", func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, compressed, "", "  ", meta, message)
				lt := &LoadedTask{path: path, Harness: harness.Claude}
				lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
					return claudecode.New().NewWire(), nil
				}))

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

	t.Run("SemanticMessagesPreserveProducerTimes", func(t *testing.T) {
		t.Parallel()
		observedAt := time.Unix(1_788_122_692, 466_000_000)
		message := &agent.TextMessage{Text: "timed"}
		path := writePhysicalTestLog(t, false,
			`{"t":"caic_meta","version":3,"prompt":"task","harness":"claude"}`,
			`{"t":"agent","ts":1788122692.466,"msg":{"type":"assistant"}}`,
		)
		loaded, err := LoadHistorySource(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := loaded.LoadMessagesWithResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return []agent.Message{message}, nil }}, nil
		})); err != nil {
			t.Fatal(err)
		}

		if len(loaded.Timeline) != 1 || loaded.Timeline[0].Message != message || !loaded.Timeline[0].ProducerTime.Equal(observedAt) {
			t.Fatalf("timeline = %#v, want timed message at %v", loaded.Timeline, observedAt)
		}
	})

	t.Run("SemanticLoadAllowsEmptyProjection", func(t *testing.T) {
		t.Parallel()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV3),
			Prompt:      "empty native records",
			Harness:     string(harness.Codex),
		})
		native := `{"t":"agent","ts":1.000,"msg":{"kind":"empty"}}`
		path := writePhysicalTestLog(t, false, meta, native, native)
		loaded := &LoadedTask{path: path}
		err := loaded.LoadMessagesWithResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return nil, nil }}, nil
		}))

		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Timeline) != 0 {
			t.Fatalf("loaded timeline = %#v, want no semantic messages", loaded.Timeline)
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
			lt := &LoadedTask{Repos: []repo.Mount{{Name: "a/b", Branch: "caic-0"}}}
			if p := lt.Primary(); p == nil || p.Name != "a/b" {
				t.Errorf("Primary() = %+v, want a/b", p)
			}
		})
	})

	t.Run("LoadMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("AlreadyLoaded", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Timeline: []agent.TimedMessage{{Message: &agent.TextMessage{Text: "cached"}}}}
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Timeline) != 1 {
				t.Errorf("Msgs mutated when already loaded")
			}
		})
		t.Run("NoPath", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{}
			lt.SetWireResolver(newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
				return claudecode.New().NewWire(), nil
			}))

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
			_, err := loadSemanticTask("/does/not/exist.jsonl", nil)
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
		want task.State
	}{
		{"failed", task.StateFailed},
		{"crashed", task.StateCrashed},
		{"stopped", task.StateStopped},
		{"purged", task.StatePurged},
		{"unknown", task.StateFailed},
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
		Version:     int(agent.LogVersionV3),
		Prompt:      "export this",
		Harness:     string(harness.Claude),
	})
	for _, compressed := range []bool{false, true} {
		format := "plain"
		if compressed {
			format = "zstd"
		}
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, compressed, meta, nativeLogLine(`{"type":"text","text":"visible"}`))
			markdown, err := ExportDiscussion(path, newTestWireResolver(func(got harness.Name) (agent.WireFormat, error) {
				if got != harness.Claude {
					t.Fatalf("resolver harness = %q, want claude", got)
				}
				return &agenttest.Wire{Output: func(line []byte) ([]agent.Message, error) {
					var raw struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(line, &raw); err != nil {
						return nil, err
					}
					return []agent.Message{&agent.TextMessage{Text: raw.Text}}, nil
				}}, nil
			}))

			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(markdown, "**Prompt**: export this") || !strings.Contains(markdown, "## Assistant\n\nvisible") {
				t.Fatalf("exported discussion = %q", markdown)
			}
		})
	}
}
