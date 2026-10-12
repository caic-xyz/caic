// Loads a bounded login-shell environment snapshot on Unix hosts.

//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// importShellEnvironment runs once before any worker goroutines or LookPath calls.
// A failed probe leaves the inherited environment untouched. Shell startup output
// may contain credentials, so errors never include captured stdout or stderr.
func importShellEnvironment(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	shell, err := loginShell(ctx)
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create shell environment pipe: %w", err)
	}
	defer func() { _ = r.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = r.SetReadDeadline(time.Now()) })
	defer stop()
	// env -0 ships with modern macOS and GNU/Linux. Descriptor 3 keeps the
	// NUL-delimited snapshot separate from arbitrary startup stdout/stderr.
	cmd := exec.CommandContext(ctx, shell, "-ilc", "exec /usr/bin/env -0 >&3") //nolint:gosec // user's configured shell; command and env utility path are fixed.
	cmd.ExtraFiles = []*os.File{w}
	// Detach from the controlling terminal so interactive Bash cannot stop
	// itself with SIGTTIN. The new session is also its own process group;
	// cancellation kills startup children as well as the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	// ExtraFiles pipes are caller-owned; WaitDelay does not close them. A
	// startup daemon can retain descriptor 3 after the shell exits, so bound
	// the read as well as the shell process lifetime.
	deadline, _ := ctx.Deadline()
	if err = r.SetReadDeadline(deadline); err == nil {
		err = cmd.Start()
	}
	closeErr := w.Close()
	if err != nil {
		return fmt.Errorf("start shell environment probe: %w", errors.Join(err, closeErr))
	}
	if closeErr != nil {
		cancel()
	}
	out, readErr := io.ReadAll(io.LimitReader(r, shellEnvLimit+1))
	if len(out) > shellEnvLimit {
		readErr = errors.New("shell environment output exceeds 1 MiB")
	}
	if readErr != nil {
		cancel()
	}
	if err := errors.Join(closeErr, readErr, cmd.Wait()); err != nil {
		return fmt.Errorf("load shell environment: %w", err)
	}
	env, err := parseShellEnvironment(out)
	if err != nil {
		return err
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("apply shell environment: %w", err)
		}
	}
	return nil
}

// loginShell uses SHELL when supplied by the session. Services without SHELL
// query the account database (including NSS on Linux), not a hardcoded Bash.
func loginShell(ctx context.Context) (string, error) {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell, nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "/usr/bin/id", "-P")
	default:
		cmd = exec.CommandContext(ctx, "/usr/bin/getent", "passwd", strconv.Itoa(os.Getuid())) //nolint:gosec // account lookup for the current process UID.
	}
	cmd.WaitDelay = time.Second
	out := &accountShellOutput{}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("resolve account shell: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(out.buf.String()), ":")
	if len(fields) < 7 || fields[len(fields)-1] == "" {
		return "", errors.New("account database returned no login shell")
	}
	return fields[len(fields)-1], nil
}
