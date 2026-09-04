// Process inspection combines ps output with Linux procfs descriptor counts into runtime process snapshots.

package mdruntime

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const (
	fdCountsMarker = "--caic-open-fds--"
	processCommand = "find /proc/[0-9]*/fd -mindepth 1 -maxdepth 1 -printf '%h\\n' 2>/dev/null | sort | uniq -c" +
		" && printf '%s\\n' '" + fdCountsMarker + "'" +
		" && exec env LC_ALL=C TZ=UTC ps -eo pid,ppid,pgrp,user,stat,pri,ni,nlwp,%cpu,%mem,rss,cputimes,lstart,args --no-headers"
)

func signalCommand(pid int, sig string) (string, error) {
	if pid < 1 {
		return "", fmt.Errorf("pid must be positive, got %d", pid)
	}
	switch sig {
	case "SIGKILL", "SIGTERM":
		return fmt.Sprintf("kill -s %s %d", sig, pid), nil
	default:
		return "", fmt.Errorf("unsupported signal %q", sig)
	}
}

// parsePSOutput parses ps output. The last column (args) may contain spaces;
// the first seventeen fields are whitespace-separated and the remainder is the command.
func parsePSOutput(out string) ([]runtime.ProcessInfo, error) {
	var procs []runtime.ProcessInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 18)
		var fields []string
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				fields = append(fields, part)
			}
		}
		if len(fields) < 18 {
			fields = strings.Fields(line)
			if len(fields) < 18 {
				continue
			}
			command := strings.Join(fields[17:], " ")
			fields = append(fields[:17], command)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		pgrp, _ := strconv.Atoi(fields[2])
		priority, _ := strconv.Atoi(fields[5])
		nice, _ := strconv.Atoi(fields[6])
		threads, _ := strconv.Atoi(fields[7])
		cpu, _ := strconv.ParseFloat(fields[8], 64)
		mem, _ := strconv.ParseFloat(fields[9], 64)
		rssKiB, _ := strconv.ParseUint(fields[10], 10, 64)
		cpuSeconds, err := strconv.ParseInt(fields[11], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse ps CPU time %q: %w", fields[11], err)
		}
		startedAt, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.Join(fields[12:17], " "))
		if err != nil {
			return nil, fmt.Errorf("parse ps start time %q: %w", strings.Join(fields[12:17], " "), err)
		}
		procs = append(procs, runtime.ProcessInfo{
			PID:       pid,
			PPID:      ppid,
			PGRP:      pgrp,
			User:      fields[3],
			State:     fields[4],
			Priority:  priority,
			Nice:      nice,
			Threads:   threads,
			CPU:       cpu,
			Mem:       mem,
			RSSBytes:  rssKiB * 1024,
			CPUTime:   time.Duration(cpuSeconds) * time.Second,
			StartedAt: startedAt,
			Command:   fields[17],
		})
	}
	return slices.DeleteFunc(procs, func(p runtime.ProcessInfo) bool {
		return strings.HasPrefix(p.Command, "ps ")
	}), nil
}

func parseProcessOutput(out string) ([]runtime.ProcessInfo, error) {
	fdOutput, psOutput, ok := strings.Cut(out, fdCountsMarker+"\n")
	if !ok {
		return nil, errors.New("process output is missing file descriptor counts")
	}
	procs, err := parsePSOutput(psOutput)
	if err != nil {
		return nil, err
	}
	counts, err := parseFDCounts(fdOutput)
	if err != nil {
		return nil, err
	}
	for i := range procs {
		if count, found := counts[procs[i].PID]; found {
			procs[i].OpenFDs = new(count)
		}
	}
	return procs, nil
}

func parseFDCounts(out string) (map[int]int, error) {
	counts := make(map[int]int)
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("parse file descriptor count line %q", line)
		}
		count, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse file descriptor count %q: %w", fields[0], err)
		}
		if count < 0 {
			return nil, fmt.Errorf("file descriptor count must not be negative, got %d", count)
		}
		pidText, ok := strings.CutPrefix(fields[1], "/proc/")
		if !ok {
			return nil, fmt.Errorf("parse file descriptor path %q", fields[1])
		}
		pidText, ok = strings.CutSuffix(pidText, "/fd")
		if !ok {
			return nil, fmt.Errorf("parse file descriptor path %q", fields[1])
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			return nil, fmt.Errorf("parse file descriptor PID %q: %w", pidText, err)
		}
		if pid < 1 {
			return nil, fmt.Errorf("file descriptor PID must be positive, got %d", pid)
		}
		counts[pid] = count
	}
	return counts, nil
}
