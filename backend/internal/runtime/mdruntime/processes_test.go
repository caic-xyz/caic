// Tests for parsing ps output from md containers.

package mdruntime

import (
	"testing"
	"time"
)

func TestSignalCommand(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := signalCommand(123, "SIGTERM")
		if err != nil {
			t.Fatal(err)
		}
		if got != "kill -s SIGTERM 123" {
			t.Errorf("signalCommand() = %q, want %q", got, "kill -s SIGTERM 123")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			pid int
			sig string
		}{
			{0, "SIGTERM"},
			{123, "SIGINT"},
		} {
			if _, err := signalCommand(tt.pid, tt.sig); err == nil {
				t.Errorf("signalCommand(%d, %q) succeeded, want error", tt.pid, tt.sig)
			}
		}
	})
}

func TestParsePSOutput(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, time.March, 20, 10, 28, 30, 0, time.UTC)
	out := `    1     0     1 root     Ss   19   0    1  0.0  0.1  1024 1 Fri Mar 20 08:30:00 2026 /sbin/init
  123     1   123 user     Sl   20   5    3  1.5  2.5  2048 3 Fri Mar 20 10:28:30 2026 agent run --flag value
  124   123   123 user     R    20   0    1  0.0  0.0  3072 0 Fri Mar 20 10:30:00 2026 ps -eo pid,ppid,pgrp,user,stat,pri,ni,nlwp,%cpu,%mem,rss,cputimes,lstart,args --no-headers
broken
`
	procs, err := parsePSOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 {
		t.Fatalf("processes = %+v, want 2 entries after filtering ps", procs)
	}
	got := procs[1]
	if got.PID != 123 || got.PPID != 1 || got.PGRP != 123 || got.User != "user" || got.State != "Sl" || got.Priority != 20 || got.Nice != 5 || got.Threads != 3 || got.CPU != 1.5 || got.Mem != 2.5 || got.RSSBytes != 2_097_152 || got.CPUTime != 3*time.Second || !got.StartedAt.Equal(startedAt) || got.Command != "agent run --flag value" {
		t.Errorf("process = %+v, want parsed agent command", got)
	}
}
