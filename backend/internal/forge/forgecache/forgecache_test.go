// Tests historical CI cache compatibility, pruning, and notification deduplication.

package forgecache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestCache(t *testing.T) {
	t.Parallel()
	t.Run("historical load write reload", func(t *testing.T) {
		t.Parallel()
		// Zero timestamps were accepted by older files and remain exempt from TTL pruning.
		fixture := `{"results":{"owner/repo/sha":{"status":"failure","checks":[{"name":"build","owner":"owner","repo":"repo","runID":12,"jobID":34,"status":"completed","conclusion":"timed_out","labels":["linux"],"queuedAt":"2020-01-01T00:00:00.123456789Z","startedAt":"2020-01-01T00:01:00Z","completedAt":"2020-01-01T00:02:00Z"}],"cachedAt":"0001-01-01T00:00:00Z"},"owner/repo/old":{"status":"success","cachedAt":"2020-01-01T00:00:00Z"}},"notified":{"task/sha":"0001-01-01T00:00:00Z","task/old":"2020-01-01T00:00:00Z"}}`
		path := filepath.Join(t.TempDir(), "ci.json")
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		r, ok := c.Get("owner", "repo", "sha")
		if !ok || r.Status != forge.CIStatusFailure || len(r.Checks) != 1 {
			t.Fatalf("historical result: %#v, hit=%t", r, ok)
		}
		check := r.Checks[0]
		if check.Name != "build" || check.Owner != "owner" || check.Repo != "repo" || check.RunID != 12 || check.JobID != 34 || check.Status != forge.CheckRunStatusCompleted || check.Conclusion != forge.CheckRunConclusionTimedOut || !reflect.DeepEqual(check.Labels, []string{"linux"}) || check.QueuedAt.Nanosecond() != 123456789 || !check.StartedAt.Equal(time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC)) || !check.CompletedAt.Equal(time.Date(2020, 1, 1, 0, 2, 0, 0, time.UTC)) {
			t.Fatalf("historical check: %#v", check)
		}
		if _, ok := c.Get("owner", "repo", "old"); ok {
			t.Fatal("expired result retained")
		}
		if !c.IsNotified("task", "sha") || c.IsNotified("task", "old") {
			t.Fatal("notification pruning/dedup changed")
		}
		if err := c.Put("owner", "repo", "sha", r); err != nil {
			t.Fatal(err)
		}
		if err := c.MarkNotified("newtask", "sha"); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := reopened.Get("owner", "repo", "sha")
		if !ok || got.CachedAt.IsZero() || !reflect.DeepEqual(got.Checks, r.Checks) || !reopened.IsNotified("task", "sha") || !reopened.IsNotified("newtask", "sha") {
			t.Fatalf("reloaded result: %#v", got)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // Test-owned path inside t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		var disk map[string]json.RawMessage
		if err := json.Unmarshal(raw, &disk); err != nil {
			t.Fatal(err)
		}
		var before, after struct {
			Results map[string]struct {
				Checks []map[string]any `json:"checks"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(fixture), &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &after); err != nil {
			t.Fatal(err)
		}
		originalChecks := before.Results["owner/repo/sha"].Checks
		writtenChecks := after.Results["owner/repo/sha"].Checks
		if !reflect.DeepEqual(writtenChecks, originalChecks) {
			t.Fatalf("historical check JSON changed: %s", raw)
		}
		if len(disk) != 2 || disk["results"] == nil || disk["notified"] == nil {
			t.Fatalf("disk envelope: %s", raw)
		}
	})
}

func TestCacheRecovery(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]string{
		"partial decode": `{"results":{"owner/repo/sha":{"status":"success","cachedAt":"2099-01-01T00:00:00Z"}},"notified":{"task/sha":"2099-01-01T00:00:00Z","bad":42}}`,
		"null":           `null`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "ci.json")
			if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := c.Get("owner", "repo", "sha"); ok || c.IsNotified("task", "sha") {
				t.Fatal("retained partial file")
			}
			if err := c.Put("owner", "repo", "sha", Result{Status: forge.CIStatusSuccess}); err != nil {
				t.Fatal(err)
			}
			if err := c.MarkNotified("task", "sha"); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if r, ok := reopened.Get("owner", "repo", "sha"); !ok || r.Status != forge.CIStatusSuccess || !reopened.IsNotified("task", "sha") {
				t.Fatal("cache failed recovery")
			}
		})
	}
	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		if _, err := Open(t.TempDir()); err == nil {
			t.Fatal("directory read did not return error")
		}
	})
}
