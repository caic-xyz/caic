// Tests for parsing machine-readable git branch, commit, and working-tree status output.

package mdruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestParseGitStatus(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		out := strings.Join([]string{
			"# branch.oid 0123456789abcdef",
			"# branch.head caic-42",
			"# branch.upstream origin/main",
			"# branch.ab +2 -1",
			"1 M. N... 100644 100644 100644 abc def src/staged.go",
			"1 .M N... 100644 100644 100644 abc def src/working.go",
			"2 R. N... 100644 100644 100644 abc def R100 src/new name.go",
			"src/old name.go",
			"? notes/new.txt",
			gitLogMarker,
			"",
			gitCommitMarker,
			"1111111111111111111111111111111111111111",
			"Add status summary",
			"",
			"\n12\t0\tsrc/status.go",
			"-\t-\tassets/logo.png",
			gitCommitMarker,
			"2222222222222222222222222222222222222222",
			"Polish the UI",
			"",
			"\n2\t2\tfrontend/view.tsx",
			"1\t1\t",
			"frontend/old.tsx",
			"frontend/new.tsx",
		}, "\x00")

		got, err := parseGitStatus(out)
		if err != nil {
			t.Fatal(err)
		}
		want := runtime.RepositoryStatus{
			Branch:   "caic-42",
			Upstream: "origin/main",
			Ahead:    2,
			Behind:   1,
			Commits: []runtime.GitCommit{
				{
					SHA:     "1111111111111111111111111111111111111111",
					Subject: "Add status summary",
					Stat: []runtime.GitFileStat{
						{Path: "src/status.go", Added: 12},
						{Path: "assets/logo.png", Binary: true},
					},
				},
				{
					SHA:     "2222222222222222222222222222222222222222",
					Subject: "Polish the UI",
					Stat: []runtime.GitFileStat{
						{Path: "frontend/view.tsx", Added: 2, Deleted: 2},
						{Path: "frontend/new.tsx", Added: 1, Deleted: 1},
					},
				},
			},
			Uncommitted: []runtime.GitFileStatus{
				{Path: "src/staged.go", IndexStatus: "M"},
				{Path: "src/working.go", WorktreeStatus: "M"},
				{Path: "src/new name.go", OriginalPath: "src/old name.go", IndexStatus: "R"},
				{Path: "notes/new.txt", IndexStatus: "?", WorktreeStatus: "?"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseGitStatus() = %#v, want %#v", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for name, out := range map[string]string{
			"missing marker":    "# branch.head main\x00",
			"bad divergence":    "# branch.ab ahead\x00" + gitLogMarker + "\x00",
			"bad ordinary":      "1 M. short\x00" + gitLogMarker + "\x00",
			"bad rename":        "2 R. short\x00" + gitLogMarker + "\x00",
			"bad unmerged":      "u UU short\x00" + gitLogMarker + "\x00",
			"unknown record":    "x surprise\x00" + gitLogMarker + "\x00",
			"incomplete commit": gitLogMarker + "\x00" + gitCommitMarker + "\x00sha",
			"bad numstat":       gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x00subject\x00words",
			"bad rename stat":   gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x00subject\x001\t1\t",
			"bad additions":     gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x00subject\x00many\t1\tfile",
			"bad deletions":     gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x00subject\x001\tmany\tfile",
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				if _, err := parseGitStatus(out); err == nil {
					t.Fatal("parseGitStatus() succeeded, want error")
				}
			})
		}
	})
}

func TestGitStatusCommand(t *testing.T) {
	t.Parallel()
	t.Run("quotes repository and includes protocol", func(t *testing.T) {
		t.Parallel()
		cmd := gitStatusCommand("/work/repo's copy")
		if !strings.HasPrefix(cmd, `cd '/work/repo'"'"'s copy'`) {
			t.Errorf("gitStatusCommand() does not safely quote repo: %q", cmd)
		}
		for _, fragment := range []string{"git status --porcelain=v2", "@{upstream}", "$upstream..HEAD", "--numstat -z", gitLogMarker, gitCommitMarker} {
			if !strings.Contains(cmd, fragment) {
				t.Errorf("gitStatusCommand() missing %q", fragment)
			}
		}
	})

	t.Run("runs against repository", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runTestGit(t, dir, "init", "-b", "main")
		runTestGit(t, dir, "config", "user.email", "caic@example.com")
		runTestGit(t, dir, "config", "user.name", "caic test")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "tracked.txt")
		runTestGit(t, dir, "commit", "-m", "base")
		runTestGit(t, dir, "branch", "upstream")
		runTestGit(t, dir, "branch", "--set-upstream-to=upstream", "main")
		if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "committed.txt")
		runTestGit(t, dir, "commit", "-m", "one ahead")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "sh", "-c", gitStatusCommand(dir)).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Branch != "main" || status.Upstream != "upstream" || status.Ahead != 1 || status.Behind != 0 {
			t.Errorf("branch status = %+v", status)
		}
		if len(status.Commits) != 1 || status.Commits[0].Subject != "one ahead" || len(status.Commits[0].Stat) != 1 || status.Commits[0].Stat[0] != (runtime.GitFileStat{Path: "committed.txt", Added: 1}) {
			t.Errorf("commits = %+v", status.Commits)
		}
		if len(status.Uncommitted) != 2 {
			t.Errorf("uncommitted = %+v", status.Uncommitted)
		}
	})
}

func runTestGit(t *testing.T, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // arguments are hardcoded test fixtures.
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
