package task

import (
	"bufio"
	"encoding/json"
	"errors"
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
	Prompt    string
	Repo      string
	Branch    string
	StartedAt time.Time
	State     State
	Msgs      []agent.Message
	Result    *Result
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
	line := scanner.Bytes()

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, errNotLogFile
	}
	if envelope.Type != "wmao_meta" {
		return nil, errNotLogFile
	}

	var meta agent.MetaMessage
	if err := json.Unmarshal(line, &meta); err != nil {
		return nil, errNotLogFile
	}

	lt := &LoadedTask{
		Prompt:    meta.Prompt,
		Repo:      meta.Repo,
		Branch:    meta.Branch,
		StartedAt: meta.StartedAt,
		State:     StateFailed, // default if no trailer
	}

	// Parse remaining lines as agent messages or the result trailer.
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
			if err := json.Unmarshal(line, &mr); err != nil {
				continue
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
				lt.Result.Err = stringError(mr.Error)
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

// parseState converts a state string back to a State value.
func parseState(s string) State {
	switch s {
	case "done":
		return StateDone
	case "failed":
		return StateFailed
	case "ended":
		return StateEnded
	default:
		return StateFailed
	}
}

// stringError is a simple error type that holds a string.
type stringError string

func (e stringError) Error() string { return string(e) }
