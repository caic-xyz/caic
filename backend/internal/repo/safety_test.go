// Tests for task safety validation checks.

package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/logtest"
)

func TestCheckSafety(t *testing.T) {
	t.Parallel()
	t.Run("LargeBinary", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		// Create a branch with a large binary file.
		runGit(t, clone, "checkout", "-b", "caic-0")
		data := make([]byte, 600*1024) // 600 KB > 500 KB threshold
		for i := range data {
			data[i] = byte(i % 256)
		}
		if err := os.WriteFile(filepath.Join(clone, "big.bin"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "big.bin")
		runGit(t, clone, "commit", "-m", "add binary")

		ds := v3.DiffStat{{Path: "big.bin", Binary: true}}
		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", ds)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if issues[0].Kind != "large_binary" {
			t.Errorf("kind = %q, want %q", issues[0].Kind, "large_binary")
		}
		if issues[0].File != "big.bin" {
			t.Errorf("file = %q, want %q", issues[0].File, "big.bin")
		}
	})

	t.Run("SmallBinaryOK", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		data := make([]byte, 100) // well under threshold
		if err := os.WriteFile(filepath.Join(clone, "small.bin"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "small.bin")
		runGit(t, clone, "commit", "-m", "add small binary")

		ds := v3.DiffStat{{Path: "small.bin", Binary: true}}
		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", ds)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 0 {
			t.Errorf("got %d issues, want 0", len(issues))
		}
	})

	t.Run("SecretDetection", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		content := "package main\n" + `const awsKey = "AK` + `IAIOSFODNN7EXAMPLE"` + "\n"
		if err := os.WriteFile(filepath.Join(clone, "config.go"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "config.go")
		runGit(t, clone, "commit", "-m", "add config")

		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if issues[0].Kind != "secret" {
			t.Errorf("kind = %q, want %q", issues[0].Kind, "secret")
		}
		if !strings.Contains(issues[0].Detail, "AWS") {
			t.Errorf("detail = %q, want to contain AWS", issues[0].Detail)
		}
	})

	t.Run("PrivateKey", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		content := "-----BEGIN RSA " + "PRIVATE KEY-----\nblahblah\n-----END RSA PRIVATE KEY-----\n"
		if err := os.WriteFile(filepath.Join(clone, "key.pem"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "key.pem")
		runGit(t, clone, "commit", "-m", "add key")

		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if !strings.Contains(issues[0].Detail, "private key") {
			t.Errorf("detail = %q, want to contain 'private key'", issues[0].Detail)
		}
	})

	t.Run("HardcodedCredential", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		content := "pass" + `word = "supersecretpassword123"` + "\n"
		if err := os.WriteFile(filepath.Join(clone, "app.conf"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "app.conf")
		runGit(t, clone, "commit", "-m", "add config")

		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if !strings.Contains(issues[0].Detail, "credential") {
			t.Errorf("detail = %q, want to contain 'credential'", issues[0].Detail)
		}
	})

	t.Run("RemoteRef", func(t *testing.T) {
		t.Parallel()
		// After Runtime.Fetch, commits live at refs/remotes/<runtime>/<branch>,
		// not a local branch. CheckSafety must work with full ref paths.
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		if err := os.WriteFile(filepath.Join(clone, "new.go"), []byte("package new\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "new.go")
		runGit(t, clone, "commit", "-m", "add file")

		// Simulate what Runtime.Fetch does: store the commit under a remote ref.
		runGit(t, clone, "update-ref", "refs/remotes/md-caic-0/caic-0", "caic-0")
		// Delete the local branch so only the remote ref remains.
		runGit(t, clone, "checkout", "main")
		runGit(t, clone, "branch", "-D", "caic-0")

		// Using the bare branch name would fail (the old bug).
		ref := "refs/remotes/md-caic-0/caic-0"
		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, ref, "main", nil)
		if err != nil {
			t.Fatalf("CheckSafety with remote ref failed: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("got %d issues, want 0: %+v", len(issues), issues)
		}
	})

	t.Run("NoIssues", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clone := initTestRepo(t, "main")

		runGit(t, clone, "checkout", "-b", "caic-0")
		if err := os.WriteFile(filepath.Join(clone, "clean.go"), []byte("package clean\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", "clean.go")
		runGit(t, clone, "commit", "-m", "add clean")

		ds := v3.DiffStat{{Path: "clean.go", LinesAdded: 1}}
		issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "caic-0", "main", ds)
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) != 0 {
			t.Errorf("got %d issues, want 0: %+v", len(issues), issues)
		}
	})
	t.Run("Streaming", func(t *testing.T) {
		t.Parallel()
		t.Run("LongLineAndRedactedLogs", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "topic")
			key := "AK" + "IAIOSFODNN7EXAMPLE"
			data := strings.Repeat("x", 128<<10) + key + "\n" + key + "\n"
			if err := os.WriteFile(filepath.Join(clone, "large.txt"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "fixture")
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			issues, err := CheckSafety(t.Context(), log, clone, "topic", "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 1 || issues[0].File != "large.txt" {
				t.Fatalf("issues: %+v", issues)
			}
			if strings.Contains(logs.String(), key) {
				t.Fatal("credential leaked into log")
			}
		})
		t.Run("OversizedLine", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "topic")
			if err := os.WriteFile(filepath.Join(clone, "large.txt"), []byte(strings.Repeat("x", 2<<20)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "fixture")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			issues, err := CheckSafety(ctx, logtest.Logger(t), clone, "topic", "main", nil)
			if err == nil || !strings.Contains(err.Error(), "1 MiB") || len(issues) != 0 {
				t.Fatalf("issues=%v, err=%v", issues, err)
			}
			if ctx.Err() != nil {
				t.Fatalf("scan did not release its producer promptly: %v", ctx.Err())
			}
		})
		t.Run("LineBoundary", func(t *testing.T) {
			t.Parallel()
			for _, length := range []int{(1 << 20) - 1, 1 << 20} {
				t.Run(fmt.Sprintf("AddedContent%d", length), func(t *testing.T) {
					t.Parallel()
					clone := initTestRepo(t, "main")
					runGit(t, clone, "checkout", "-b", "topic")
					if err := os.WriteFile(filepath.Join(clone, "boundary.txt"), []byte(strings.Repeat("x", length)+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, clone, "add", ".")
					runGit(t, clone, "commit", "-m", "fixture")
					_, err := CheckSafety(t.Context(), logtest.Logger(t), clone, "topic", "main", nil)
					// The added-line '+' counts toward the documented diff-line bound.
					if length < 1<<20 && err != nil {
						t.Fatal(err)
					}
					if length == 1<<20 && (err == nil || !strings.Contains(err.Error(), "1 MiB")) {
						t.Fatalf("oversized line: %v", err)
					}
				})
			}
		})

		t.Run("CommandError", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			issues, err := CheckSafety(t.Context(), logtest.Logger(t), clone, "missing", "main", nil)
			if err == nil || !strings.Contains(err.Error(), "git diff for secret scan") || len(issues) != 0 {
				t.Fatalf("issues=%v, err=%v", issues, err)
			}
		})
		t.Run("CancelInheritedPipe", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "topic")
			if err := os.WriteFile(filepath.Join(clone, "changed.txt"), []byte("change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "fixture")
			script := filepath.Join(t.TempDir(), "diff.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 3\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "config", "diff.external", "sh "+script)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := CheckSafety(ctx, logtest.Logger(t), clone, "topic", "main", nil)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("cancelled scan waited for inherited stdout")
			}
		})
		t.Run("ErrorRedaction", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "topic")
			if err := os.WriteFile(filepath.Join(clone, "changed.txt"), []byte("change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "fixture")
			script := filepath.Join(t.TempDir(), "diff.sh")
			credential := "AK" + "IAIOSFODNN7EXAMPLE"
			source := "private-source-canary"
			content := "#!/bin/sh\nhead -c 1048576 /dev/zero | tr '\\0' x >&2\nprintf '" + credential + " " + source + "\\n' >&2\nexit 1\n"
			if err := os.WriteFile(script, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "config", "diff.external", "sh "+script)
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			_, err := CheckSafety(t.Context(), log, clone, "topic", "main", nil)
			if err == nil {
				t.Fatal("command unexpectedly succeeded")
			}
			msg := err.Error()
			log.ErrorContext(t.Context(), "sync failed", "err", err)
			if !strings.Contains(msg, "git diff for secret scan") || !strings.Contains(msg, "exit status") {
				t.Fatal("error lost operation or command status")
			}
			for _, value := range []string{credential, source} {
				if strings.Contains(msg, value) || strings.Contains(logs.String(), value) {
					t.Fatal("command source or credential leaked through error")
				}
			}
		})
	})
}

func TestHumanSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{500 * 1024, "500 KB"},
		{1024 * 1024, "1.0 MB"},
		{1536 * 1024, "1.5 MB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.in)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScanDiffForSecrets_Deduplication(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clone := initTestRepo(t, "main")

	runGit(t, clone, "checkout", "-b", "caic-0")
	// Multiple AWS keys in the same file should produce only one issue.
	content := "key1 = \"AK" + "IAIOSFODNN7EXAMPLE\"\nkey2 = \"AK" + "IAIOSFODNN7EXAMPLE\"\n"
	if err := os.WriteFile(filepath.Join(clone, "keys.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "keys.go")
	runGit(t, clone, "commit", "-m", "add keys")

	issues, err := scanDiffForSecrets(ctx, logtest.Logger(t), clone, "caic-0", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Errorf("got %d issues, want 1 (deduplication)", len(issues))
	}
}

// initTestRepo and runGit are defined in checkout_test.go (same package).
