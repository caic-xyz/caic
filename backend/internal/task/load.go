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
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentclaude "github.com/caic-xyz/caic/backend/internal/agent/claude"
	agentcodex "github.com/caic-xyz/caic/backend/internal/agent/codex"
	agentgemini "github.com/caic-xyz/caic/backend/internal/agent/gemini"
	agentkilo "github.com/caic-xyz/caic/backend/internal/agent/kilo"
	agentopencode "github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// metaKnown is the set of JSON field names recognised by agent.MetaMessage.
var metaKnown = jsonutil.KnownFields(agent.MetaMessage{})

// resultKnown is the set of JSON field names recognised by agent.MetaResultMessage.
var resultKnown = jsonutil.KnownFields(agent.MetaResultMessage{})

// LoadedTask holds the data reconstructed from a single JSONL log file.
type LoadedTask struct {
	TaskID            string // Task ID parsed from log filename; empty if unparseable.
	Prompt            string
	Title             string
	Repos             []RepoMount // GitRoot will be empty for purged tasks loaded from logs.
	Harness           agent.Harness
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Latest relay ts from caic_diff_stat records, falling back to log file mtime.
	State             State
	ForgeIssue        int // Originating issue number for bot comment callbacks.
	ForgeOwner        string
	ForgeRepo         string
	ForgePR           int // PR number created during the task; 0 if none.
	Tailscale         bool
	USB               bool
	Display           bool
	Msgs              []agent.Message
	Result            *Result

	path string // Absolute path for lazy message loading via LoadMessages.
}

// Primary returns a pointer to the primary RepoMount (Repos[0]), or nil for no-repo tasks.
func (lt *LoadedTask) Primary() *RepoMount {
	if len(lt.Repos) == 0 {
		return nil
	}
	return &lt.Repos[0]
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
	if full.ForgePR > 0 {
		lt.ForgeOwner = full.ForgeOwner
		lt.ForgeRepo = full.ForgeRepo
		lt.ForgePR = full.ForgePR
	}
	return nil
}

// unmarshalMeta decodes a MetaMessage from JSON and warns about any unrecognised
// fields (e.g. fields from an older log format that have since been removed).
func unmarshalMeta(data []byte, m *agent.MetaMessage) error {
	if err := json.Unmarshal(data, m); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		jsonutil.WarnUnknown("caic_meta", jsonutil.CollectUnknown(raw, metaKnown))
	}
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
	if err := unmarshalMeta(scanner.Bytes(), &meta); err != nil {
		return nil, errNotLogFile
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Parse task ID from filename: "<taskID>-<safeRepo>-<safeBranch>.jsonl".
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	taskIDStr := base
	if i := strings.IndexByte(base, '-'); i >= 0 {
		taskIDStr = base[:i]
	}

	repos := make([]RepoMount, len(meta.Repos))
	for i, mr := range meta.Repos {
		repos[i] = RepoMount{Name: mr.Name, BaseBranch: mr.BaseBranch, Branch: mr.Branch}
	}
	lt := &LoadedTask{
		path:              path,
		TaskID:            taskIDStr,
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repos:             repos,
		Harness:           meta.Harness,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: info.ModTime().UTC(),
		State:             StateRunning, // sentinel: overridden by caic_result trailer or loadPurgedTasksFrom
		ForgeIssue:        meta.ForgeIssue,
		Tailscale:         meta.Tailscale,
		USB:               meta.USB,
		Display:           meta.Display,
	}

	// Read the tail of the file to find caic_pr, caic_result, and
	// caic_diff_stat records. The latest caic_diff_stat "ts" field provides
	// a more accurate LastStateUpdateAt than file mtime.
	const tailSize = 65536 // 64 KiB — sufficient for any realistic trailer.
	size := info.Size()
	offset := max(int64(0), size-tailSize)
	buf := make([]byte, size-offset)
	n, _ := f.ReadAt(buf, offset)
	if n > 0 {
		for _, line := range bytes.Split(buf[:n], []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if bytes.Contains(line, []byte(`"caic_pr"`)) {
				var mp agent.MetaPRMessage
				if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
					lt.ForgeOwner = mp.ForgeOwner
					lt.ForgeRepo = mp.ForgeRepo
					lt.ForgePR = mp.ForgePR
				}
			}
			if bytes.Contains(line, []byte(`"caic_diff_stat"`)) {
				var ds agent.DiffStatMessage
				if json.Unmarshal(line, &ds) == nil && ds.Ts > 0 {
					if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
						lt.LastStateUpdateAt = t
					}
				}
			}
			if bytes.Contains(line, []byte(`"caic_result"`)) {
				var mr agent.MetaResultMessage
				if err := json.Unmarshal(line, &mr); err == nil {
					var raw map[string]json.RawMessage
					if json.Unmarshal(line, &raw) == nil {
						jsonutil.WarnUnknown("caic_result", jsonutil.CollectUnknown(raw, resultKnown))
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
	if err := unmarshalMeta(scanner.Bytes(), &meta); err != nil {
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

	repos := make([]RepoMount, len(meta.Repos))
	for i, mr := range meta.Repos {
		repos[i] = RepoMount{Name: mr.Name, BaseBranch: mr.BaseBranch, Branch: mr.Branch}
	}
	lt := &LoadedTask{
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repos:             repos,
		Harness:           meta.Harness,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: mtime,
		State:             StateRunning, // sentinel: overridden by caic_result trailer or loadPurgedTasksFrom
		ForgeIssue:        meta.ForgeIssue,
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

		if envelope.Type == "caic_pr" {
			var mp agent.MetaPRMessage
			if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
				lt.ForgeOwner = mp.ForgeOwner
				lt.ForgeRepo = mp.ForgeRepo
				lt.ForgePR = mp.ForgePR
			}
			continue
		}

		if envelope.Type == "caic_diff_stat" {
			var ds agent.DiffStatMessage
			if json.Unmarshal(line, &ds) == nil && ds.Ts > 0 {
				if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
					lt.LastStateUpdateAt = t
				}
			}
			// Also parse as a regular message so it appears in lt.Msgs.
		}

		if envelope.Type == "caic_result" {
			var mr agent.MetaResultMessage
			if err := json.Unmarshal(line, &mr); err != nil {
				return nil, fmt.Errorf("invalid caic_result: %w", err)
			}
			var raw map[string]json.RawMessage
			if json.Unmarshal(line, &raw) == nil {
				jsonutil.WarnUnknown("caic_result", jsonutil.CollectUnknown(raw, resultKnown))
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
	case agent.Kilo:
		return agentkilo.ParseMessage
	case agent.OpenCode:
		return agentopencode.ParseMessage
	default:
		return agentclaude.ParseMessage
	}
}

// tsToTime converts a Unix epoch float64 (seconds with sub-second precision)
// to a time.Time in UTC.
func tsToTime(ts float64) time.Time {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// parseState converts a state string back to a State value.
func parseState(s string) State {
	switch s {
	case "failed":
		return StateFailed
	case "purged", "terminated": // "terminated" is for backward compat with pre-rename logs; remove once old logs age out
		return StatePurged
	default:
		return StateFailed
	}
}
