package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
)

// errNotLogFile is returned when a file doesn't contain a valid wmao_meta header.
var errNotLogFile = errors.New("not a wmao log file")

// LoadedTask holds the data reconstructed from a single JSONL log file.
type LoadedTask struct {
	Prompt            string
	Repo              string
	Branch            string
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Derived from log file mtime; best-effort for adopt.
	State             State
	Msgs              []agent.Message
	Result            *Result
}

// LoadLogs scans logDir for *.jsonl files and reconstructs completed tasks.
// Files without a valid wmao_meta header line are skipped. Returns tasks
// sorted by StartedAt ascending.
func LoadLogs(logDir string) ([]*LoadedTask, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var tasks []*LoadedTask
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		lt, err := loadLogFile(filepath.Join(logDir, e.Name()))
		if err != nil {
			if !errors.Is(err, errNotLogFile) {
				slog.Warn("skipping log file", "file", e.Name(), "err", err)
			}
			continue
		}
		tasks = append(tasks, lt)
	}

	slices.SortFunc(tasks, func(a, b *LoadedTask) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return tasks, nil
}

// LoadTerminated returns the last n tasks in a terminal state (failed, terminated)
// from logDir, sorted by StartedAt descending (most recent first).
// Returns nil when logDir is empty or no terminated tasks exist.
func LoadTerminated(logDir string, n int) []*LoadedTask {
	if logDir == "" || n <= 0 {
		return nil
	}
	all, err := LoadLogs(logDir)
	if err != nil {
		slog.Warn("failed to load logs for terminated tasks", "err", err)
		return nil
	}
	var terminated []*LoadedTask
	for _, lt := range all {
		// Only include tasks with an explicit wmao_result trailer.
		// Log files without a trailer may belong to still-running tasks
		// whose default state is StateFailed.
		if lt.Result != nil {
			terminated = append(terminated, lt)
		}
	}
	// LoadLogs returns ascending; reverse for most-recent-first.
	slices.Reverse(terminated)
	if len(terminated) > n {
		terminated = terminated[:n]
	}
	return terminated
}

// loadLogFile parses a single JSONL log file. Returns nil if the file has no
// valid wmao_meta header.
func loadLogFile(path string) (_ *LoadedTask, retErr error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	// First line must be the metadata header.
	if !scanner.Scan() {
		return nil, errNotLogFile
	}
	var meta agent.MetaMessage
	d := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
	d.DisallowUnknownFields()
	if err := d.Decode(&meta); err != nil {
		return nil, errNotLogFile
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	// Use the file modification time as a best-effort approximation of the
	// last state change (the file is written to as messages arrive).
	var mtime time.Time
	if info, err := f.Stat(); err == nil {
		mtime = info.ModTime().UTC()
	}

	lt := &LoadedTask{
		Prompt:            meta.Prompt,
		Repo:              meta.Repo,
		Branch:            meta.Branch,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: mtime,
		State:             StateFailed, // default if no trailer
	}

	// Parse remaining lines as agent messages or the result trailer.
	var envelope struct {
		Type string `json:"type"`
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}

		if envelope.Type == "wmao_result" {
			var mr agent.MetaResultMessage
			rd := json.NewDecoder(bytes.NewReader(line))
			rd.DisallowUnknownFields()
			if err := rd.Decode(&mr); err != nil {
				return nil, fmt.Errorf("invalid wmao_result: %w", err)
			}
			lt.State = parseState(mr.State)
			lt.Result = &Result{
				Task:        lt.Prompt,
				Repo:        lt.Repo,
				Branch:      lt.Branch,
				State:       lt.State,
				CostUSD:     mr.CostUSD,
				DurationMs:  mr.DurationMs,
				NumTurns:    mr.NumTurns,
				DiffStat:    mr.DiffStat,
				AgentResult: mr.AgentResult,
			}
			if mr.Error != "" {
				lt.Result.Err = errors.New(mr.Error)
			}
			continue
		}

		// Parse as a regular agent message.
		msg, err := agent.ParseMessage(line)
		if err != nil {
			continue
		}
		lt.Msgs = append(lt.Msgs, msg)
	}

	return lt, scanner.Err()
}

// LoadBranchLogs loads all JSONL log files for the given branch from logDir,
// returning messages from all sessions concatenated chronologically. Returns
// nil when logDir is empty, no matching files exist, or on read errors.
func LoadBranchLogs(logDir, branch string) *LoadedTask {
	if logDir == "" {
		return nil
	}
	all, err := LoadLogs(logDir)
	if err != nil {
		return nil
	}

	var merged *LoadedTask
	for _, lt := range all {
		if lt.Branch != branch {
			continue
		}
		if merged == nil {
			merged = lt
		} else {
			merged.Msgs = append(merged.Msgs, lt.Msgs...)
			// Later sessions are authoritative for prompt and metadata.
			if lt.Prompt != "" {
				merged.Prompt = lt.Prompt
			}
			if !lt.StartedAt.IsZero() {
				merged.StartedAt = lt.StartedAt
			}
			if !lt.LastStateUpdateAt.IsZero() {
				merged.LastStateUpdateAt = lt.LastStateUpdateAt
			}
			if lt.Result != nil {
				merged.Result = lt.Result
				merged.State = lt.State
			}
		}
	}
	return merged
}

// parseState converts a state string back to a State value.
func parseState(s string) State {
	switch s {
	case "failed":
		return StateFailed
	case "terminated":
		return StateTerminated
	default:
		return StateFailed
	}
}
