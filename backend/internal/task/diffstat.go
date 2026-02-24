package task

import (
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ParseDiffNumstat parses git diff --numstat output into a DiffStat.
// Each line has the format: <added>\t<deleted>\t<path>.
// Binary files use "-\t-\t<path>".
// Returns nil if there are no changed files.
func ParseDiffNumstat(numstat string) agent.DiffStat {
	numstat = strings.TrimSpace(numstat)
	if numstat == "" {
		return nil
	}
	var files agent.DiffStat
	for line := range strings.SplitSeq(numstat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fs := agent.DiffFileStat{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			fs.Binary = true
		} else {
			fs.Added, _ = strconv.Atoi(parts[0])
			fs.Deleted, _ = strconv.Atoi(parts[1])
		}
		files = append(files, fs)
	}
	return files
}
