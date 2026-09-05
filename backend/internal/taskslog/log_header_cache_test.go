// Tests the task-log header cache and pins its marshaled shape.

package taskslog

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// pinnedHeaderCacheKeys is the exact set of JSON keys a marshaled header cache
// entry can contain, keyed by object path. The cache guards compatibility with
// a schema version, not a field set: a key added to LoadedTask or a type it
// embeds (RepoMount, Result, agent.DiffStat, agent.Usage, runtime mounts)
// without bumping headerCacheVersion would be silently zero-valued in entries
// written by older binaries — and for compressed terminal history those
// entries are never invalidated, so the stale read persists for the life of
// the log. This test pins the real marshaled schema of a fully populated
// LoadedTask (custom MarshalJSON included), so the key and the version bump
// must land together.
var pinnedHeaderCacheKeys = []string{
	"agent_version",
	"base_image",
	"cache_mounts",
	"cache_mounts[0].container_path",
	"cache_mounts[0].description",
	"cache_mounts[0].host_path",
	"cache_mounts[0].name",
	"cache_mounts[0].read_only",
	"cache_mounts[0].shallow",
	"container_platform",
	"diff_created",
	"display",
	"effort",
	"forge_issue",
	"forge_owner",
	"forge_pr",
	"forge_repo",
	"forked_from_task_id",
	"github_token",
	"harness",
	"last_state_update_at",
	"log_size",
	"log_version",
	"max_cpus",
	"model",
	"mounts",
	"mounts[0].container_path",
	"mounts[0].host_path",
	"mounts[0].read_only",
	"parent_task_id",
	"prompt",
	"repos",
	"repos[0].base_branch",
	"repos[0].branch",
	"repos[0].container_path",
	"repos[0].git_root",
	"repos[0].name",
	"result",
	"result.agent_result",
	"result.cost_usd",
	"result.diff_stat",
	"result.diff_stat[0].added",
	"result.diff_stat[0].binary",
	"result.diff_stat[0].deleted",
	"result.diff_stat[0].path",
	"result.duration",
	"result.error",
	"result.num_turns",
	"result.state",
	"result.usage",
	"result.usage.cache_creation_input_tokens",
	"result.usage.cache_read_input_tokens",
	"result.usage.input_tokens",
	"result.usage.output_tokens",
	"result.usage.reasoning_output_tokens",
	"runtime_name",
	"session_id",
	"started_at",
	"state",
	"sudo",
	"tailscale",
	"task_id",
	"title",
	"usb",
}

// pinnedHeaderCacheFixture is a fully populated LoadedTask: every slice
// non-empty and every struct field set, so the marshaled form exercises the
// complete key set pinned above.
func pinnedHeaderCacheFixture() *LoadedTask {
	diffStat := agent.DiffStat{{Path: "file", Added: 1, Deleted: 1, Binary: true}}
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
		StartedAt:         time.Now().UTC(),
		LastStateUpdateAt: time.Now().UTC(),
		State:             StatePurged,
		ForgeIssue:        1,
		ForkedFromTaskID:  "fork",
		ParentTaskID:      "parent",
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
		Model:        "model",
		Effort:       "effort",
		SessionID:    "session",
		AgentVersion: "version",
		LogSize:      1,
		DiffCreated:  true,
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

// pinError is a non-nil error so Result.MarshalJSON's error key is exercised by
// the pinned fixture.
type pinError struct{}

func (pinError) Error() string { return "pin" }

// collectJSONKeys returns the object keys that the JSON value at path
// contributes: for an object, each key as path+"."+k (or k at the root); for
// an array, the keys of its first element under path+"[0]"; for scalars and
// null, none.
func collectJSONKeys(raw json.RawMessage, path string) []string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		keys := make([]string, 0, len(obj))
		for k, v := range obj {
			full := k
			if path != "" {
				full = path + "." + k
			}
			keys = append(keys, full)
			keys = append(keys, collectJSONKeys(v, full)...)
		}
		return keys
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return collectJSONKeys(arr[0], path+"[0]")
	}
	return nil
}

func TestHeaderCachePinsLoadedTaskFields(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(pinnedHeaderCacheFixture())
	if err != nil {
		t.Fatal(err)
	}
	var root json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	keys := collectJSONKeys(root, "")
	slices.Sort(keys)
	if len(keys) != len(pinnedHeaderCacheKeys) {
		t.Fatalf("LoadedTask marshaled key set changed (got %d keys: %v); update pinnedHeaderCacheKeys and bump headerCacheVersion", len(keys), keys)
	}
	for i := range keys {
		if keys[i] != pinnedHeaderCacheKeys[i] {
			t.Fatalf("LoadedTask marshaled key set changed: got %v, want pinned %v; update pinnedHeaderCacheKeys and bump headerCacheVersion", keys, pinnedHeaderCacheKeys)
		}
	}
}
