// Tests SSH argv quoting through a real POSIX shell and the remote command shape.

package agent

import (
	"os/exec"
	"slices"
	"testing"
)

func TestShellJoin(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "plain", "a=b", "spaces and 'quotes'", "$(printf injected); $HOME `printf injected`\n*"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", ShellJoin([]string{"env", "TEST_VALUE=" + value, "/bin/sh", "-c", `printf '%s' "$TEST_VALUE"`})) //nolint:gosec // Exercises shell quoting with fixed hostile test values.
			out, err := cmd.Output()
			if err != nil || string(out) != value {
				t.Fatalf("shell argv preservation = %q, %v; want %q", out, err, value)
			}
		})
	}
	t.Run("assignment as command word", func(t *testing.T) {
		t.Parallel()
		// A bare NAME=value first word would be an assignment, not a command.
		out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", ShellJoin([]string{"X=1"})+" 2>&1; echo $?").Output() //nolint:gosec // Fixed test input.
		if err != nil || string(out) == "0\n" {
			t.Fatalf("X=1 ran as an assignment: %q, %v", out, err)
		}
	})
}

func TestSSHCommand(t *testing.T) {
	t.Parallel()
	cmd := SSHCommand(t.Context(), "task", "pi", "update", "--all")
	if got := cmd.Args[len(cmd.Args)-2:]; !slices.Equal(got, []string{"task", "pi update --all"}) {
		t.Fatalf("args = %q", cmd.Args)
	}
	if !slices.Contains(cmd.Args, "ServerAliveInterval=5") {
		t.Fatalf("one-shot command lacks keepalive: %q", cmd.Args)
	}
	if session := sshSessionCommand(t.Context(), "task", "attach"); slices.Contains(session.Args, "ServerAliveInterval=5") || !slices.Contains(session.Args, "ConnectTimeout=10") {
		t.Fatalf("session args = %q", session.Args)
	}
}
