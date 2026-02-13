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

	"github.com/maruel/caic/backend/internal/agent"
	agentgemini "github.com/maruel/caic/backend/internal/agent/gemini"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// LoadedTask holds the data reconstructed from a single JSONL log file.
type LoadedTask struct {
	Prompt            string
	Repo              string
	Branch            string
	Harness           agent.Harness
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Derived from log file mtime; best-effort for adopt.
	State             State
	Msgs              []agent.Message
	Result            *Result
}

// LoadLogs scans logDir for *.jsonl files and reconstructs tasks.
// Files without a valid caic_meta header line are skipped. Returns one
// LoadedTask per file, sorted by StartedAt ascending.
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

// loadLogFile parses a single JSONL log file. Returns nil if the file has no
// valid caic_meta header.
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
		Harness:           meta.Harness,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: mtime,
		State:             StateFailed, // default if no trailer
	}

	parseFn := parseFnForHarness(meta.Harness)

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

		if envelope.Type == "caic_result" {
			var mr agent.MetaResultMessage
			rd := json.NewDecoder(bytes.NewReader(line))
			rd.DisallowUnknownFields()
			if err := rd.Decode(&mr); err != nil {
				return nil, fmt.Errorf("invalid caic_result: %w", err)
			}
			lt.State = parseState(mr.State)
			lt.Result = &Result{
				Task:       lt.Prompt,
				Repo:       lt.Repo,
				Branch:     lt.Branch,
				State:      lt.State,
				CostUSD:    mr.CostUSD,
				DurationMs: mr.DurationMs,
				NumTurns:   mr.NumTurns,
				Usage: agent.Usage{
					InputTokens:              mr.InputTokens,
					OutputTokens:             mr.OutputTokens,
					CacheCreationInputTokens: mr.CacheCreationInputTokens,
					CacheReadInputTokens:     mr.CacheReadInputTokens,
				},
				DiffStat:    mr.DiffStat,
				AgentResult: mr.AgentResult,
			}
			if mr.Error != "" {
				lt.Result.Err = errors.New(mr.Error)
			}
			continue
		}

		// Parse as a regular agent message using the harness-specific parser.
		msg, err := parseFn(line)
		if err != nil {
			continue
		}
		lt.Msgs = append(lt.Msgs, msg)
	}

	return lt, scanner.Err()
}

// parseFnForHarness returns the message parser for the given harness.
func parseFnForHarness(h agent.Harness) func([]byte) (agent.Message, error) {
	switch h {
	case agent.Gemini:
		return agentgemini.ParseMessage
	default:
		return agent.ParseMessage
	}
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
