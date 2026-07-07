// Loads task definitions from disk and resolves their configurations.

package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
	"github.com/caic-xyz/caic/backend/internal/runtime"
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

func decodeTypeEnvelope(line []byte) (typeEnvelope, bool) {
	var env typeEnvelope
	return env, json.Unmarshal(line, &env) == nil
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

type logTailScan struct {
	fw *jsonutil.FieldWarner

	lastResultCostUSD  float64
	lastResultDuration time.Duration
	lastResultNumTurns int
	lastResultUsage    agent.Usage
}

func applyMetaResult(lt *LoadedTask, mr *agent.MetaResultMessage) {
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
			ReasoningOutputTokens:    mr.ReasoningOutputTokens,
		},
		DiffStat:    mr.DiffStat,
		AgentResult: mr.AgentResult,
	}
	if len(mr.DiffStat) > 0 {
		lt.DiffCreated = true
	}
	if mr.Error != "" {
		lt.Result.Err = errors.New(mr.Error)
	}
}

// noteDiffStatLine updates lt from a caic_diff_stat record: it marks DiffCreated
// when the diff is non-empty (sticky, so a later empty diff never clears it) and
// advances LastStateUpdateAt from the relay timestamp.
func noteDiffStatLine(lt *LoadedTask, line []byte) {
	var ds agent.DiffStatMessage
	if json.Unmarshal(line, &ds) != nil {
		return
	}
	if len(ds.DiffStat) > 0 {
		lt.DiffCreated = true
	}
	if ds.Ts > 0 {
		if t := tsToTime(ds.Ts); t.After(lt.LastStateUpdateAt) {
			lt.LastStateUpdateAt = t
		}
	}
}

// TODO: Trim legacyCaicInit after 2026-08 once legacy caic_init logs are old enough to ignore.
// legacyCaicInit is the OpenCode pre-caic_session session metadata record.
type legacyCaicInit struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Version   string `json:"version"`
}

// LoadedTask holds the data reconstructed from a single task log file.
type LoadedTask struct {
	TaskID            string // Task ID parsed from log filename; empty if unparseable.
	Prompt            string
	Title             string
	Repos             []RepoMount // GitRoot will be empty for purged tasks loaded from logs.
	Harness           harness.Name
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
	Sudo              bool
	GitHubToken       bool
	BaseImage         string
	ContainerPlatform string
	MaxCPUs           int
	CacheMounts       []runtime.CacheMount
	Mounts            []runtime.Mount
	Model             string
	Effort            string
	SessionID         string // Backend-native session/thread ID required to resume stateful harnesses.
	AgentVersion      string
	LogSize           int64 // Byte size of the log file on disk; populated by LoadLogs.
	DiffCreated       bool  // True if any non-empty diff was recorded in the log; sticky across the run.
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

// LogPath returns the absolute task log path used to load the task.
func (lt *LoadedTask) LogPath() string {
	return lt.path
}

func loadedTaskFromMeta(path, taskID string, meta *agent.MetaMessage, modified time.Time, size int64) *LoadedTask {
	repos := make([]RepoMount, len(meta.Repos))
	for i, mr := range meta.Repos {
		repos[i] = RepoMountFromMeta(mr, "")
	}
	return &LoadedTask{
		path:              path,
		TaskID:            taskID,
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repos:             repos,
		Harness:           meta.Harness,
		Model:             meta.Model,
		Effort:            meta.Effort,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: modified,
		State:             StateRunning,
		ForgeIssue:        meta.ForgeIssue,
		Tailscale:         meta.Tailscale,
		USB:               meta.USB,
		Display:           meta.Display,
		Sudo:              meta.Sudo,
		GitHubToken:       meta.GitHubToken,
		BaseImage:         meta.BaseImage,
		ContainerPlatform: meta.ContainerPlatform,
		MaxCPUs:           meta.MaxCPUs,
		CacheMounts:       runtimeCacheMountsFromMeta(meta.CacheMounts),
		Mounts:            runtimeMountsFromMeta(meta.Mounts),
		LogSize:           size,
	}
}

// LoadLogs scans logDir for task log files and loads task metadata.
// Only the header and result trailer are parsed; call LoadMessages for
// full conversation history. Call SetParser on each task before LoadMessages.
func LoadLogs(logDir string) ([]*LoadedTask, error) {
	paths, err := logPaths(logDir, nil)
	if err != nil {
		return nil, err
	}
	return loadLogsFromPaths(paths), nil
}

// LoadLogsForTaskIDs loads metadata only for logs whose parsed filename task ID
// matches one of taskIDs. It avoids parsing unrelated purged task logs during
// startup adoption of live runtime instances.
func LoadLogsForTaskIDs(logDir string, taskIDs []string) ([]*LoadedTask, error) {
	ids := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	paths, err := logPaths(logDir, ids)
	if err != nil {
		return nil, err
	}
	return loadLogsFromPaths(paths), nil
}

func logPaths(logDir string, taskIDs map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Filter to task log files. If both plain and compressed logs exist for
	// the same base name, prefer the compressed file.
	pathsByBase := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !IsLogName(e.Name()) {
			continue
		}
		base := trimLogExt(e.Name())
		if taskIDs != nil {
			if _, ok := taskIDs[taskIDFromLogBase(base)]; !ok {
				continue
			}
		}
		path := filepath.Join(logDir, e.Name())
		if prev := pathsByBase[base]; prev == "" || isLogCompressed(path) {
			pathsByBase[base] = path
		}
	}
	paths := make([]string, 0, len(pathsByBase))
	for _, p := range pathsByBase {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, nil
}

func loadLogsFromPaths(paths []string) []*LoadedTask {
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
	return tasks
}

func taskIDFromLogBase(base string) string {
	if before, _, ok := strings.Cut(base, "-"); ok {
		return before
	}
	return base
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
	lt.mergeSessionMetadata(full)
	if full.ForgePR > 0 {
		lt.ForgeOwner = full.ForgeOwner
		lt.ForgeRepo = full.ForgeRepo
		lt.ForgePR = full.ForgePR
	}
	return nil
}

// LoadSessionMetadata scans the log for backend-neutral session metadata.
func (lt *LoadedTask) LoadSessionMetadata() error {
	if lt.path == "" || (lt.SessionID != "" && lt.AgentVersion != "") {
		return nil
	}
	full, err := loadLogSessionMetadata(lt.path, lt.parseFn)
	if err != nil {
		return err
	}
	lt.mergeSessionMetadata(full)
	return nil
}

// maxTailLoadBytes is the maximum number of bytes to read from the tail of a
// log file during on-demand loading. Files larger than this are read from the
// tail only, skipping older messages to avoid OOM.
const maxTailLoadBytes = 64 << 20 // 64 MiB

// LoadMessagesTail loads messages from the log file, reading only the tail when
// the log may be large. This is used for on-demand loading to avoid OOM on
// multi-GB log files. Plain logs can seek to the tail; compressed logs are
// scanned once while retaining only the tail window in memory.
func (lt *LoadedTask) LoadMessagesTail() error {
	if lt.Msgs != nil || lt.path == "" {
		return nil
	}
	if lt.parseFn == nil {
		return fmt.Errorf("no parser set for harness %q; call SetParser first", lt.Harness)
	}
	// For small plain files, use the regular full load. Compressed files are
	// measured by compressed bytes on disk, so their decompressed history can be
	// much larger than LogSize; keep them on the bounded tail path.
	if !isLogCompressed(lt.path) && lt.LogSize <= maxTailLoadBytes {
		return lt.LoadMessages()
	}
	slog.Info("load: reading tail only", "path", lt.path, "size", lt.LogSize, "tail", maxTailLoadBytes)
	full, err := loadLogFileTail(lt.path, lt.parseFn, maxTailLoadBytes)
	if err != nil {
		return err
	}
	lt.Msgs = full.Msgs
	lt.Title = full.Title
	lt.State = full.State
	lt.Result = full.Result
	lt.mergeSessionMetadata(full)
	if full.ForgePR > 0 {
		lt.ForgeOwner = full.ForgeOwner
		lt.ForgeRepo = full.ForgeRepo
		lt.ForgePR = full.ForgePR
	}
	if !full.LastStateUpdateAt.IsZero() {
		lt.LastStateUpdateAt = full.LastStateUpdateAt
	}
	return nil
}

// StreamMessages streams the task's conversation messages directly from the log
// file, yielding each in order without materializing them into Msgs. Memory
// usage is O(1) regardless of log size, so it is the preferred path for
// replaying terminal tasks to SSE clients without retaining the full history in
// memory.
func (lt *LoadedTask) StreamMessages() iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		if lt.path == "" {
			return
		}
		if lt.parseFn == nil {
			yield(nil, fmt.Errorf("no parser set for harness %q; call SetParser first", lt.Harness))
			return
		}
		for m, e := range streamLogFile(lt.path, lt.replayParser(), 0) {
			if !yield(m, e) {
				return
			}
		}
	}
}

func (lt *LoadedTask) mergeSessionMetadata(src *LoadedTask) {
	if src == nil {
		return
	}
	if lt.SessionID == "" {
		lt.SessionID = src.SessionID
	}
	if lt.Model == "" {
		lt.Model = src.Model
	}
	if lt.AgentVersion == "" {
		lt.AgentVersion = src.AgentVersion
	}
}

// replayParser adapts the harness parser for replaying a task *log* file
// (which interleaves agent messages with the caic_meta header and caic_*
// control records) rather than raw relay output. It drops the metadata header
// and caic_* control records — none of which are conversation messages — and
// parses everything else with parseFn. This mirrors the line handling in
// loadLogFile.
func (lt *LoadedTask) replayParser() func([]byte) ([]agent.Message, error) {
	return func(line []byte) ([]agent.Message, error) {
		var env typeEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, nil //nolint:nilerr // malformed/non-object lines are skipped, matching loadLogFile.
		}
		switch env.Type {
		case "caic_meta", "caic_pr", "caic_result", "caic_diff_stat", "caic_session", "caic_init", agent.PendingUserActionMessageType:
			return nil, nil
		default:
			return lt.parseFn(line)
		}
	}
}

// streamLogFile streams parsed messages from a plain or compressed task log.
func streamLogFile(path string, parseFn func([]byte) ([]agent.Message, error), offset int64) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		if offset > 0 && isLogCompressed(path) {
			yield(nil, fmt.Errorf("seek compressed log %s to %d: unsupported", path, offset))
			return
		}
		f, err := openLogReader(path)
		if err != nil {
			yield(nil, fmt.Errorf("open %s: %w", path, err))
			return
		}
		defer func() { _ = f.Close() }()
		if offset > 0 {
			sf, ok := f.(io.Seeker)
			if !ok {
				yield(nil, fmt.Errorf("seek %s to %d: unsupported", path, offset))
				return
			}
			if _, err := sf.Seek(offset, io.SeekStart); err != nil {
				yield(nil, fmt.Errorf("seek %s to %d: %w", path, offset, err))
				return
			}
		}
		for m, e := range yieldLogMessages(f, parseFn, offset > 0, path) {
			if !yield(m, e) {
				return
			}
		}
	}
}

// yieldLogMessages scans JSONL records and yields parsed conversation messages.
func yieldLogMessages(r io.Reader, parseFn func([]byte) ([]agent.Message, error), skipFirst bool, src string) iter.Seq2[agent.Message, error] {
	return func(yield func(agent.Message, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
		first := skipFirst
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if first {
				first = false
				continue
			}
			parsed, parseErr := parseFn(line)
			if parseErr != nil {
				slog.Warn("log", "msg", "skipping unparseable output line", "src", src, "err", parseErr)
				continue
			}
			for _, m := range parsed {
				if !yield(m, nil) {
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, err)
		}
	}
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

func applySessionMetadataLine(lt *LoadedTask, typ string, line []byte) bool {
	switch typ {
	case "caic_session":
		var m agent.MetaSessionMessage
		if json.Unmarshal(line, &m) != nil {
			return false
		}
		if m.SessionID != "" {
			lt.SessionID = m.SessionID
		}
		if m.Model != "" {
			lt.Model = m.Model
		}
		if m.AgentVersion != "" {
			lt.AgentVersion = m.AgentVersion
		}
		return m.SessionID != "" || m.AgentVersion != ""
	case "caic_init":
		var m legacyCaicInit
		if json.Unmarshal(line, &m) != nil {
			return false
		}
		if m.SessionID != "" {
			lt.SessionID = m.SessionID
		}
		if m.Model != "" {
			lt.Model = m.Model
		}
		if m.Version != "" {
			lt.AgentVersion = m.Version
		}
		return m.SessionID != "" || m.Version != ""
	default:
		return false
	}
}

func applySessionMetadataMessages(lt *LoadedTask, msgs []agent.Message) bool {
	for _, msg := range msgs {
		init, ok := msg.(*agent.InitMessage)
		if !ok {
			continue
		}
		if init.SessionID != "" {
			lt.SessionID = init.SessionID
		}
		if init.Model != "" {
			lt.Model = init.Model
		}
		if init.Version != "" {
			lt.AgentVersion = init.Version
		}
		return init.SessionID != "" || init.Version != ""
	}
	return false
}

// maybeLogTailRecord cheaply filters ordinary conversation lines before JSON decoding.
func maybeLogTailRecord(line []byte) bool {
	return bytes.Contains(line, []byte(`"caic_`)) ||
		bytes.Contains(line, []byte(`"system"`)) ||
		bytes.Contains(line, []byte(`"result"`))
}

func (s *logTailScan) apply(lt *LoadedTask, line []byte) {
	if !maybeLogTailRecord(line) {
		return
	}
	var typeEnv typeEnvelope
	if json.Unmarshal(line, &typeEnv) != nil {
		return
	}
	switch typeEnv.Type {
	case "caic_session", "caic_init":
		applySessionMetadataLine(lt, typeEnv.Type, line)
	case "caic_pr":
		var mp agent.MetaPRMessage
		if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
			lt.ForgeOwner = mp.ForgeOwner
			lt.ForgeRepo = mp.ForgeRepo
			lt.ForgePR = mp.ForgePR
		}
	case "caic_diff_stat":
		noteDiffStatLine(lt, line)
	case "caic_result":
		var mr agent.MetaResultMessage
		if err := json.Unmarshal(line, &mr); err == nil {
			var raw map[string]json.RawMessage
			if s.fw != nil && json.Unmarshal(line, &raw) == nil {
				s.fw.Warn("caic_result", jsonutil.CollectUnknown(raw, resultKnown))
			}
			applyMetaResult(lt, &mr)
		}
	case "system":
		var sm tailInit
		if json.Unmarshal(line, &sm) == nil && sm.Subtype == "init" {
			if sm.Model != "" {
				lt.Model = sm.Model
			}
			if sm.Version != "" {
				lt.AgentVersion = sm.Version
			}
		}
	case "result":
		var rm tailResult
		if json.Unmarshal(line, &rm) == nil {
			s.lastResultCostUSD = rm.TotalCostUSD
			s.lastResultDuration = time.Duration(rm.DurationMs) * time.Millisecond
			s.lastResultNumTurns = rm.NumTurns
			s.lastResultUsage = rm.Usage
		}
	}
}

func (s *logTailScan) finish(lt *LoadedTask) {
	if lt.Result != nil && lt.Result.CostUSD == 0 && s.lastResultCostUSD > 0 {
		lt.Result.CostUSD = s.lastResultCostUSD
		lt.Result.Duration = s.lastResultDuration
		lt.Result.NumTurns = s.lastResultNumTurns
		lt.Result.Usage = s.lastResultUsage
	}
}

func loadLogSessionMetadata(path string, parseFn func([]byte) ([]agent.Message, error)) (_ *LoadedTask, retErr error) {
	f, err := openLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 32<<20)
	lt := &LoadedTask{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env typeEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if applySessionMetadataLine(lt, env.Type, line) {
			break
		}
		if parseFn == nil {
			continue
		}
		msgs, err := parseFn(line)
		if err != nil {
			continue
		}
		if applySessionMetadataMessages(lt, msgs) {
			break
		}
	}
	return lt, scanner.Err()
}

// loadLogHeader reads the metadata header and result trailer from a task log.
// It does NOT parse individual messages — call LoadMessages for that. The path
// is stored for lazy loading.
func loadLogHeader(path string) (_ *LoadedTask, retErr error) {
	if isLogCompressed(path) {
		return loadCompressedLogHeader(path)
	}
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

	// Parse task ID from filename: "<taskID>-<safeRepo>-<safeBranch>.jsonl[.zst]".
	base := trimLogExt(filepath.Base(path))
	taskIDStr := taskIDFromLogBase(base)

	lt := loadedTaskFromMeta(path, taskIDStr, &meta, info.ModTime().UTC(), info.Size())

	// Read the tail of the file to find caic_pr, caic_result, and
	// caic_diff_stat records. The latest caic_diff_stat "ts" field provides
	// a more accurate LastStateUpdateAt than file mtime.
	const tailSize = 65536 // 64 KiB — sufficient for any realistic trailer.
	size := info.Size()
	offset := max(int64(0), size-tailSize)
	buf := make([]byte, size-offset)
	n, _ := f.ReadAt(buf, offset)
	if n > 0 {
		scan := logTailScan{fw: fw}
		for line := range bytes.SplitSeq(buf[:n], []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			scan.apply(lt, line)
		}
		scan.finish(lt)
	}

	return lt, nil
}

// loadCompressedLogHeader reads compressed metadata and trailers sequentially.
//
// Plain logs can seek near EOF and scan only the tail for caic_result, caic_pr,
// and caic_diff_stat records. Zstd logs do not support that random access in
// this format, so compressed headers use one streaming pass instead.
func loadCompressedLogHeader(path string) (_ *LoadedTask, retErr error) {
	if lt, ok := loadLogSummary(path); ok {
		return lt, nil
	}

	f, err := openLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return nil, err
	}

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

	base := trimLogExt(filepath.Base(path))
	taskIDStr := taskIDFromLogBase(base)

	lt := loadedTaskFromMeta(path, taskIDStr, &meta, info.ModTime().UTC(), info.Size())

	scan := logTailScan{fw: fw}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		scan.apply(lt, line)
	}
	scan.finish(lt)
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := storeLogSummary(lt); err != nil {
		slog.Warn("task log summary: write failed", "path", path, "err", err)
	}
	return lt, nil
}

// loadLogFile parses a single task log file and requires a message parser.
// Returns nil if the file has no valid caic_meta header.
func loadLogFile(path string, parseFn func([]byte) ([]agent.Message, error)) (_ *LoadedTask, retErr error) {
	if parseFn == nil {
		return nil, errors.New("load log file: parseFn is nil")
	}
	f, err := openLogReader(path)
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
	var size int64
	if info, err := os.Stat(filepath.Clean(path)); err == nil {
		mtime = info.ModTime().UTC()
		size = info.Size()
	}

	lt := loadedTaskFromMeta(path, "", &meta, mtime, size)

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
		case "caic_session", "caic_init":
			applySessionMetadataLine(lt, envelope.Type, line)

		case "caic_pr":
			var mp agent.MetaPRMessage
			if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
				lt.ForgeOwner = mp.ForgeOwner
				lt.ForgeRepo = mp.ForgeRepo
				lt.ForgePR = mp.ForgePR
			}

		case "caic_diff_stat":
			noteDiffStatLine(lt, line)

		case "caic_result":
			var mr agent.MetaResultMessage
			if err := json.Unmarshal(line, &mr); err != nil {
				return nil, fmt.Errorf("invalid caic_result: %w", err)
			}
			applyMetaResult(lt, &mr)

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

// loadLogFileTail reads only the tail of a log file. It always reads the first
// line (metadata header), then seeks to (size - tailBytes) and parses only the
// remaining lines. This avoids loading multi-GB files into memory.
func loadLogFileTail(path string, parseFn func([]byte) ([]agent.Message, error), tailBytes int64) (_ *LoadedTask, retErr error) {
	if isLogCompressed(path) {
		return loadCompressedLogFileTail(path, parseFn, tailBytes)
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	// First line must be the metadata header.
	if !scanner.Scan() {
		return nil, errNotLogFile
	}
	var meta agent.MetaMessage
	if err := unmarshalMeta(scanner.Bytes(), &meta, &jsonutil.FieldWarner{}); err != nil {
		return nil, errNotLogFile
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	lt := loadedTaskFromMeta(path, "", &meta, info.ModTime().UTC(), info.Size())

	// Seek to the tail of the file.
	offset := max(int64(0), info.Size()-tailBytes)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek %s to %d: %w", path, offset, err)
	}

	// Reuse scanner from the seeked position. The first line after seek is
	// likely partial (we're mid-line), so skip it unless we're at the start.
	scanner = bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
	skipFirst := offset > 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if skipFirst {
			skipFirst = false
			continue
		}
		var envelope typeEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "caic_session", "caic_init":
			applySessionMetadataLine(lt, envelope.Type, line)

		case "caic_pr":
			var mp agent.MetaPRMessage
			if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
				lt.ForgeOwner = mp.ForgeOwner
				lt.ForgeRepo = mp.ForgeRepo
				lt.ForgePR = mp.ForgePR
			}
		case "caic_diff_stat":
			noteDiffStatLine(lt, line)
		case "caic_result":
			var mr agent.MetaResultMessage
			if err := json.Unmarshal(line, &mr); err != nil {
				return nil, fmt.Errorf("invalid caic_result: %w", err)
			}
			applyMetaResult(lt, &mr)
		default:
			parsed, err := parseFn(line)
			if err != nil {
				continue
			}
			lt.Msgs = append(lt.Msgs, parsed...)
		}
	}
	return lt, scanner.Err()
}

// loadCompressedLogFileTail scans a zstd log once and retains only the latest
// decompressed JSONL records bounded by tailBytes.
func loadCompressedLogFileTail(path string, parseFn func([]byte) ([]agent.Message, error), tailBytes int64) (_ *LoadedTask, retErr error) {
	f, err := openLogReader(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err2 := f.Close(); retErr == nil {
			retErr = err2
		}
	}()

	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)
	if !scanner.Scan() {
		return nil, errNotLogFile
	}
	var meta agent.MetaMessage
	if err := unmarshalMeta(scanner.Bytes(), &meta, &jsonutil.FieldWarner{}); err != nil {
		return nil, errNotLogFile
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}

	lt := loadedTaskFromMeta(path, "", &meta, info.ModTime().UTC(), info.Size())

	var lines [][]byte
	var total int64
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		line = bytes.Clone(line)
		lines = append(lines, line)
		total += int64(len(line) + 1)
		for total > tailBytes && len(lines) > 1 {
			total -= int64(len(lines[0]) + 1)
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, line := range lines {
		if err := applyLoadedLogLine(lt, line, parseFn, path); err != nil {
			return nil, err
		}
	}
	return lt, nil
}

func applyLoadedLogLine(lt *LoadedTask, line []byte, parseFn func([]byte) ([]agent.Message, error), path string) error {
	envelope, ok := decodeTypeEnvelope(line)
	if !ok {
		return nil
	}

	switch envelope.Type {
	case "caic_session", "caic_init":
		applySessionMetadataLine(lt, envelope.Type, line)

	case "caic_pr":
		var mp agent.MetaPRMessage
		if json.Unmarshal(line, &mp) == nil && mp.ForgePR > 0 {
			lt.ForgeOwner = mp.ForgeOwner
			lt.ForgeRepo = mp.ForgeRepo
			lt.ForgePR = mp.ForgePR
		}

	case "caic_diff_stat":
		noteDiffStatLine(lt, line)

	case "caic_result":
		var mr agent.MetaResultMessage
		if err := json.Unmarshal(line, &mr); err != nil {
			return fmt.Errorf("invalid caic_result: %w", err)
		}
		applyMetaResult(lt, &mr)

	default:
		parsed, err := parseFn(line)
		if err != nil {
			slog.Warn("failed to parse message", "err", err, "path", path)
			return nil
		}
		lt.Msgs = append(lt.Msgs, parsed...)
	}
	return nil
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
	case "pending":
		return StatePending
	case "branching":
		return StateBranching
	case "provisioning":
		return StateProvisioning
	case "starting":
		return StateStarting
	case "running":
		return StateRunning
	case "waiting":
		return StateWaiting
	case "asking":
		return StateAsking
	case "has_plan":
		return StateHasPlan
	case "pulling":
		return StatePulling
	case "pushing":
		return StatePushing
	case "stopping":
		return StateStopping
	case "stopped":
		return StateStopped
	case "purging":
		return StatePurging
	case "crashed":
		return StateCrashed
	case "failed":
		return StateFailed
	case "purged", "terminated": // "terminated" is for backward compat with pre-rename logs; remove once old logs age out
		return StatePurged
	default:
		return StateFailed
	}
}
