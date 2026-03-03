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
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentclaude "github.com/caic-xyz/caic/backend/internal/agent/claude"
	agentcodex "github.com/caic-xyz/caic/backend/internal/agent/codex"
	agentgemini "github.com/caic-xyz/caic/backend/internal/agent/gemini"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// LoadedTask holds the data reconstructed from a single JSONL log file.
type LoadedTask struct {
	Prompt            string
	Title             string
	Repo              string
	Branch            string
	Harness           agent.Harness
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Derived from log file mtime; best-effort for adopt.
	State             State
	Msgs              []agent.Message
	Result            *Result

	path string // Absolute path for lazy message loading via LoadMessages.
}

// LoadLogs scans logDir for *.jsonl files and loads task metadata.
// Only the header (first line) and result trailer (last line) are parsed;
// individual messages are NOT loaded. Call LoadMessages on specific tasks
// that need their conversation history. Returns one LoadedTask per file,
// sorted by StartedAt ascending.
func LoadLogs(logDir string) ([]*LoadedTask, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Filter to .jsonl files.
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			paths = append(paths, filepath.Join(logDir, e.Name()))
		}
	}

	// Parse headers in parallel — each file is independent.
	type result struct {
		lt  *LoadedTask
		err error
	}
	results := make([]result, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lt, err := loadLogHeader(p)
			results[i] = result{lt, err}
		}()
	}
	wg.Wait()

	var tasks []*LoadedTask
	for i, r := range results {
		if r.err != nil {
			if !errors.Is(r.err, errNotLogFile) {
				slog.Warn("skipping log file", "file", filepath.Base(paths[i]), "err", r.err)
			}
			continue
		}
		tasks = append(tasks, r.lt)
	}

	slices.SortFunc(tasks, func(a, b *LoadedTask) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return tasks, nil
}

// LoadMessages lazily loads the full conversation messages from the log file.
// This is a no-op if messages are already loaded.
func (lt *LoadedTask) LoadMessages() error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	full, err := loadLogFile(lt.path)
	if err != nil {
		return err
	}
	lt.Msgs = full.Msgs
	return nil
}

// loadLogHeader reads only the metadata header (first line) and the result
// trailer (last line) from a JSONL log file. It does NOT parse individual
// messages — call LoadMessages for that. The path is stored for lazy loading.
func loadLogHeader(path string) (_ *LoadedTask, retErr error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	// Read first line: metadata header.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 32<<20)
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

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	lt := &LoadedTask{
		path:              path,
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repo:              meta.Repo,
		Branch:            meta.Branch,
		Harness:           meta.Harness,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: info.ModTime().UTC(),
		State:             StateFailed, // default if no trailer
	}

	// Read the tail of the file to find a caic_result trailer.
	const tailSize = 65536 // 64 KiB — sufficient for any realistic trailer.
	size := info.Size()
	offset := max(int64(0), size-tailSize)
	buf := make([]byte, size-offset)
	n, _ := f.ReadAt(buf, offset)
	if n > 0 {
		// Find the last non-empty line.
		tail := bytes.TrimRight(buf[:n], "\n\r\t ")
		if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
		if bytes.Contains(tail, []byte(`"caic_result"`)) {
			var mr agent.MetaResultMessage
			rd := json.NewDecoder(bytes.NewReader(tail))
			rd.DisallowUnknownFields()
			if err := rd.Decode(&mr); err == nil {
				lt.State = parseState(mr.State)
				if mr.Title != "" {
					lt.Title = mr.Title
				}
				lt.Result = &Result{
					State:    lt.State,
					CostUSD:  mr.CostUSD,
					Duration: time.Duration(mr.Duration * float64(time.Second)),
					NumTurns: mr.NumTurns,
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
			}
		}
	}

	return lt, nil
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
	// 32 MiB max line: user input with base64 images can produce very long NDJSON lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

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
		Title:             meta.Title,
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
			if mr.Title != "" {
				lt.Title = mr.Title
			}
			lt.Result = &Result{
				State:    lt.State,
				CostUSD:  mr.CostUSD,
				Duration: time.Duration(mr.Duration * float64(time.Second)),
				NumTurns: mr.NumTurns,
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
		parsed, err := parseFn(line)
		if err != nil {
			continue
		}
		lt.Msgs = append(lt.Msgs, parsed...)
	}

	return lt, scanner.Err()
}

// parseFnForHarness returns the message parser for the given harness.
//
// TODO: This is a layering violation, let's fix this eventually.
func parseFnForHarness(h agent.Harness) func([]byte) ([]agent.Message, error) {
	switch h {
	case agent.Codex:
		return agentcodex.ParseMessage
	case agent.Gemini:
		return agentgemini.ParseMessage
	default:
		return agentclaude.ParseMessage
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
