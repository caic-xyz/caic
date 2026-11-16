// Records the running harness version for harnesses whose protocol does not report it.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// versionProbeTimeout bounds one --version invocation, including the SSH connection.
const versionProbeTimeout = 10 * time.Second

// RecordVersion runs executable --version in the target before a process
// start and records the result, so each start in the task log names the
// harness version that ran, including after an in-place update. Call it only
// for harnesses whose protocol does not report their own version.
//
// A failed probe is logged and the start continues: the version is
// diagnostic, and a broken harness reports its own launch failure.
func RecordVersion(ctx context.Context, opts *Options, executable string) error {
	host := opts.Target.SSHHost
	if host == "" {
		return errors.New("agent connection target missing SSH host")
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := SSHCommand(probeCtx, host, executable, "--version").Output()
	if err == nil {
		var version string
		if version, err = parseVersion(string(out)); err == nil {
			return opts.Records.AppendMessage(ctx, &MetaSessionMessage{AgentVersion: version})
		}
	}
	opts.Logger.WarnContext(ctx, "harness version unavailable", "executable", executable, "target", host, "err", err)
	return nil
}

// parseVersion returns the first field of the first line that starts with a
// digit, matching "1.5.2", "codex-cli 0.169.0", and "2.1.325 (Claude Code)".
func parseVersion(out string) (string, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	for field := range strings.FieldsSeq(line) {
		if field[0] >= '0' && field[0] <= '9' {
			return field, nil
		}
	}
	return "", fmt.Errorf("no version in %q", line)
}
