// Tests shell environment snapshots, subprocess discovery, and failed probes.

//go:build !windows

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseShellEnvironment(t *testing.T) {
	t.Parallel()
	t.Run("values and bookkeeping", func(t *testing.T) {
		t.Parallel()
		out := "PATH=/custom/bin\x00TOKEN=a=b\n'quoted'\x00EMPTY=\x00HOME=/probe\x00PWD=/probe\x00SHLVL=5\x00NOTIFY_SOCKET=/probe\x00"
		env, err := parseShellEnvironment([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		if len(env) != 3 || env["PATH"] != "/custom/bin" || env["TOKEN"] != "a=b\n'quoted'" {
			t.Fatalf("unexpected snapshot keys or values")
		}
		if v, ok := env["EMPTY"]; !ok || v != "" {
			t.Fatal("missing empty export")
		}
	})
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		for _, out := range []string{
			"",
			"KEY=value",
			"malformed\x00",
			"=value\x00",
			"KEY=value\x00\x00",
		} {
			if _, err := parseShellEnvironment([]byte(out)); err == nil {
				t.Fatal("accepted invalid snapshot")
			}
		}
	})
}

func TestImportShellEnvironment(t *testing.T) {
	t.Run("exports reach lookup and children", func(t *testing.T) {
		dir := t.TempDir()
		shell := filepath.Join(dir, "shell")
		sessionCheck := ""
		if runtime.GOOS == "linux" {
			// A session leader has no inherited controlling terminal. Without
			// setsid, interactive Bash can stop itself on terminal-started caic.
			sessionCheck = "[ \"$(/bin/ps -o sid= -p $$ | /usr/bin/tr -d ' ')\" = \"$$\" ] || exit 9\n"
		}
		script := "#!/bin/sh\n" + sessionCheck +
			"[ \"$1\" = -ilc ] || exit 1\n" +
			"printf 'startup noise\\000not=an-export\\000\\n'\n" +
			"export PATH=\"$CAIC_TEST_BIN\"\n" +
			"export CAIC_TEST_VALUE=\"line one\nline two='quoted'\"\n" +
			"/bin/sh -c \"$2\"\n" +
			"printf 'logout noise\\n'\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // executable shell fixture in test-owned directory.
			t.Fatal(err)
		}
		tool := filepath.Join(dir, "caic-env-test-tool")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$CAIC_TEST_VALUE\"\n"), 0o700); err != nil { //nolint:gosec // executable tool fixture in test-owned directory.
			t.Fatal(err)
		}
		// Register cleanup for every key the probe changes. It returns all other
		// inherited values unchanged, except bookkeeping that the parser ignores.
		t.Setenv("SHELL", shell)
		t.Setenv("PATH", os.Getenv("PATH"))
		t.Setenv("CAIC_TEST_BIN", dir)
		t.Setenv("CAIC_TEST_VALUE", "inherited")
		if err := importShellEnvironment(t.Context()); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), "caic-env-test-tool").Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "line one\nline two='quoted'" {
			t.Fatal("child did not inherit shell exports")
		}
	})
	t.Run("failed probe retains inherited environment", func(t *testing.T) {
		shell := filepath.Join(t.TempDir(), "shell")
		// Even a complete snapshot cannot be applied after an unsuccessful exit.
		script := "#!/bin/sh\nexport CAIC_TEST_VALUE=changed\n/usr/bin/env -0 >&3\nprintf 'secret diagnostic' >&2\nexit 7\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // executable shell fixture in test-owned directory.
			t.Fatal(err)
		}
		t.Setenv("SHELL", shell)
		t.Setenv("CAIC_TEST_VALUE", "inherited")
		err := importShellEnvironment(t.Context())
		if err == nil || strings.Contains(err.Error(), "secret diagnostic") {
			t.Fatalf("expected sanitized probe error, got %v", err)
		}
		if os.Getenv("CAIC_TEST_VALUE") != "inherited" {
			t.Fatal("failed import changed environment")
		}
	})
	t.Run("retained snapshot pipe is bounded", func(t *testing.T) {
		shell := filepath.Join(t.TempDir(), "shell")
		// The shell exits successfully, but a startup child keeps descriptor 3.
		script := "#!/bin/sh\nexport CAIC_TEST_VALUE=changed\n/usr/bin/env -0 >&3\n/bin/sleep 30 &\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // executable shell fixture in test-owned directory.
			t.Fatal(err)
		}
		t.Setenv("SHELL", shell)
		t.Setenv("CAIC_TEST_VALUE", "inherited")
		ctx, cancel := context.WithCancel(t.Context())
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		t.Cleanup(func() {
			timer.Stop()
			cancel()
		})
		start := time.Now()
		if err := importShellEnvironment(ctx); err == nil {
			t.Fatal("accepted snapshot with a retained pipe")
		}
		if time.Since(start) > time.Second {
			t.Fatal("pipe read did not respond to cancellation")
		}
		if os.Getenv("CAIC_TEST_VALUE") != "inherited" {
			t.Fatal("incomplete capture changed environment")
		}
	})
	t.Run("oversized snapshot retains inherited environment", func(t *testing.T) {
		shell := filepath.Join(t.TempDir(), "shell")
		// More than 1 MiB of machine output must cancel the writer promptly.
		script := "#!/bin/sh\n/usr/bin/dd if=/dev/zero bs=1048577 count=1 >&3 2>/dev/null\n/bin/sleep 30\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // executable shell fixture in test-owned directory.
			t.Fatal(err)
		}
		t.Setenv("SHELL", shell)
		t.Setenv("CAIC_TEST_VALUE", "inherited")
		err := importShellEnvironment(t.Context())
		if err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
			t.Fatalf("expected capture-limit error, got %v", err)
		}
		if os.Getenv("CAIC_TEST_VALUE") != "inherited" {
			t.Fatal("oversized capture changed environment")
		}
	})
	t.Run("timeout stops startup children", func(t *testing.T) {
		dir := t.TempDir()
		shell := filepath.Join(dir, "shell")
		marker := filepath.Join(dir, "survived")
		script := "#!/bin/sh\n/bin/sh -c '/bin/sleep 0.5; printf survived > \"$CAIC_TEST_MARKER\"' &\nwait\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // executable shell fixture in test-owned directory.
			t.Fatal(err)
		}
		t.Setenv("SHELL", shell)
		t.Setenv("CAIC_TEST_MARKER", marker)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		t.Cleanup(cancel)
		if err := importShellEnvironment(ctx); err == nil {
			t.Fatal("accepted timed out probe")
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("startup child survived cancellation: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/sh")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := importShellEnvironment(ctx); err == nil {
			t.Fatal("accepted canceled probe")
		}
	})
}

func TestAccountShellOutput(t *testing.T) {
	t.Parallel()
	out := &accountShellOutput{}
	if _, err := io.Copy(out, strings.NewReader(strings.Repeat("x", (1<<20)+1))); err == nil {
		t.Fatal("accepted oversized shell output")
	}
	if out.buf.Len() > 1<<20 {
		t.Fatal("output exceeded capture limit")
	}
}
