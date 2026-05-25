// Loads task definitions from disk and resolves their configurations.

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
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// errNotLogFile is returned when a file doesn't contain a valid caic_meta header.
var errNotLogFile = errors.New("not a caic log file")

// metaKnown is the set of JSON field names recognised by agent.MetaMessage.
var metaKnown = jsonutil.KnownFields(agent.MetaMessage{})

// resultKnown is the set of JSON field names recognised by agent.MetaResultMessage.
var resultKnown = jsonutil.KnownFields(agent.MetaResultMessage{})

// typeEnvelope extracts just the "type" field from a JSON line.
type typeEnvelope struct {
	Type string `json:"type"`
}

// tailInit is the subset of a system/init message parsed from the tail scan.
type tailInit struct {
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Version string `json:"claude_code_version"`
}

// tailResult is the subset of a result message parsed from the tail scan.
type tailResult struct {
	TotalCostUSD float64     `json:"total_cost_usd"`
	DurationMs   int64       `json:"duration_ms"`
	NumTurns     int         `json:"num_turns"`
	Usage        agent.Usage `json:"usage"`
}

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
	GitHubToken       bool
	Model             string
	AgentVersion      string
	Msgs              []agent.Message
	Result            *Result

	path    string                                // Absolute path for lazy message loading via LoadMessages.
	parseFn func([]byte) ([]agent.Message, error) // Parser for this harness; set by LoadLogs.
}

// Primary returns a pointer to the primary RepoMount (Repos[0]), or nil for no-repo tasks.
func (lt *LoadedTask) Primary() *RepoMount {
	if len(lt.Repos) == 0 {
		return nil
	}
	return &lt.Repos[0]
}

// LoadLogs scans logDir for *.jsonl files and loads task metadata.
// Only the header and result trailer are parsed; call LoadMessages for
// full conversation history. Call SetParser on each task before LoadMessages.
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
		wg.Go(func() {
			lt, err := loadLogHeader(p)
			results[i] = result{lt, err}
		})
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

// SetParser sets the parse function for lazy message loading.
func (lt *LoadedTask) SetParser(fn func([]byte) ([]agent.Message, error)) {
	lt.parseFn = fn
}

// LoadMessages lazily loads the full conversation messages from the log file.
// This is a no-op if messages are already loaded. Requires parseFn to be set
// via LoadLogs backends or SetParser.
func (lt *LoadedTask) LoadMessages() error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if lt.parseFn == nil {
		return fmt.Errorf("no parser set for harness %q; call SetParser first", lt.Harness)
	}
	full, err := loadLogFile(lt.path, lt.parseFn)
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
func unmarshalMeta(data []byte, m *agent.MetaMessage, fw *jsonutil.FieldWarner) error {
	if err := json.Unmarshal(data, m); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		fw.Warn("caic_meta", jsonutil.CollectUnknown(raw, metaKnown))
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
	fw := &jsonutil.FieldWarner{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 32<<20)
	if !scanner.Scan() {
		return nil, errNotLogFile
	}
	var meta agent.MetaMessage
	if err := unmarshalMeta(scanner.Bytes(), &meta, fw); err != nil {
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
	if before, _, ok := strings.Cut(base, "-"); ok {
		taskIDStr = before
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
		GitHubToken:       meta.GitHubToken,
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
		// Track the last ResultMessage stats from the tail for backfill
		// when the trailer has zero cost (session exited without final ResultMessage).
		var lastResultCostUSD float64
		var lastResultDuration time.Duration
		var lastResultNumTurns int
		var lastResultUsage agent.Usage
		for line := range bytes.SplitSeq(buf[:n], []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			// Match caic-internal messages by type field, not substring,
			// to avoid false positives from harness messages.
			var typeEnv typeEnvelope
			if json.Unmarshal(line, &typeEnv) == nil {
				switch typeEnv.Type {
				case "caic_pr":
					var mp agent.MetaPRMessage
					if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
						lt.ForgeOwner = mp.ForgeOwner
						lt.ForgeRepo = mp.ForgeRepo
						lt.ForgePR = mp.ForgePR
					}
				case "caic_diff_stat":
					var ds agent.DiffStatMessage
					if json.Unmarshal(line, &ds) == nil && ds.Ts > 0 {
						if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
							lt.LastStateUpdateAt = t
						}
					}
				case "caic_result":
					var mr agent.MetaResultMessage
					if err := json.Unmarshal(line, &mr); err == nil {
						var raw map[string]json.RawMessage
						if json.Unmarshal(line, &raw) == nil {
							fw.Warn("caic_result", jsonutil.CollectUnknown(raw, resultKnown))
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
				case "system":
					// Extract model and version from system/init messages
					// in the tail. For long logs the init may be outside
					// the 64 KiB window — model/version will be empty.
					var sm tailInit
					if json.Unmarshal(line, &sm) == nil && sm.Subtype == "init" {
						if sm.Model != "" {
							lt.Model = sm.Model
						}
						// TODO: This is a claudecode hack.
						if sm.Version != "" {
							lt.AgentVersion = sm.Version
						}
					}
				case "result":
					// Track the last ResultMessage for backfill when
					// the caic_result trailer has zero cost.
					var rm tailResult
					// TODO: This is a claudecode hack.
					if json.Unmarshal(line, &rm) == nil {
						lastResultCostUSD = rm.TotalCostUSD
						lastResultDuration = time.Duration(rm.DurationMs) * time.Millisecond
						lastResultNumTurns = rm.NumTurns
						lastResultUsage = rm.Usage
					}
				}
			}
		}
		// Backfill trailer result from last ResultMessage when the trailer
		// has zero cost (session exited without a final ResultMessage).
		if lt.Result != nil && lt.Result.CostUSD == 0 && lastResultCostUSD > 0 {
			lt.Result.CostUSD = lastResultCostUSD
			lt.Result.Duration = lastResultDuration
			lt.Result.NumTurns = lastResultNumTurns
			lt.Result.Usage = lastResultUsage
		}
	}

	return lt, nil
}

// loadLogFile parses a single JSONL log file. Returns nil if the file has no
// valid caic_meta header.
func loadLogFile(path string, parseFn func([]byte) ([]agent.Message, error)) (_ *LoadedTask, retErr error) {
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

	fw := &jsonutil.FieldWarner{}

	// First line must be the metadata header.
	if !scanner.Scan() {
		return nil, errNotLogFile
	}
	var meta agent.MetaMessage
	if err := unmarshalMeta(scanner.Bytes(), &meta, fw); err != nil {
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

	// Parse remaining lines as agent messages or the result trailer.
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// This confirms the line must be a dict but it's okay to not contain a `type` field.
		var envelope typeEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}

		switch envelope.Type {
		case "caic_pr":
			var mp agent.MetaPRMessage
			if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
				lt.ForgeOwner = mp.ForgeOwner
				lt.ForgeRepo = mp.ForgeRepo
				lt.ForgePR = mp.ForgePR
			}

		case "caic_diff_stat":
			var ds agent.DiffStatMessage
			if json.Unmarshal(line, &ds) == nil && ds.Ts > 0 {
				if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
					lt.LastStateUpdateAt = t
				}
			}

		case "caic_result":
			var mr agent.MetaResultMessage
			if err := json.Unmarshal(line, &mr); err != nil {
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

		default:
			parsed, err := parseFn(line)
			if err != nil {
				slog.Warn("failed to parse message", "err", err, "path", path)
				continue
			}
			lt.Msgs = append(lt.Msgs, parsed...)
		}
	}

	return lt, scanner.Err()
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
	case "stopped":
		return StateStopped
	case "purged", "terminated": // "terminated" is for backward compat with pre-rename logs; remove once old logs age out
		return StatePurged
	default:
		return StateFailed
	}
}
