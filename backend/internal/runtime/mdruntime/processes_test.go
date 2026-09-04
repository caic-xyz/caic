// Tests and benchmarks for parsing process snapshots from md containers.

package mdruntime

import (
	"testing"
	"time"
)

const benchmarkPSOutput = `    1     0     1 root     Ss   19   0    1  0.0  0.1  1024 1 Fri Mar 20 08:30:00 2026 /sbin/init
   42     1    42 root     S    19   0    1  0.0  0.2  2048 1 Fri Mar 20 08:30:05 2026 /usr/sbin/sshd -D
   99    42    42 root     S    19   0    1  0.0  0.3  3072 0 Fri Mar 20 08:30:10 2026 sshd: user [priv]
  100    99   100 user     S    19   0    1  0.1  0.5  5120 2 Fri Mar 20 08:30:20 2026 -bash
  200   100   100 user     R    19   0    5 45.2 12.3 125952 83 Fri Mar 20 08:31:20 2026 node agent.js
  201   100   100 user     S    19   0    1  0.0  0.1  1024 0 Fri Mar 20 10:29:15 2026 make -j8
  300   201   100 user     R    19   0    1 98.7  5.6 57344 45 Fri Mar 20 10:29:18 2026 gcc main.c
  301   201   100 user     R    19   0    1 97.1  4.8 49152 42 Fri Mar 20 10:29:21 2026 gcc parser.c
`

const benchmarkProcessOutput = `      7 /proc/1/fd
      5 /proc/42/fd
      4 /proc/99/fd
      6 /proc/100/fd
     31 /proc/200/fd
      8 /proc/201/fd
      4 /proc/300/fd
      4 /proc/301/fd
--caic-open-fds--
` + benchmarkPSOutput

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

func TestParseProcessOutput(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		startedAt := time.Date(2026, time.March, 20, 10, 28, 30, 0, time.UTC)
		out := `      7 /proc/1/fd
     17 /proc/123/fd
--caic-open-fds--
    1     0     1 root     Ss   19   0    1  0.0  0.1  1024 1 Fri Mar 20 08:30:00 2026 /sbin/init
  123     1   123 user     Sl   20   5    3  1.5  2.5  2048 3 Fri Mar 20 10:28:30 2026 agent run --flag value
  124   123   123 user     R    20   0    1  0.0  0.0  3072 0 Fri Mar 20 10:30:00 2026 ps -eo pid,ppid,pgrp,user,stat,pri,ni,nlwp,%cpu,%mem,rss,cputimes,lstart,args --no-headers
broken
`
		procs, err := parseProcessOutput(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(procs) != 2 {
			t.Fatalf("processes = %+v, want 2 entries after filtering ps", procs)
		}
		if procs[0].OpenFDs == nil || *procs[0].OpenFDs != 7 {
			t.Errorf("init OpenFDs = %v, want 7", procs[0].OpenFDs)
		}
		got := procs[1]
		if got.PID != 123 || got.PPID != 1 || got.PGRP != 123 || got.User != "user" || got.State != "Sl" || got.Priority != 20 || got.Nice != 5 || got.Threads != 3 || got.OpenFDs == nil || *got.OpenFDs != 17 || got.CPU != 1.5 || got.Mem != 2.5 || got.RSSBytes != 2_097_152 || got.CPUTime != 3*time.Second || !got.StartedAt.Equal(startedAt) || got.Command != "agent run --flag value" {
			t.Errorf("process = %+v, want parsed agent command", got)
		}
	})

	t.Run("unavailable count", func(t *testing.T) {
		t.Parallel()
		procs, err := parseProcessOutput(fdCountsMarker + "\n" + benchmarkPSOutput)
		if err != nil {
			t.Fatal(err)
		}
		if procs[0].OpenFDs != nil {
			t.Errorf("OpenFDs = %v, want nil", procs[0].OpenFDs)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		for _, out := range []string{
			benchmarkPSOutput,
			"invalid\n" + fdCountsMarker + "\n" + benchmarkPSOutput,
		} {
			if _, err := parseProcessOutput(out); err == nil {
				t.Errorf("parseProcessOutput(%q) succeeded, want error", out)
			}
		}
	})
}

func BenchmarkParseProcessOutput(b *testing.B) {
	b.Run("ps_only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			procs, err := parsePSOutput(benchmarkPSOutput)
			if err != nil {
				b.Fatal(err)
			}
			if len(procs) != 8 {
				b.Fatalf("process count = %d", len(procs))
			}
		}
	})
	b.Run("with_fd_counts", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			procs, err := parseProcessOutput(benchmarkProcessOutput)
			if err != nil {
				b.Fatal(err)
			}
			if len(procs) != 8 {
				b.Fatalf("process count = %d", len(procs))
			}
		}
	})
}
