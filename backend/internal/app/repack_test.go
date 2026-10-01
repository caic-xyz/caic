// Tests bounded repository repacking without losing reachable or dangling objects.

package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md/git"
)

func TestRepackRepository(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	repackTestGit(t, dir, "init", "-q")
	repackTestGit(t, dir, "config", "user.name", "Repack Test")
	repackTestGit(t, dir, "config", "user.email", "repack@example.test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repackTestGit(t, dir, "add", "file.txt")
	repackTestGit(t, dir, "commit", "-qm", "first")
	head := repackTestGit(t, dir, "rev-parse", "HEAD")
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "hash-object", "-w", "--stdin") //nolint:gosec // test-controlled path
	cmd.Stdin = strings.NewReader("dangling object\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("write dangling object: %v: %s", err, out)
	}
	dangling := strings.TrimSpace(string(out))
	if err := repackRepository(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	stats, err := (&git.Checkout{Root: dir}).ObjectStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.LooseCount != 0 || stats.PackBytes == 0 {
		t.Fatalf("object stats after repack = %+v", stats)
	}
	if got := repackTestGit(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD after repack = %s, want %s", got, head)
	}
	repackTestGit(t, dir, "cat-file", "-e", dangling)
}

func repackTestGit(t *testing.T, dir string, args ...string) string {
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // test-controlled args
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// BenchmarkRepackRepository measures repack command overhead on a small
// checkout. Large-repository compression needs a representative local checkout.
func BenchmarkRepackRepository(b *testing.B) {
	dir := b.TempDir()
	cmd := exec.CommandContext(b.Context(), "git", "-C", dir, "init", "-q") //nolint:gosec // benchmark-controlled path
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git init: %v: %s", err, out)
	}
	b.ReportAllocs()
	for i := range b.N {
		b.StopTimer()
		cmd := exec.CommandContext(b.Context(), "git", "-C", dir, "hash-object", "-w", "--stdin") //nolint:gosec // benchmark-controlled path
		cmd.Stdin = strings.NewReader(fmt.Sprintf("loose object %d %d", time.Now().UnixNano(), i))
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git hash-object: %v: %s", err, out)
		}
		b.StartTimer()
		if err := repackRepository(b.Context(), dir); err != nil {
			b.Fatal(err)
		}
	}
}
