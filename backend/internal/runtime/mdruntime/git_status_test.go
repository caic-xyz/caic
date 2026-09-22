// Tests for parsing machine-readable git branch, commit, and working-tree status output.

package mdruntime

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
			"# branch.upstream host/caic-42",
			"# branch.ab +0 -0",
			"1 M. N... 100644 100644 100644 abc def src/staged.go",
			"1 .M N... 100644 100644 100644 abc def src/working.go",
			"2 R. N... 100644 100644 100644 abc def R100 src/new name.go",
			"src/old name.go",
			"? notes/new.txt",
			"1 .M N... 100644 100644 100644 abc def assets/photo.jpg",
			gitComparisonMarker + "origin/main",
			gitDivergenceMarker + "1\t2",
			gitOperationMarker,
			"rebase",
			gitTotalStatMarker,
			"14\t1\tsrc/status.go",
			"-\t-\tassets/logo.png",
			"3\t1\tfrontend/new.tsx",
			" src/status.go       | 14 +-\n assets/logo.png     | Bin 100 -> 250 bytes\n frontend/new.tsx    |  3 +-\n 3 files changed\n",
			gitWorktreeStatMarker,
			"2\t0\tsrc/staged.go",
			"1\t1\tsrc/working.go",
			"3\t0\tnotes/new.txt",
			"-\t-\tassets/photo.jpg",
			" src/staged.go       | 2 ++\n src/working.go      | 1 +-\n notes/new.txt       | 3 +++\n assets/photo.jpg    | Bin 400 -> 500 bytes\n 4 files changed\n",
			gitLogMarker,
			"",
			gitCommitMarker,
			"1111111111111111111111111111111111111111",
			"2026-08-30",
			"tag: v1.2.3",
			"Add status summary",
			"",
			"\n12\t0\tsrc/status.go",
			"-\t-\tassets/logo.png",
			" src/status.go       | 12 +\n assets/logo.png     | Bin 100 -> 250 bytes\n 2 files changed\n",
			gitCommitMarker,
			"2222222222222222222222222222222222222222",
			"2026-09-01",
			"HEAD -> caic-42",
			"Polish the UI",
			"",
			"\n2\t2\tfrontend/view.tsx",
			"1\t1\t",
			"frontend/old.tsx",
			"frontend/new.tsx",
			" frontend/view.tsx              | 2 +-\n frontend/old.tsx => new.tsx    | 1 +-\n 2 files changed\n",
		}, "\x00")

		got, err := parseGitStatus(out)
		if err != nil {
			t.Fatal(err)
		}
		want := runtime.RepositoryStatus{
			Branch:    "caic-42",
			Upstream:  "origin/main",
			Operation: runtime.RepositoryOperationRebase,
			Ahead:     2,
			Behind:    1,
			DiffStat: []runtime.GitFileStat{
				{Path: "src/status.go", LinesAdded: 14, LinesDeleted: 1},
				{Path: "assets/logo.png", Binary: true, OldSize: 100, NewSize: 250},
				{Path: "frontend/new.tsx", LinesAdded: 3, LinesDeleted: 1},
			},
			Commits: []runtime.GitCommit{
				{
					SHA:          "1111111111111111111111111111111111111111",
					Subject:      "Add status summary",
					Decorations:  "tag: v1.2.3",
					AuthoredDate: "2026-08-30",
					Stat: []runtime.GitFileStat{
						{Path: "src/status.go", LinesAdded: 12},
						{Path: "assets/logo.png", Binary: true, OldSize: 100, NewSize: 250},
					},
				},
				{
					SHA:          "2222222222222222222222222222222222222222",
					Subject:      "Polish the UI",
					Decorations:  "HEAD -> caic-42",
					AuthoredDate: "2026-09-01",
					Stat: []runtime.GitFileStat{
						{Path: "frontend/view.tsx", LinesAdded: 2, LinesDeleted: 2},
						{Path: "frontend/new.tsx", LinesAdded: 1, LinesDeleted: 1},
					},
				},
			},
			Uncommitted: []runtime.GitFileStatus{
				{Path: "src/staged.go", IndexStatus: "M", LinesAdded: 2},
				{Path: "src/working.go", WorktreeStatus: "M", LinesAdded: 1, LinesDeleted: 1},
				{Path: "src/new name.go", OriginalPath: "src/old name.go", IndexStatus: "R"},
				{Path: "notes/new.txt", IndexStatus: "?", WorktreeStatus: "?", LinesAdded: 3},
				{Path: "assets/photo.jpg", WorktreeStatus: "M", Binary: true, OldSize: 400, NewSize: 500},
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
			"bad comparison":    gitDivergenceMarker + "ahead\x00" + gitLogMarker + "\x00",
			"bad worktree stat": gitWorktreeStatMarker + "\x00words\x00" + gitLogMarker + "\x00",
			"bad ordinary":      "1 M. short\x00" + gitLogMarker + "\x00",
			"bad rename":        "2 R. short\x00" + gitLogMarker + "\x00",
			"bad unmerged":      "u UU short\x00" + gitLogMarker + "\x00",
			"unknown record":    "x surprise\x00" + gitLogMarker + "\x00",
			"incomplete commit": gitLogMarker + "\x00" + gitCommitMarker + "\x00sha",
			"bad numstat":       gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x002026-09-01\x00\x00subject\x00words",
			"bad rename stat":   gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x002026-09-01\x00\x00subject\x001\t1\t",
			"bad additions":     gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x002026-09-01\x00\x00subject\x00many\t1\tfile",
			"bad deletions":     gitLogMarker + "\x00" + gitCommitMarker + "\x00sha\x002026-09-01\x00\x00subject\x001\tmany\tfile",
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

func TestParseCompactGitStatus(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		out := strings.Join([]string{
			"# branch.oid 0123456789abcdef",
			"# branch.head caic-42",
			"1 .M N... 100644 100644 100644 abc def src/working.go",
			"2 R. N... 100644 100644 100644 abc def R100 src/new name.go",
			"src/old name.go",
			"? notes/new.txt",
			gitComparisonMarker + "origin/main",
			gitDivergenceMarker + "1\t2",
			gitOperationMarker,
			"rebase",
			gitTotalStatMarker,
			"14\t1\tsrc/status.go",
			"-\t-\tassets/logo.png",
			"1\t1\t",
			"src/old.tsx",
			"src/new.tsx",
			" src/status.go       | 14 +-\n assets/logo.png     | Bin 100 -> 250 bytes\n src/old.tsx => src/new.tsx    | 1 +-\n 3 files changed\n",
			gitWorktreeStatMarker,
			"anything after the terminal marker must be ignored",
		}, "\x00")

		got, err := parseCompactGitStatus(out)
		if err != nil {
			t.Fatal(err)
		}
		want := runtime.RepositoryStatus{
			Branch:    "caic-42",
			Upstream:  "origin/main",
			Operation: runtime.RepositoryOperationRebase,
			Ahead:     2,
			Behind:    1,
			DiffStat: []runtime.GitFileStat{
				{Path: "src/status.go", LinesAdded: 14, LinesDeleted: 1},
				{Path: "assets/logo.png", Binary: true, OldSize: 100, NewSize: 250},
				{Path: "src/new.tsx", LinesAdded: 1, LinesDeleted: 1},
			},
			Uncommitted: []runtime.GitFileStatus{
				{Path: "src/working.go", WorktreeStatus: "M"},
				{Path: "src/new name.go", OriginalPath: "src/old name.go", IndexStatus: "R"},
				{Path: "notes/new.txt", IndexStatus: "?", WorktreeStatus: "?"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseCompactGitStatus() = %#v, want %#v", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for name, out := range map[string]string{
			"bad comparison": gitDivergenceMarker + "ahead\x00" + gitTotalStatMarker + "\x00" + gitWorktreeStatMarker + "\x00",
			"bad total stat": gitTotalStatMarker + "\x00words\x00" + gitWorktreeStatMarker + "\x00",
			"bad additions":  gitTotalStatMarker + "\x00many\t1\tfile\x00" + gitWorktreeStatMarker + "\x00",
			"bad deletions":  gitTotalStatMarker + "\x001\tmany\tfile\x00" + gitWorktreeStatMarker + "\x00",
			"bad ordinary":   "1 M. short\x00" + gitWorktreeStatMarker + "\x00",
			"bad rename":     "2 R. short\x00" + gitWorktreeStatMarker + "\x00",
			"bad unmerged":   "u UU short\x00" + gitWorktreeStatMarker + "\x00",
			"bad operation":  gitOperationMarker + "\x00bogus\x00" + gitWorktreeStatMarker + "\x00",
			"unknown record": "x surprise\x00" + gitWorktreeStatMarker + "\x00",
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				if _, err := parseCompactGitStatus(out); err == nil {
					t.Fatal("parseCompactGitStatus() succeeded, want error")
				}
			})
		}
	})
}

func TestCompactGitStatusCommand(t *testing.T) {
	t.Parallel()
	t.Run("quotes repository and omits the history walk", func(t *testing.T) {
		t.Parallel()
		cmd := compactGitStatusCommand("/work/repo's copy", "upstream", "trunk")
		if !strings.HasPrefix(cmd, `cd '/work/repo'"'"'s copy'`) {
			t.Errorf("compactGitStatusCommand() does not safely quote repo: %q", cmd)
		}
		for _, fragment := range []string{"git status --porcelain=v2", "@{upstream}", "upstream/trunk", `git diff "$comparison" --numstat --stat -z`, "GIT_OPTIONAL_LOCKS=0", gitComparisonMarker, gitDivergenceMarker, gitOperationMarker, gitTotalStatMarker, gitWorktreeStatMarker} {
			if !strings.Contains(cmd, fragment) {
				t.Errorf("compactGitStatusCommand() missing %q", fragment)
			}
		}
		for _, fragment := range []string{gitLogMarker, gitCommitMarker, "$comparison..HEAD", "git diff HEAD"} {
			if strings.Contains(cmd, fragment) {
				t.Errorf("compactGitStatusCommand() includes history-walk fragment %q", fragment)
			}
		}
	})

	t.Run("runs against repository", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		runTestGit(t, dir, "checkout", "-b", "caic-42")
		if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "committed.txt")
		runTestGit(t, dir, "commit", "-m", "one ahead")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "staged.txt")
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", "-c", compactGitStatusCommand(dir, "origin", "main")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseCompactGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Branch != "caic-42" || status.Upstream != "origin/main" || status.Ahead != 1 || status.Behind != 0 {
			t.Errorf("branch status = %+v", status)
		}
		if len(status.Commits) != 0 {
			t.Errorf("compact status walked commits = %+v", status.Commits)
		}
		if len(status.Uncommitted) != 3 {
			t.Errorf("uncommitted = %+v", status.Uncommitted)
		}
		wantDiffStat := []runtime.GitFileStat{
			{Path: "committed.txt", LinesAdded: 1},
			{Path: "staged.txt", LinesAdded: 1},
			{Path: "tracked.txt", LinesAdded: 1, LinesDeleted: 1},
			{Path: "untracked.txt", LinesAdded: 1},
		}
		if !reflect.DeepEqual(status.DiffStat, wantDiffStat) {
			t.Errorf("diff stat = %+v, want %+v", status.DiffStat, wantDiffStat)
		}
		if cached := runTestGitOutput(t, dir, "diff", "--cached", "--name-only"); cached != "staged.txt" {
			t.Errorf("cached diff after status = %q, want staged.txt", cached)
		}
	})

	t.Run("uses tracking upstream instead of configured base", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		runTestGit(t, dir, "checkout", "-b", "usage-base")
		if err := os.WriteFile(filepath.Join(dir, "usage.txt"), []byte("usage base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "usage.txt")
		runTestGit(t, dir, "commit", "-m", "usage base")
		runTestGit(t, dir, "update-ref", "refs/remotes/origin/usage", "HEAD")
		runTestGit(t, dir, "checkout", "-b", "caic-42", "origin/main")
		runTestGit(t, dir, "branch", "--set-upstream-to=origin/main", "caic-42")
		if err := os.WriteFile(filepath.Join(dir, "task.txt"), []byte("task change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "task.txt")
		runTestGit(t, dir, "commit", "-m", "task change")

		out, err := exec.CommandContext(t.Context(), "bash", "-c", compactGitStatusCommand(dir, "origin", "usage")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseCompactGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Upstream != "origin/main" || status.Ahead != 1 || status.Behind != 0 {
			t.Errorf("branch status = %+v, want origin/main +1 -0", status)
		}
	})

	t.Run("unresolvable comparison reports counts without a branch stat", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		runTestGit(t, dir, "checkout", "-b", "caic-42")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", "-c", compactGitStatusCommand(dir, "missing", "branch")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseCompactGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		// caic-42 has no upstream and missing/branch does not resolve, so the
		// probe degrades to uncommitted counts instead of failing the whole
		// report and blanking the task card.
		if status.Branch != "caic-42" || status.Upstream != "" || status.Ahead != 0 || status.Behind != 0 {
			t.Errorf("branch status = %+v", status)
		}
		if len(status.DiffStat) != 0 {
			t.Errorf("diff stat = %+v, want none", status.DiffStat)
		}
		if len(status.Uncommitted) != 1 {
			t.Errorf("uncommitted = %+v", status.Uncommitted)
		}
	})

	t.Run("reports an in-progress merge", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		runTestGit(t, dir, "checkout", "-b", "feature")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("feature\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "commit", "-am", "feature change")
		runTestGit(t, dir, "checkout", "main")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "commit", "-am", "main change")
		runTestGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runTestGit(t, dir, "checkout", "feature")
		if err := exec.CommandContext(t.Context(), "git", "-C", dir, "merge", "main").Run(); err == nil { //nolint:gosec // temporary repository is test-owned.
			t.Fatal("expected the merge to conflict")
		}

		out, err := exec.CommandContext(t.Context(), "bash", "-c", compactGitStatusCommand(dir, "origin", "main")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseCompactGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Operation != runtime.RepositoryOperationMerge {
			t.Errorf("operation = %q, want merge", status.Operation)
		}
		if status.Ahead != 1 || status.Behind != 1 {
			t.Errorf("divergence = +%d -%d, want +1 -1", status.Ahead, status.Behind)
		}
		conflicts := 0
		for _, file := range status.Uncommitted {
			if file.IndexStatus == "U" || file.WorktreeStatus == "U" {
				conflicts++
			}
		}
		if conflicts == 0 {
			t.Errorf("uncommitted = %+v, want a conflicted file", status.Uncommitted)
		}
	})

	t.Run("unborn branch", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runTestGit(t, dir, "init", "-b", "main")
		runTestGit(t, dir, "config", "user.email", "caic@example.com")
		runTestGit(t, dir, "config", "user.name", "caic test")
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "staged.txt")

		out, err := exec.CommandContext(t.Context(), "bash", "-c", compactGitStatusCommand(dir, "origin", "main")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseCompactGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Branch != "main" || status.Upstream != "" || len(status.DiffStat) != 0 || len(status.Uncommitted) != 1 {
			t.Errorf("unborn branch status = %+v", status)
		}
	})
}

func TestGitStatusCommand(t *testing.T) {
	t.Parallel()
	t.Run("quotes repository and includes protocol", func(t *testing.T) {
		t.Parallel()
		cmd := gitStatusCommand("/work/repo's copy", "upstream", "trunk")
		if !strings.HasPrefix(cmd, `cd '/work/repo'"'"'s copy'`) {
			t.Errorf("gitStatusCommand() does not safely quote repo: %q", cmd)
		}
		for _, fragment := range []string{"git status --porcelain=v2", "@{upstream}", "upstream/trunk", "$comparison..HEAD", "--left-right", "--date-order", "--decorate=short", "%as", "%D", "GIT_OPTIONAL_LOCKS=0", "git add -N", `git diff "$comparison" --numstat --stat -z`, "git diff HEAD --numstat --stat -z", gitComparisonMarker, gitDivergenceMarker, gitOperationMarker, gitTotalStatMarker, gitWorktreeStatMarker, gitLogMarker, gitCommitMarker} {
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
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "tracked.txt")
		runTestGit(t, dir, "commit", "-m", "base")
		runTestGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "committed.txt")
		runTestGit(t, dir, "commit", "-m", "one ahead")
		runTestGit(t, dir, "remote", "add", "host", ".")
		runTestGit(t, dir, "update-ref", "refs/remotes/host/caic-42", "HEAD")
		runTestGit(t, dir, "branch", "--set-upstream-to=host/caic-42", "main")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "staged.txt")
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", "-c", gitStatusCommand(dir, "origin", "main")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err := parseGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Branch != "main" || status.Upstream != "host/caic-42" || status.Ahead != 0 || status.Behind != 0 {
			t.Errorf("branch status = %+v", status)
		}
		if len(status.Commits) != 0 {
			t.Errorf("commits = %+v", status.Commits)
		}
		if len(status.Uncommitted) != 3 {
			t.Errorf("uncommitted = %+v", status.Uncommitted)
		}
		if status.Uncommitted[0].LinesAdded != 1 || status.Uncommitted[0].LinesDeleted != 0 || status.Uncommitted[1].LinesAdded != 1 || status.Uncommitted[1].LinesDeleted != 1 || status.Uncommitted[2].LinesAdded != 1 {
			t.Errorf("uncommitted stats = %+v", status.Uncommitted)
		}
		wantDiffStat := []runtime.GitFileStat{
			{Path: "staged.txt", LinesAdded: 1},
			{Path: "tracked.txt", LinesAdded: 1, LinesDeleted: 1},
			{Path: "untracked.txt", LinesAdded: 1},
		}
		if !reflect.DeepEqual(status.DiffStat, wantDiffStat) {
			t.Errorf("diff stat = %+v, want %+v", status.DiffStat, wantDiffStat)
		}
		if cached := runTestGitOutput(t, dir, "diff", "--cached", "--name-only"); cached != "staged.txt" {
			t.Errorf("cached diff after status = %q, want staged.txt", cached)
		}

		out, err = exec.CommandContext(t.Context(), "bash", "-c", gitStatusCommand(dir, "missing", "branch")).Output() //nolint:gosec // command and temporary repository are test-owned.
		if err != nil {
			t.Fatal(err)
		}
		status, err = parseGitStatus(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if status.Upstream != "host/caic-42" || status.Ahead != 0 || status.Behind != 0 || len(status.Commits) != 0 {
			t.Errorf("fallback branch status = %+v", status)
		}
	})

	t.Run("valid recovers when an untracked file vanishes during diff", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		vanished := filepath.Join(dir, "vanished.txt")
		if err := os.WriteFile(vanished, []byte("vanished\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "retained.txt"), []byte("retained\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		shim := gitShim(t, `if [ "$1" = diff ] && [ ! -e "$MD_TEST_MARKER" ]; then
	: > "$MD_TEST_MARKER"
	rm -f "$MD_TEST_VANISH"
	printf 'fatal: stat %s: No such file or directory\n' "$MD_TEST_VANISH" >&2
	exit 128
fi`)
		env := append(os.Environ(),
			"PATH="+shim,
			"MD_TEST_MARKER="+filepath.Join(t.TempDir(), "diff-injected"),
			"MD_TEST_VANISH="+vanished,
		)
		out, stderr := runGitStatusCommand(t, dir, env)
		status, err := parseGitStatus(out)
		if err != nil {
			t.Fatalf("parseGitStatus() error %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
		}
		if strings.Contains(stderr, "fatal: stat") {
			t.Errorf("recovered transient error leaked to stderr: %s", stderr)
		}
		paths := make([]string, len(status.DiffStat))
		for i, stat := range status.DiffStat {
			paths[i] = stat.Path
		}
		if slices.Contains(paths, "vanished.txt") || !slices.Contains(paths, "retained.txt") {
			t.Errorf("diff stat = %v, want retained.txt without vanished.txt", paths)
		}
	})

	t.Run("valid skips an untracked file that vanishes before add", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		vanished := filepath.Join(dir, "vanished.txt")
		if err := os.WriteFile(vanished, []byte("vanished\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		shim := gitShim(t, `if [ "$1" = add ] && [ ! -e "$MD_TEST_MARKER" ]; then
	: > "$MD_TEST_MARKER"
	rm -f "$MD_TEST_VANISH"
fi`)
		env := append(os.Environ(),
			"PATH="+shim,
			"MD_TEST_MARKER="+filepath.Join(t.TempDir(), "add-injected"),
			"MD_TEST_VANISH="+vanished,
		)
		out, stderr := runGitStatusCommand(t, dir, env)
		if _, err := parseGitStatus(out); err != nil {
			t.Fatalf("parseGitStatus() error %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
		}
		if strings.Contains(stderr, "did not match any files") {
			t.Errorf("skipped add error leaked to stderr: %s", stderr)
		}
	})

	t.Run("valid ignores an untracked nested repository", func(t *testing.T) {
		t.Parallel()
		dir := initStatusRepo(t)
		nested := filepath.Join(dir, "nested")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, nested, "init", "-b", "main")
		out, stderr := runGitStatusCommand(t, dir, os.Environ())
		status, err := parseGitStatus(out)
		if err != nil {
			t.Fatalf("parseGitStatus() error %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
		}
		untracked := make([]string, len(status.Uncommitted))
		for i, file := range status.Uncommitted {
			untracked[i] = file.Path
		}
		if !slices.Contains(untracked, "nested/") {
			t.Errorf("uncommitted = %v, want nested/", untracked)
		}
	})
}

func TestGitCommitDiffStatCommand(t *testing.T) {
	t.Parallel()
	from := "1111111111111111111111111111111111111111"
	to := "2222222222222222222222222222222222222222"
	cmd, err := gitCommitDiffStatCommand("/work/repo's copy", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmd, `cd '/work/repo'"'"'s copy'`) {
		t.Errorf("gitCommitDiffStatCommand() does not safely quote repo: %q", cmd)
	}
	if !strings.Contains(cmd, "git diff --numstat --stat --find-renames=50% '"+from+"' '"+to+"' --") {
		t.Errorf("gitCommitDiffStatCommand() = %q, want commit comparison", cmd)
	}
	if _, err := gitCommitDiffStatCommand("/repo", "short", to); err == nil {
		t.Fatal("gitCommitDiffStatCommand() accepted a short object ID")
	}
}

func TestGitFileDiffCommand(t *testing.T) {
	t.Parallel()
	t.Run("normalizes configurable output", func(t *testing.T) {
		t.Parallel()
		dir := initFileDiffRepo(t)
		runTestGit(t, dir, "config", "core.quotePath", "false")
		runTestGit(t, dir, "config", "diff.algorithm", "histogram")
		runTestGit(t, dir, "config", "diff.mnemonicPrefix", "true")
		runTestGit(t, dir, "config", "diff.noprefix", "true")
		runTestGit(t, dir, "config", "diff.renames", "false")
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		cmd, err := gitFileDiffCommand(dir, "", "tracked.txt", "")
		if err != nil {
			t.Fatal(err)
		}
		out := runFileDiffCommand(t, cmd)
		for _, header := range []string{
			"diff --git a/tracked.txt b/tracked.txt",
			"--- a/tracked.txt",
			"+++ b/tracked.txt",
		} {
			if !strings.Contains(out, header) {
				t.Errorf("normalized diff missing %q: %q", header, out)
			}
		}
	})

	t.Run("committed file", func(t *testing.T) {
		t.Parallel()
		dir := initFileDiffRepo(t)
		path := filepath.Join(dir, "tracked.txt")
		if err := os.WriteFile(path, []byte("committed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "tracked.txt")
		runTestGit(t, dir, "commit", "-m", "change tracked")
		commit := runTestGitOutput(t, dir, "rev-parse", "HEAD")
		if err := os.WriteFile(path, []byte("working\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		cmd, err := gitFileDiffCommand(dir, commit, "tracked.txt", "")
		if err != nil {
			t.Fatal(err)
		}
		out := runFileDiffCommand(t, cmd)
		if !strings.Contains(out, "+committed") || strings.Contains(out, "+working") {
			t.Errorf("committed diff = %q", out)
		}
	})

	t.Run("retains file metadata", func(t *testing.T) {
		t.Parallel()
		t.Run("new", func(t *testing.T) {
			t.Parallel()
			dir := initFileDiffRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runTestGit(t, dir, "add", "new.txt")
			runTestGit(t, dir, "commit", "-m", "add file")
			assertFileDiffContains(t, dir, runTestGitOutput(t, dir, "rev-parse", "HEAD"), "new.txt", "new file mode 100644")
		})

		t.Run("deleted", func(t *testing.T) {
			t.Parallel()
			dir := initFileDiffRepo(t)
			runTestGit(t, dir, "rm", "tracked.txt")
			runTestGit(t, dir, "commit", "-m", "delete file")
			assertFileDiffContains(t, dir, runTestGitOutput(t, dir, "rev-parse", "HEAD"), "tracked.txt", "deleted file mode 100644")
		})

		t.Run("renamed and mode changed", func(t *testing.T) {
			t.Parallel()
			dir := initFileDiffRepo(t)
			runTestGit(t, dir, "mv", "tracked.txt", "moved.txt")
			if err := os.Chmod(filepath.Join(dir, "moved.txt"), 0o700); err != nil { //nolint:gosec // The executable mode change is the behavior under test.
				t.Fatal(err)
			}
			runTestGit(t, dir, "add", "moved.txt")
			runTestGit(t, dir, "commit", "-m", "move file")
			assertFileDiffContains(
				t,
				dir,
				runTestGitOutput(t, dir, "rev-parse", "HEAD"),
				"moved.txt",
				"old mode 100644",
				"new mode 100755",
				"similarity index 100%",
				"rename from tracked.txt",
				"rename to moved.txt",
			)
		})
	})

	t.Run("uncommitted files preserve index", func(t *testing.T) {
		t.Parallel()
		dir := initFileDiffRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("staged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, dir, "add", "tracked.txt")
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			path  string
			added string
		}{
			{path: "tracked.txt", added: "+staged"},
			{path: "untracked.txt", added: "+new"},
		}
		for _, tc := range cases {
			cmd, err := gitFileDiffCommand(dir, "", tc.path, "")
			if err != nil {
				t.Fatal(err)
			}
			if out := runFileDiffCommand(t, cmd); !strings.Contains(out, tc.added) {
				t.Errorf("uncommitted diff for %s = %q", tc.path, out)
			}
		}
		if cached := runTestGitOutput(t, dir, "diff", "--cached", "--name-only"); cached != "tracked.txt" {
			t.Errorf("cached diff = %q, want tracked.txt", cached)
		}
	})

	t.Run("uncommitted rename", func(t *testing.T) {
		t.Parallel()
		dir := initFileDiffRepo(t)
		runTestGit(t, dir, "mv", "tracked.txt", "moved.txt")
		cmd, err := gitFileDiffCommand(dir, "", "moved.txt", "tracked.txt")
		if err != nil {
			t.Fatal(err)
		}
		out := runFileDiffCommand(t, cmd)
		for _, metadata := range []string{
			"similarity index 100%",
			"rename from tracked.txt",
			"rename to moved.txt",
		} {
			if !strings.Contains(out, metadata) {
				t.Errorf("renamed diff missing %q: %q", metadata, out)
			}
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		t.Parallel()
		if _, err := gitFileDiffCommand("/repo", "", "", ""); err == nil {
			t.Fatal("empty path succeeded")
		}
		if _, err := gitFileDiffCommand("/repo", "--all", "file.txt", ""); err == nil {
			t.Fatal("invalid commit succeeded")
		}
	})
}

func assertFileDiffContains(t *testing.T, dir, commit, path string, want ...string) {
	cmd, err := gitFileDiffCommand(dir, commit, path, "")
	if err != nil {
		t.Fatal(err)
	}
	out := runFileDiffCommand(t, cmd)
	for _, fragment := range want {
		if !strings.Contains(out, fragment) {
			t.Errorf("file diff missing %q: %q", fragment, out)
		}
	}
}

func initFileDiffRepo(t *testing.T) string {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.email", "caic@example.com")
	runTestGit(t, dir, "config", "user.name", "caic test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "tracked.txt")
	runTestGit(t, dir, "commit", "-m", "base")
	return dir
}

func runFileDiffCommand(t *testing.T, command string) string {
	cmd := exec.CommandContext(t.Context(), "bash", "-c", command) //nolint:gosec // command is built from test-owned paths and refs.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("file diff command: %v: %s", err, out)
	}
	return string(out)
}

func runTestGit(t *testing.T, dir string, args ...string) {
	_ = runTestGitOutput(t, dir, args...)
}

// initStatusRepo creates a repository that gitStatusCommand can inspect: one
// commit on main tracking origin/main, matching a container's primary branch.
func initStatusRepo(t *testing.T) string {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.email", "caic@example.com")
	runTestGit(t, dir, "config", "user.name", "caic test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "tracked.txt")
	runTestGit(t, dir, "commit", "-m", "base")
	runTestGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	runTestGit(t, dir, "remote", "add", "origin", ".")
	runTestGit(t, dir, "branch", "--set-upstream-to=origin/main", "main")
	return dir
}

// gitShim writes a git wrapper that runs inject before delegating to the real
// git, and returns a PATH value that puts the wrapper first.
func gitShim(t *testing.T, inject string) string {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + inject + "\nexec " + shellQuote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // the wrapper must be executable.
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func runGitStatusCommand(t *testing.T, dir string, env []string) (stdout, stderr string) {
	cmd := exec.CommandContext(t.Context(), "bash", "-c", gitStatusCommand(dir, "origin", "main")) //nolint:gosec // repository is a test temp dir.
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("status command: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

func runTestGitOutput(t *testing.T, dir string, args ...string) string {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // arguments are hardcoded test fixtures.
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	} else {
		return strings.TrimSpace(string(out))
	}
	return ""
}
