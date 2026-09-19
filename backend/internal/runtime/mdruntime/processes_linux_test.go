//go:build linux

// Regression test for process start times under md's clobbered UTC zoneinfo.

package mdruntime

import (
	"os/exec"
	"testing"
	"time"
)

// TestProcessCommandUTCStartedAt runs the production processCommand and checks
// that a just-started process reports a StartedAt near the current time. md
// bind-mounts the host /etc/localtime over the symlinked
// /usr/share/zoneinfo/Etc/UTC, so a zoneinfo "UTC" lookup resolves to the host
// zone and would skew StartedAt by the host's UTC offset.
func TestProcessCommandUTCStartedAt(t *testing.T) {
	t.Parallel()

	child := exec.CommandContext(t.Context(), "sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	out, err := exec.CommandContext(t.Context(), "sh", "-c", processCommand).Output()
	if err != nil {
		t.Skipf("cannot run process command: %v", err)
	}
	procs, err := parseProcessOutput(string(out))
	if err != nil {
		t.Fatalf("parseProcessOutput() error = %v", err)
	}
	var found bool
	for i := range procs {
		if procs[i].PID != child.Process.Pid {
			continue
		}
		found = true
		if age := time.Since(procs[i].StartedAt); age < -5*time.Second || age > 5*time.Second {
			t.Errorf("StartedAt age = %v, want within 5s of now", age)
		}
	}
	if !found {
		t.Fatalf("pid %d missing from process list (%d entries)", child.Process.Pid, len(procs))
	}
}
