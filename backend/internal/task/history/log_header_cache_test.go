// Tests versioned header-cache snapshots, exact disk shapes, and corrupt-cache recovery.

package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"

	"github.com/caic-xyz/caic/backend/internal/tokens"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// pinnedHeaderCacheFixture is a fully populated LoadedTask: every slice
// non-empty and every struct field set, so the marshaled form exercises the
// complete snapshot shape.
func pinnedHeaderCacheFixture() *LoadedTask {
	diffStat := v3.DiffStat{{Path: "file", LinesAdded: 1, LinesDeleted: 1, Binary: true}}
	return &LoadedTask{
		TaskID: "task",
		Prompt: "prompt",
		Title:  "title",
		Repos: []repo.Mount{{
			Name:          "repo",
			BaseBranch:    "main",
			Branch:        "caic-0",
			GitRoot:       "/git",
			ContainerPath: "/work",
		}},
		Harness:           harness.Claude,
		StartedAt:         time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC),
		LastActivityAt:    time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC),
		State:             task.StatePurged,
		ForgeIssue:        1,
		OwnerID:           "user-1",
		ForkedFromTaskID:  "fork",
		ParentTaskID:      "parent",
		CaicMCP:           true,
		ForgeOwner:        "owner",
		ForgeRepo:         "repo",
		ForgePR:           1,
		Tailscale:         true,
		USB:               true,
		Display:           true,
		Sudo:              true,
		GitHubToken:       true,
		RuntimeName:       "runtime",
		BaseImage:         "image",
		ContainerPlatform: "platform",
		MaxCPUs:           1,
		CacheMounts: []runtime.CacheMount{{
			Name:          "cache",
			Description:   "desc",
			HostPath:      "/host",
			ContainerPath: "/container",
			ReadOnly:      true,
			Shallow:       true,
		}},
		Mounts: []runtime.Mount{{
			HostPath:      "/host",
			ContainerPath: "/container",
			ReadOnly:      true,
		}},
		RequestedModel:        "model",
		RequestedEffort:       "effort",
		ReportedModel:         "reported-model",
		ReportedEffort:        "reported-effort",
		SessionID:             "session",
		AgentVersion:          "version",
		AutoCompactTokenLimit: 244800,
		LogSize:               1,
		DiffCreated:           true,
		LastTrailer: &task.Result{
			State:        task.StatePurged,
			DiffStat:     diffStat,
			CostUSD:      1,
			CostKnown:    true,
			CostComplete: true,
			Duration:     time.Second,
			NumTurns:     1,
			Usage:        agent.Usage{InputTokens: 1, OutputTokens: 1, ReasoningOutputTokens: 1},
			AgentResult:  "result",
			Err:          pinError{},
		},
	}
}

// pinError exercises the snapshot's error-text projection in the fixture.
type pinError struct{}

func (pinError) Error() string { return "pin" }

// pinnedHeaderTask pins the current persisted task snapshot independently of runtime types.
const pinnedHeaderTask = `{"task_id":"task","prompt":"prompt","title":"title","repos":[{"name":"repo","base_branch":"main","branch":"caic-0","git_root":"/git","container_path":"/work"}],"harness":"claude","started_at":"2026-01-01T00:00:00.123Z","last_activity_at":"2026-01-01T00:00:00.123Z","state":"purged","forge_issue":1,"owner_id":"user-1","forked_from_task_id":"fork","parent_task_id":"parent","caic_mcp":true,"forge_owner":"owner","forge_repo":"repo","forge_pr":1,"tailscale":true,"usb":true,"display":true,"sudo":true,"github_token":true,"runtime_name":"runtime","base_image":"image","container_platform":"platform","max_cpus":1,"cache_mounts":[{"name":"cache","description":"desc","host_path":"/host","container_path":"/container","read_only":true,"shallow":true}],"mounts":[{"host_path":"/host","container_path":"/container","read_only":true}],"model":"model","effort":"effort","reported_model":"reported-model","reported_effort":"reported-effort","session_id":"session","agent_version":"version","auto_compact_token_limit":244800,"log_size":1,"diff_created":true,"result":{"state":"purged","diff_stat":[{"path":"file","added":1,"deleted":1,"binary":true}],"cost_usd":1,"cost_known":true,"cost_complete":true,"duration":1000000000,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_output_tokens":1},"agent_result":"result","error":"pin"}}`

func TestHeaderCache(t *testing.T) {
	t.Parallel()
	t.Run("CostProvenance", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			result string
			known  bool
			cost   float64
		}{
			{"reported zero", `{"t":"result","state":"purged","cost_known":true}`, true, 0},
			{"unpriced DSH", `{"t":"result","state":"purged"}`, false, 0},
			{"reported positive", `{"t":"result","state":"purged","cost_usd":0.25}`, true, 0.25},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, false,
					`{"t":"caic_meta","version":3,"harness":"dsh","prompt":"test"}`, tc.result)
				cold, err := loadLogHeader(testLogger(), path, true, backends.Default("", nil))
				if err != nil {
					t.Fatal(err)
				}
				cached, ok := readHeaderCache(path)
				if !ok {
					t.Fatal("cache missed after cold scan")
				}
				for _, loaded := range []*LoadedTask{cold, cached} {
					if r := loaded.LastTrailer; r == nil || r.CostKnown != tc.known || r.CostUSD != tc.cost {
						t.Fatalf("cost provenance = %+v; want known=%v cost=%v", r, tc.known, tc.cost)
					}
				}
			})
		}
	})
	t.Run("MetadataCompletenessProof", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, native, trailer string
			complete              bool
		}{
			{"native zero", `{"type":"result","total_cost_usd":0}`, `{"t":"result","state":"purged"}`, true},
			{"native amount matches", `{"type":"result","total_cost_usd":0.25}`, `{"t":"result","state":"purged","cost_usd":0.25}`, true},
			{"native amount differs", `{"type":"result","total_cost_usd":0.5}`, `{"t":"result","state":"purged","cost_usd":0.25}`, false},
			{"no native report", `{"type":"result","usage":{"input_tokens":100}}`, `{"t":"result","state":"purged","cost_usd":0.25,"cost_known":true}`, false},
			{"terminal priced proof", `{"type":"result","usage":{"input_tokens":100}}`, `{"t":"result","state":"purged","cost_usd":0.25,"cost_known":true,"cost_complete":true}`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := writePhysicalTestLog(t, false, `{"t":"caic_meta","version":3,"harness":"claude","prompt":"proof"}`, `{"t":"agent","ts":1.000,"msg":`+tc.native+`}`, tc.trailer)
				cold, err := loadLogHeader(testLogger(), path, true, backends.Default("", nil))
				if err != nil {
					t.Fatal(err)
				}
				cached, ok := readHeaderCache(path)
				if !ok {
					t.Fatal("cache missed")
				}
				for _, loaded := range []*LoadedTask{cold, cached} {
					r := loaded.LastTrailer
					if r == nil || !r.CostKnown || r.CostComplete != tc.complete {
						t.Fatalf("proof lost/misclassified: %+v, want complete=%v", r, tc.complete)
					}
				}
			})
		}
	})

	t.Run("RebuildsSessionProjectionFromVersion9", func(t *testing.T) {
		t.Parallel()
		path := writePhysicalTestLog(t, false,
			`{"t":"caic_meta","version":3,"harness":"claude","prompt":"test","model":"requested"}`,
			`{"t":"session","session_id":"initial","model":"initial","reported_effort":"low"}`,
			`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","session_id":"native","model":"active","effort":"high"}}`,
			`{"t":"session","session_id":"restored","model":"stale","reported_effort":"low"}`,
			`{"t":"context_cleared"}`,
			`{"t":"result","state":"purged"}`)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"version":9,"log_size":%d,"log_mtime_unix_nano":%d,"task":{"session_id":"restored","reported_model":"stale","reported_effort":"low","state":"purged"}}`, info.Size(), info.ModTime().UnixNano())
		if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"rebuild", "cache"} {
			loaded, err := loadLogHeader(testLogger(), path, true, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			if loaded.SessionID != "" || loaded.ReportedModel != "active" || loaded.ReportedEffort != "high" || loaded.RequestedModel != "requested" {
				t.Fatalf("%s metadata = %+v, requested model = %q", source, loaded.SessionMetadata, loaded.RequestedModel)
			}
		}
		if _, ok := readHeaderCache(path); !ok {
			t.Fatal("reconstructed projection was not cached")
		}
	})
	t.Run("InvalidatesOlderSchemas", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, version := range []int{6, 7, 8, 9, 10, 11} {
			body := fmt.Sprintf(`{"version":%d,"log_size":1,"log_mtime_unix_nano":%d,"task":%s}`, version, info.ModTime().UnixNano(), pinnedHeaderTask)
			if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok := readHeaderCache(path); ok {
				t.Fatal("older cache must rebuild inventory from log")
			}
		}
	})

	t.Run("CurrentSnapshot", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"version":12,"log_size":1,"log_mtime_unix_nano":%d,"task":%s}`, info.ModTime().UnixNano(), pinnedHeaderTask)
		if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := readHeaderCache(path)
		if !ok {
			t.Fatal("header cache missed")
		}
		if got.path != path || got.LastTrailer.Err == nil || got.LastTrailer.Err.Error() != "pin" {
			t.Fatalf("restored snapshot = %+v", got)
		}
		if !bytes.Equal(inventoryJSON(t, got), []byte(pinnedHeaderTask)) {
			t.Fatalf("restored shape = %s", inventoryJSON(t, got))
		}
		if err := writeHeaderCache(path, pinnedHeaderCacheFixture()); err != nil {
			t.Fatal(err)
		}
		emitted, err := os.ReadFile(logHeaderCachePath(path))
		if err != nil {
			t.Fatal(err)
		}
		var want, actual any
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(emitted, &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("emitted cache = %s, want %s", emitted, body)
		}
	})
	t.Run("OptionalFieldsAndCollections", func(t *testing.T) {
		t.Parallel()
		fixture := pinnedHeaderCacheFixture()
		fixture.Repos = []repo.Mount{}
		fixture.CacheMounts = nil
		fixture.Mounts = []runtime.Mount{}
		fixture.LastTrailer.DiffStat = v3.DiffStat{{Path: "binary", OldSize: 42, NewSize: 84}}
		fixture.LastTrailer.DiskUsedBytes = new(int64(0))
		fixture.LastTrailer.Usage.CacheTTLSeconds = 300
		fixture.LastTrailer.Usage.CacheCreationInputTokens = 117
		fixture.LastTrailer.Usage.CacheWrites = tokens.CacheWrites{{TTL: 5 * time.Minute, Tokens: 10}, {TTL: time.Hour, Tokens: 100}, {TTL: 2 * time.Hour, Tokens: 7}}
		fixture.LastTrailer.StartupFailure = &v3.StartupFailure{Harness: "claude", Phase: "start", Cause: "broken"}
		raw := inventoryJSON(t, fixture)
		for _, fragment := range []string{`"repos":[]`, `"cache_mounts":null`, `"mounts":[]`, `"oldSize":42`, `"newSize":84`, `"disk_used_bytes":0`, `"cache_ttl_seconds":300`, `"cache_writes":[{"ttl_seconds":300,"tokens":10},{"ttl_seconds":3600,"tokens":100},{"ttl_seconds":7200,"tokens":7}]`, `"startup_failure":{"harness":"claude","phase":"start","cause":"broken"}`} {
			if !bytes.Contains(raw, []byte(fragment)) {
				t.Fatalf("shape missing %s: %s", fragment, raw)
			}
		}
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeHeaderCache(path, fixture); err != nil {
			t.Fatal(err)
		}
		got, ok := readHeaderCache(path)
		if !ok || !bytes.Equal(inventoryJSON(t, got), raw) {
			t.Fatal("optional and collection fields did not round-trip")
		}
	})
	t.Run("StateValidation", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, body string
			}{
				{"omitted", `{}`},
				{"result_omitted", `{"state":"purged","result":{}}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					path := filepath.Join(t.TempDir(), "task.jsonl")
					writeLogFile(t, filepath.Dir(path), filepath.Base(path), `{"t":"caic_meta","version":3,"harness":"claude","prompt":"rebuild","repos":[]}`, `{"t":"result","state":"purged"}`)
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					body := fmt.Sprintf(`{"version":12,"log_size":%d,"log_mtime_unix_nano":%d,"task":%s}`, info.Size(), info.ModTime().UnixNano(), tc.body)
					if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					if _, ok := readHeaderCache(path); !ok {
						t.Fatal("cache rejected valid header")
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, body string
			}{
				{"unknown", `{"state":"future"}`},
				{"empty", `{"state":""}`},
				{"null", `{"state":null}`},
				{"result_unknown", `{"state":"purged","result":{"state":"future"}}`},
				{"result_null", `{"state":"purged","result":{"state":null}}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					path := filepath.Join(t.TempDir(), "task.jsonl")
					writeLogFile(t, filepath.Dir(path), filepath.Base(path), `{"t":"caic_meta","version":3,"harness":"claude","prompt":"rebuild","repos":[]}`, `{"t":"result","state":"purged"}`)
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					body := fmt.Sprintf(`{"version":12,"log_size":%d,"log_mtime_unix_nano":%d,"task":%s}`, info.Size(), info.ModTime().UnixNano(), tc.body)
					if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					if _, ok := readHeaderCache(path); ok {
						t.Fatal("cache accepted invalid header")
					}
					got, err := loadLogHeader(testLogger(), path, true, backends.Default("", nil))
					if err != nil || got.Prompt != "rebuild" {
						t.Fatalf("rebuild = %+v, %v", got, err)
					}
					raw, err := os.ReadFile(logHeaderCachePath(path))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(raw), `"future"`) {
						t.Fatal("corrupt snapshot survived rebuild")
					}
				})
			}
		})
	})
}
