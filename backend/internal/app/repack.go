// Repack registered Git checkouts when enough loose objects accumulate.

package app

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// repackRepository consolidates a checkout's reachable objects and retains
// unreachable objects in a cruft pack. Limit Git's pack memory and threads so
// maintenance of a large checkout does not exhaust the server's memory.
func repackRepository(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, //nolint:gosec // registered checkout path
		"-c", "core.packedGitWindowSize=64m", "-c", "core.packedGitLimit=256m",
		"-c", "pack.threads=1", "-c", "pack.windowMemory=64m", "-c", "pack.deltaCacheSize=64m",
		"repack", "-a", "-d", "--cruft")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git repack %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}
