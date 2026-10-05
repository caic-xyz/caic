// Tests historical v6 header-cache snapshots, exact disk shapes, and corrupt-cache recovery.

package taskslog

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

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// pinnedHeaderCacheFixture is a fully populated LoadedTask: every slice
// non-empty and every struct field set, so the marshaled form exercises the
// complete historical snapshot shape.
func pinnedHeaderCacheFixture() *LoadedTask {
	diffStat := agent.DiffStat{{Path: "file", LinesAdded: 1, LinesDeleted: 1, Binary: true}}
	return &LoadedTask{
		TaskID: "task",
		Prompt: "prompt",
		Title:  "title",
		Repos: []RepoMount{{
			Name:          "repo",
			BaseBranch:    "main",
			Branch:        "caic-0",
			GitRoot:       "/git",
			ContainerPath: "/work",
		}},
		LogVersion:        2,
		Harness:           harness.Claude,
		StartedAt:         time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC),
		LastStateUpdateAt: time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC),
		State:             StatePurged,
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
		RequestedModel:  "model",
		RequestedEffort: "effort",
		ReportedModel:   "reported-model",
		ReportedEffort:  "reported-effort",
		SessionID:       "session",
		AgentVersion:    "version",
		LogSize:         1,
		DiffCreated:     true,
		LastTrailer: &Result{
			State:       StatePurged,
			DiffStat:    diffStat,
			CostUSD:     1,
			Duration:    time.Second,
			NumTurns:    1,
			Usage:       agent.Usage{InputTokens: 1, OutputTokens: 1, ReasoningOutputTokens: 1},
			AgentResult: "result",
			Err:         pinError{},
		},
	}
}

// pinError exercises the v6 snapshot's error-text projection in the fixture.
type pinError struct{}

func (pinError) Error() string { return "pin" }

// historicalHeaderTask is the complete task shape written by v6 before data
// extraction. Keep this literal independent of data and runtime declarations.
const historicalHeaderTask = `{"task_id":"task","prompt":"prompt","title":"title","repos":[{"name":"repo","base_branch":"main","branch":"caic-0","git_root":"/git","container_path":"/work"}],"log_version":2,"harness":"claude","started_at":"2026-01-01T00:00:00.123Z","last_state_update_at":"2026-01-01T00:00:00.123Z","state":"purged","forge_issue":1,"owner_id":"user-1","forked_from_task_id":"fork","parent_task_id":"parent","caic_mcp":true,"forge_owner":"owner","forge_repo":"repo","forge_pr":1,"tailscale":true,"usb":true,"display":true,"sudo":true,"github_token":true,"runtime_name":"runtime","base_image":"image","container_platform":"platform","max_cpus":1,"cache_mounts":[{"name":"cache","description":"desc","host_path":"/host","container_path":"/container","read_only":true,"shallow":true}],"mounts":[{"host_path":"/host","container_path":"/container","read_only":true}],"model":"model","effort":"effort","reported_model":"reported-model","reported_effort":"reported-effort","session_id":"session","agent_version":"version","log_size":1,"diff_created":true,"result":{"state":"purged","diff_stat":[{"path":"file","added":1,"deleted":1,"binary":true}],"cost_usd":1,"duration":1000000000,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_output_tokens":1},"agent_result":"result","error":"pin"}}`

func TestHeaderCache(t *testing.T) {
	t.Parallel()
	t.Run("HistoricalSnapshot", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"version":6,"log_size":1,"log_mtime_unix_nano":%d,"task":%s}`, info.ModTime().UnixNano(), historicalHeaderTask)
		if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := readHeaderCache(path)
		if !ok {
			t.Fatal("historical v6 cache missed")
		}
		if got.path != path || got.LastTrailer.Err == nil || got.LastTrailer.Err.Error() != "pin" {
			t.Fatalf("restored snapshot = %+v", got)
		}
		if !bytes.Equal(inventoryJSON(t, got), []byte(historicalHeaderTask)) {
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
		fixture.Repos = []RepoMount{}
		fixture.CacheMounts = nil
		fixture.Mounts = []runtime.Mount{}
		fixture.LastTrailer.DiffStat = agent.DiffStat{{Path: "binary", OldSize: 42, NewSize: 84}}
		fixture.LastTrailer.DiskUsedBytes = new(int64(0))
		fixture.LastTrailer.Usage.CacheTTLSeconds = 300
		fixture.LastTrailer.StartupFailure = &agent.StartupFailure{Harness: "claude", Phase: "start", Cause: "broken"}
		raw := inventoryJSON(t, fixture)
		for _, fragment := range []string{`"repos":[]`, `"cache_mounts":null`, `"mounts":[]`, `"oldSize":42`, `"newSize":84`, `"disk_used_bytes":0`, `"cache_ttl_seconds":300`, `"startup_failure":{"harness":"claude","phase":"start","cause":"broken"}`} {
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
		for _, tc := range []struct {
			name, body string
			valid      bool
		}{
			{"omitted", `{}`, true},
			{"unknown", `{"state":"future"}`, false},
			{"empty", `{"state":""}`, false},
			{"null", `{"state":null}`, false},
			{"result_unknown", `{"state":"purged","result":{"state":"future"}}`, false},
			{"result_null", `{"state":"purged","result":{"state":null}}`, false},
			{"result_omitted", `{"state":"purged","result":{}}`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "task.jsonl")
				writeLogFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"caic_meta","version":1,"harness":"claude","prompt":"rebuild","repos":[]}`, `{"type":"caic_result","state":"purged"}`)
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"version":6,"log_size":%d,"log_mtime_unix_nano":%d,"task":%s}`, info.Size(), info.ModTime().UnixNano(), tc.body)
				if err := os.WriteFile(logHeaderCachePath(path), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, ok := readHeaderCache(path); ok != tc.valid {
					t.Fatalf("cache accepted = %t, want %t", ok, tc.valid)
				}
				if !tc.valid {
					got, err := loadLogHeader(testLogger(), path, true)
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
				}
			})
		}
	})
}
