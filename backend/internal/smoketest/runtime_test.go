// Tests for fake agent, runtime, and repository fixtures used by smoke and e2e tests.

package smoketest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestRuntimeBackendRepositoryStatus(t *testing.T) {
	t.Parallel()

	b := NewRuntimeBackend(0)
	id, err := b.Launch(t.Context(), []runtime.Repo{
		{GitRoot: "primary", Branch: "caic-1"},
		{GitRoot: "mapped", Branch: "caic-1"},
	}, &runtime.StartOptions{LogWriter: io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("primary repository has committed and uncommitted changes", func(t *testing.T) {
		t.Parallel()

		status, err := b.RepositoryStatus(t.Context(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if status.Ahead != 1 || status.Behind != 0 {
			t.Errorf("divergence = ahead %d behind %d, want ahead 1 behind 0", status.Ahead, status.Behind)
		}
		if !slices.Equal(status.DiffStat, []runtime.GitFileStat{
			{Path: "cmd/caic/main.go", LinesAdded: 8},
			{Path: "frontend/src/App.tsx", LinesAdded: 4, LinesDeleted: 2},
		}) {
			t.Errorf("DiffStat = %#v", status.DiffStat)
		}
		if len(status.Commits) != 1 || status.Commits[0].Subject != "Add task activity summary" {
			t.Errorf("Commits = %#v", status.Commits)
		}
		if !slices.Equal(status.Uncommitted, []runtime.GitFileStatus{{
			Path:           "frontend/src/App.tsx",
			WorktreeStatus: "M",
			LinesAdded:     4,
			LinesDeleted:   2,
		}}) {
			t.Errorf("Uncommitted = %#v", status.Uncommitted)
		}
	})

	t.Run("mapped repository has an independent worktree state", func(t *testing.T) {
		t.Parallel()

		status, err := b.RepositoryStatus(t.Context(), id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if status.Ahead != 0 || status.Behind != 1 {
			t.Errorf("divergence = ahead %d behind %d, want ahead 0 behind 1", status.Ahead, status.Behind)
		}
		if !slices.Equal(status.DiffStat, []runtime.GitFileStat{
			{Path: "internal/service/api.go", LinesAdded: 6, LinesDeleted: 1},
			{Path: "README.md", LinesAdded: 3, LinesDeleted: 4},
		}) {
			t.Errorf("DiffStat = %#v", status.DiffStat)
		}
		if len(status.Commits) != 0 {
			t.Errorf("Commits = %#v, want none", status.Commits)
		}
	})
}

func TestRuntimeBackendFileDiff(t *testing.T) {
	t.Parallel()

	b := NewRuntimeBackend(0)
	tests := []struct {
		name    string
		repoIdx int
		commit  string
		path    string
		want    string
		numstat string
	}{
		{
			name:    "committed primary file",
			repoIdx: 0,
			commit:  "7b14c36e1f5a0d2c9e8f4b6a3c1d0e9f8a7b6c5d",
			path:    "cmd/caic/main.go",
			want:    "+\tstatus := task.RepositoryStatus()",
			numstat: "8\t0\tcmd/caic/main.go\n",
		},
		{
			name:    "uncommitted primary file",
			repoIdx: 0,
			path:    "frontend/src/App.tsx",
			want:    "+  <span>Repository changes</span>",
			numstat: "4\t2\tfrontend/src/App.tsx\n",
		},
		{
			name:    "uncommitted mapped Go file",
			repoIdx: 1,
			path:    "internal/service/api.go",
			want:    "+\tstate := repositoryState()",
			numstat: "6\t1\tinternal/service/api.go\n",
		},
		{
			name:    "uncommitted mapped documentation",
			repoIdx: 1,
			path:    "README.md",
			want:    "+Open Repository changes.",
			numstat: "3\t4\tREADME.md\n",
		},
		{
			name: "unknown file",
			path: "unknown.txt",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := b.FileDiff(t.Context(), "", test.repoIdx, test.commit, test.path, "")
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" && got != "" {
				t.Errorf("FileDiff() = %q, want empty content", got)
			}
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Errorf("FileDiff() = %q, want content containing %q", got, test.want)
			}
			if got == "" {
				return
			}
			patch := filepath.Join(t.TempDir(), "fixture.patch")
			if err := os.WriteFile(patch, []byte(got), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "git", "apply", "--numstat", patch) //nolint:gosec // patch is an owned test file.
			cmd.Dir = filepath.Join("..", "..", "..")
			numstat, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git apply --numstat: %v: %s", err, numstat)
			}
			if string(numstat) != test.numstat {
				t.Errorf("git apply --numstat = %q, want %q", numstat, test.numstat)
			}
		})
	}
}

func TestFakeAgentNaturalPromptMatching(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name:   "explicit lifecycle scenario",
			prompt: "FAKE_LIFECYCLE e2e lifecycle 6487f1ff-2b95-435e-b2c0-27cafedadd0d",
			want:   "Lifecycle streaming marker",
		},
		{
			name:   "keyword substring in identifier",
			prompt: "e2e lifecycle 6487f1ff-2b95-435e-b2c0-27cafedadd0d",
			want:   "Why do programmers prefer dark mode?",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "python3", "-u", "-c", string(fakeScript)) //nolint:gosec // fakeScript is an embedded constant
			cmd.Stdin = strings.NewReader(tc.prompt + "\n")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("fake scenario output does not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestFakeAgentDemoIncludesCredibleTiming(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "python3", "-u", "-c", string(fakeScript)) //nolint:gosec // fakeScript is an embedded constant
	cmd.Stdin = strings.NewReader("FAKE_DEMO timing\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"tool_result","tool_use_id":"toolu_read_1","duration_ms":180`,
		`"type":"tool_result","tool_use_id":"toolu_bash_1","duration_ms":620`,
		`"type":"result","subtype":"success"`,
		`"duration_ms":2200`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("fake demo output does not contain %q:\n%s", want, text)
		}
	}
}

func TestFakeAgentTimingMessagesParse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
		check func(*testing.T, agent.Message)
	}{
		{
			name:  "thinking",
			input: `{"type":"thinking","text":"checking timing"}`,
			check: func(t *testing.T, msg agent.Message) {
				t.Helper()
				thinking, ok := msg.(*agent.ThinkingMessage)
				if !ok || thinking.Text != "checking timing" {
					t.Fatalf("message = %#v, want thinking message", msg)
				}
			},
		},
		{
			name:  "tool result duration",
			input: `{"type":"tool_result","tool_use_id":"tool-1","duration_ms":620}`,
			check: func(t *testing.T, msg agent.Message) {
				t.Helper()
				result, ok := msg.(*agent.ToolResultMessage)
				if !ok || result.DurationMs != 620 {
					t.Fatalf("message = %#v, want 620ms tool result", msg)
				}
			},
		},
		{
			name:  "rate limit",
			input: `{"type":"rate_limit","status":"rejected","resets_at":"2026-09-11T13:00:00Z","rate_limit_type":"five_hour","utilization":1,"quota_provider":"claudecode","quota_label":"Claude Code","quota_window":"5h"}`,
			check: func(t *testing.T, msg agent.Message) {
				rateLimit, ok := msg.(*agent.RateLimitMessage)
				if !ok || rateLimit.Status != agent.RateLimitStatusRejected || rateLimit.QuotaWindow != "5h" {
					t.Fatalf("message = %#v, want rejected 5h rate limit", msg)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msgs, err := parseMessage([]byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("message count = %d, want 1", len(msgs))
			}
			tc.check(t, msgs[0])
		})
	}
}

func TestCleanupSmokeRunContainers(t *testing.T) {
	t.Parallel()

	t.Run("removes_only_containers_returned_by_run_label_filter", func(t *testing.T) {
		t.Parallel()
		var calls [][]string
		run := func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(calls) == 1 {
				return []byte("owned-1\nowned-2\n"), nil
			}
			return nil, nil
		}
		if err := cleanupSmokeRunContainers(t.Context(), "podman", "unique-run", run); err != nil {
			t.Fatal(err)
		}
		want := [][]string{
			{"podman", "container", "ls", "-aq", "--filter", "label=caic.smoke_run=unique-run"},
			{"podman", "container", "rm", "-f", "owned-1", "owned-2"},
		}
		if len(calls) != len(want) {
			t.Fatalf("command count = %d, want %d: %q", len(calls), len(want), calls)
		}
		for i := range calls {
			if !slices.Equal(calls[i], want[i]) {
				t.Fatalf("command %d = %q, want %q", i, calls[i], want[i])
			}
		}
	})
	t.Run("empty_result_does_not_remove", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, nil
		}
		if err := cleanupSmokeRunContainers(t.Context(), "podman", "unique-run", run); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("commands = %d, want only list", calls)
		}
	})
	t.Run("rejects_missing_ownership_boundary", func(t *testing.T) {
		t.Parallel()
		err := cleanupSmokeRunContainers(t.Context(), "", "", func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("must not run")
		})
		if err == nil {
			t.Fatal("missing runtime and run token were accepted")
		}
	})
}

func TestInitHarnessCache(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	if err := InitHarnessCache(cacheDir); err != nil {
		t.Fatalf("InitHarnessCache: %v", err)
	}

	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		inventory, fresh := cache.ModelInventory(h, "")
		if !fresh {
			t.Errorf("ModelInventory(%q) = %#v fresh=%t, want fresh inventory", h, inventory, fresh)
		}
		if !slices.Equal(inventory.IDs(), []string{"fake-model"}) {
			t.Errorf("ModelInventory(%q).IDs() = %v, want [fake-model]", h, inventory.IDs())
		}
	}
}
