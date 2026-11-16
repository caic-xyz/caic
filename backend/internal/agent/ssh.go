// SSH commands and file writes into runtime targets, with shared timeouts and argv quoting.

package agent

import (
	"context"
	"os/exec"
	"path"
	"strings"
)

// sshConnectArgs bound connection setup for every SSH command.
var sshConnectArgs = []string{"-o", "ConnectTimeout=10"}

// sshOneShotArgs also end a one-shot command whose connection stops
// responding. Relay sessions omit keepalives: the relay outlives a dropped
// attachment and caic reattaches explicitly.
var sshOneShotArgs = append(append([]string(nil), sshConnectArgs...), "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2")

// SSHCommand runs argv on host. OpenSSH joins remote arguments with spaces for
// the login shell, so each argument is quoted to arrive as one argv value.
func SSHCommand(ctx context.Context, host string, argv ...string) *exec.Cmd {
	return SSHScript(ctx, host, ShellJoin(argv))
}

// SSHScript runs a POSIX shell script on host through its login shell. Quote
// interpolated values with ShellQuote.
func SSHScript(ctx context.Context, host, script string) *exec.Cmd {
	return sshExec(ctx, sshOneShotArgs, host, script)
}

// sshSessionCommand runs a long-lived relay attachment on host.
func sshSessionCommand(ctx context.Context, host, script string) *exec.Cmd {
	return sshExec(ctx, sshConnectArgs, host, script)
}

func sshExec(ctx context.Context, options []string, host, script string) *exec.Cmd {
	args := make([]string, 0, len(options)+2)
	args = append(args, options...)
	args = append(args, host, script)
	return exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // Hosts are runtime-owned; callers quote script values.
}

// WriteFile writes data to path on host, creating its parent directory and
// replacing an existing file.
func WriteFile(ctx context.Context, host, filePath string, data []byte, mode int64) error {
	return deployFiles(ctx, host, path.Dir(filePath), []deployFile{{name: path.Base(filePath), mode: mode, data: data}})
}

// ShellJoin quotes each argument with ShellQuote and joins them with spaces.
func ShellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = ShellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// ShellQuote preserves one argument through a POSIX shell without expansion.
// Arguments made only of characters the shell never interprets stay bare.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@%+:,./_-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
