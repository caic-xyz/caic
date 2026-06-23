// Process list parsing for md-backed runtime instances.

package mdruntime

import (
	"slices"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// parsePSOutput parses the output of ps with the columns above. The last
// column (args) may contain spaces; we split by whitespace for the first 7
// fields and treat the remainder as the command.
func parsePSOutput(out string) ([]runtime.ProcessInfo, error) {
	var procs []runtime.ProcessInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 8)
		var clean []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) < 8 {
			clean = strings.Fields(line)
			if len(clean) < 8 {
				continue
			}
			cmd := strings.Join(clean[7:], " ")
			clean = append(clean[:7], cmd)
		}
		pid, err := strconv.Atoi(clean[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(clean[1])
		cpu, _ := strconv.ParseFloat(clean[4], 64)
		mem, _ := strconv.ParseFloat(clean[5], 64)
		procs = append(procs, runtime.ProcessInfo{
			PID:     pid,
			PPID:    ppid,
			User:    clean[2],
			State:   clean[3],
			CPU:     cpu,
			Mem:     mem,
			Time:    clean[6],
			Command: clean[7],
		})
	}
	// Filter out the ps process itself.
	procs = slices.DeleteFunc(procs, func(p runtime.ProcessInfo) bool {
		return strings.HasPrefix(p.Command, "ps ")
	})
	return procs, nil
}
