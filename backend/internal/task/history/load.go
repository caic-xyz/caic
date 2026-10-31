// Package history reconstructs task metadata and conversations from physical journals.

package history

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

const maxParallelLogHeaderLoads = 8

// ParserResolver creates fresh independent full and inventory parsers.
type ParserResolver interface {
	ResolveParser(h harness.Name) (agent.MessageParser, error)
	ResolveMetadataParser(h harness.Name) (agent.MessageParser, error)
}

func applyMetaResult(lt *LoadedTask, mr *agent.MetaResultMessage) {
	lt.State = parseState(mr.State)
	if mr.Title != "" {
		lt.Title = mr.Title
	}
	lt.LastTrailer = &task.Result{
		State:          lt.State,
		CostUSD:        mr.CostUSD,
		Duration:       time.Duration(mr.Duration * float64(time.Second)),
		NumTurns:       mr.NumTurns,
		Usage:          mr.TokenUsage(),
		DiffStat:       mr.DiffStat,
		DiskUsedBytes:  mr.DiskUsedBytes,
		AgentResult:    mr.AgentResult,
		StartupFailure: mr.StartupFailure,
	}
	if len(mr.DiffStat) > 0 {
		lt.DiffCreated = true
	}
	if mr.Error != "" {
		lt.LastTrailer.Err = errors.New(mr.Error)
	}
}

// LoadedTask holds the data reconstructed from a single task log file.
//
// Header-cache persistence uses an explicit versioned snapshot projection.
type LoadedTask struct {
	agent.SessionMetadata

	TaskID            string // Task ID parsed from log filename; empty if unparseable.
	Prompt            string
	Title             string
	Repos             []repo.Mount // GitRoot will be empty for purged tasks loaded from logs.
	Harness           harness.Name
	StartedAt         time.Time
	LastStateUpdateAt time.Time // Latest relay ts from caic_diff_stat records, falling back to log file mtime.
	State             task.State
	ForgeIssue        int // Originating issue number for bot comment callbacks.
	OwnerID           string
	ForkedFromTaskID  string
	ParentTaskID      string
	CaicMCP           bool
	ForgeOwner        string
	ForgeRepo         string
	ForgePR           int // PR number created during the task; 0 if none.
	Tailscale         bool
	USB               bool
	Display           bool
	Sudo              bool
	GitHubToken       bool
	RuntimeName       runtime.Name
	BaseImage         string
	ContainerPlatform string
	MaxCPUs           int
	CacheMounts       []runtime.CacheMount
	Mounts            []runtime.Mount
	RequestedModel    string       // User-requested model.
	RequestedEffort   string       // User-requested reasoning effort.
	LogSize           int64        // Byte size of the log file on disk; populated by Store.Load.
	DiffCreated       bool         // True if any non-empty diff was recorded in the log; sticky across the run.
	LastTrailer       *task.Result // Completion result parsed from the log trailer at load time; a snapshot of the last recorded run, never live state.
	Timeline          []agent.TimedMessage

	path           string         // Absolute path for lazy message loading via LoadMessages.
	resolver       ParserResolver // Fresh parser factory supplied by the task owner.
	messagesLoaded bool           // A completed semantic scan may validly produce no messages.
}

func loadSemanticTask(path string, resolver ParserResolver) (*LoadedTask, error) {
	loaded := &LoadedTask{}
	var projection taskMetadataProjection
	err := scanSemanticLog(path, resolver, func(record agent.ParsedRecord) {
		projection.apply(loaded, record.Messages)
		for _, parsed := range record.Messages {
			if record.Control && isInventoryOnlyMessage(parsed.Message) {
				continue
			}
			loaded.Timeline = append(loaded.Timeline, parsed)
		}
	})
	if err != nil {
		return nil, err
	}
	projection.finish(loaded)
	return loaded, nil
}

func loadedTaskFromMeta(path, taskID string, meta *v3.MetaMessage, modified time.Time, size int64) *LoadedTask {
	repos := make([]repo.Mount, len(meta.Repos))
	for i, mr := range meta.Repos {
		repos[i] = repo.Mount{Name: mr.Name, BaseBranch: mr.BaseBranch, Branch: mr.Branch, ContainerPath: mr.ContainerPath}
	}
	return &LoadedTask{
		path:              path,
		TaskID:            taskID,
		Prompt:            meta.Prompt,
		Title:             meta.Title,
		Repos:             repos,
		Harness:           harness.Name(meta.Harness),
		RequestedModel:    meta.RequestedModel,
		RequestedEffort:   meta.RequestedEffort,
		StartedAt:         meta.StartedAt,
		LastStateUpdateAt: modified,
		State:             task.StateRunning,
		ForgeIssue:        meta.ForgeIssue,
		OwnerID:           meta.OwnerID,
		ForkedFromTaskID:  meta.ForkedFromTaskID,
		ParentTaskID:      meta.ParentTaskID,
		CaicMCP:           meta.CaicMCP,
		Tailscale:         meta.Tailscale,
		USB:               meta.USB,
		Display:           meta.Display,
		Sudo:              meta.Sudo,
		GitHubToken:       meta.GitHubToken,
		RuntimeName:       runtime.Name(meta.RuntimeName),
		BaseImage:         meta.BaseImage,
		ContainerPlatform: meta.ContainerPlatform,
		MaxCPUs:           meta.MaxCPUs,
		CacheMounts:       runtimeCacheMountsFromMeta(meta.CacheMounts),
		Mounts:            runtimeMountsFromMeta(meta.Mounts),
		LogSize:           size,
	}
}

// loadLogMetaHeader parses only the leading metadata header of a task log
// (plain or compressed) into a LoadedTask. It intentionally does not consume
// the log past the header or validate EOF; it exists so the per-repo cap can
// attribute a log to its repo without decoding the body.
func loadLogMetaHeader(path string) (loaded *LoadedTask, retErr error) {
	retErr = taskslog.ScanFile(path, false, func(info os.FileInfo, _ *taskslog.Scanner, meta v3.MetaMessage) error {
		base := taskslog.TrimExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		return nil
	})
	if retErr != nil {
		return nil, retErr
	}
	return loaded, nil
}

// loadLogHeader projects the header, controls, and native metadata from a task log.
// It retains no conversation messages; call LoadMessages for those. The path
// is stored for lazy loading. A header cache matching the log size and mtime
// skips the full decode; any mismatch falls back to the scan and refreshes it.
func loadLogHeader(log *slog.Logger, path string, cacheHeader bool, resolver ParserResolver) (loaded *LoadedTask, retErr error) {
	if cached, ok := readHeaderCache(path); ok {
		cached.resolver = resolver
		return cached, nil
	}
	retErr = taskslog.ScanFile(path, false, func(info os.FileInfo, scanner *taskslog.Scanner, meta v3.MetaMessage) error {
		base := taskslog.TrimExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		loaded.resolver = resolver
		return scanInventoryRecords(path, scanner, loaded, resolver)
	})
	if retErr != nil {
		return nil, retErr
	}
	// Cache the header so the next load skips the scan. Only terminal (immutable)
	// logs are worth caching: they are the compressed, CPU-bound set, while live
	// logs change on every append and would just invalidate the entry.
	if cacheHeader && loaded.State.IsTerminal() {
		if err := writeHeaderCache(path, loaded); err != nil {
			// Non-fatal: the load succeeded and the next load re-scans. Log it so a
			// persistent failure (e.g. a read-only cache dir) is visible; it slows
			// startup but never affects correctness.
			log.Warn("write task log header cache", "path", path, "err", err)
		}
	}
	return loaded, nil
}

// Primary returns a pointer to the primary repository mount (Repos[0]), or nil for no-repo tasks.
func (lt *LoadedTask) Primary() *repo.Mount {
	if len(lt.Repos) == 0 {
		return nil
	}
	return &lt.Repos[0]
}

// LogPath returns the absolute task log path used to load the task.
func (lt *LoadedTask) LogPath() string {
	return lt.path
}

// LoadMessages lazily reconstructs the complete ordered conversation once.
func (lt *LoadedTask) LoadMessages() error {
	if lt.messagesLoaded || lt.Timeline != nil || lt.path == "" {
		return nil
	}
	loaded, err := loadSemanticTask(lt.path, lt.resolver)
	if err != nil {
		return err
	}
	applySemanticTask(lt, loaded, true)
	return nil
}

// StreamMessages streams parsed task conversation records directly from the
// log. Cancellation is checked between records and before delivery.
func (lt *LoadedTask) StreamMessages(ctx context.Context) iter.Seq2[agent.TimedMessage, error] {
	return func(yield func(agent.TimedMessage, error) bool) {
		if lt.path == "" {
			yield(agent.TimedMessage{}, errors.New("task has no log path"))
			return
		}
		err := taskslog.ScanFile(lt.path, true, func(_ os.FileInfo, scanner *taskslog.Scanner, _ v3.MetaMessage) error {
			wire, err := lt.resolver.ResolveParser(harness.Name(scanner.Harness))
			if err != nil {
				return err
			}
			parser, err := agent.NewLogRecordParser(wire)
			if err != nil {
				return err
			}
			if _, err := parser.DecodeFrame(&scanner.HeaderFrame); err != nil {
				return err
			}
			for scanner.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				record, err := parser.DecodeFrame(scanner.Frame())
				if err != nil {
					return err
				}
				for _, message := range record.Messages {
					if record.Control && !isHistoryStreamControlMessage(message.Message) {
						continue
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if !yield(message, nil) {
						return errTaskLogStreamStopped
					}
				}
			}
			return scanner.Err()
		})
		if err != nil && !errors.Is(err, errTaskLogStreamStopped) {
			yield(agent.TimedMessage{}, err)
		}
	}
}

// UsageRows streams this task's durable usage by producer day and model.
//
// Costs and quota snapshots require live task state, so reconstruction leaves
// both absent. Task-ID-less logs yield no rows. A non-nil error
// is yielded at most once and terminates the sequence.
func (lt *LoadedTask) UsageRows(ctx context.Context) iter.Seq2[data.UsageRow, error] {
	return taskUsageRows(lt, ctx)
}

// relayBoundaries owns generation-local byte positions for physical import
// reconciliation. Host controls and client input add no bytes.
type relayBoundaries struct {
	records    []agent.RelayRecordBoundary
	generation string
	end        int64
}

func (r *relayBoundaries) observe(record agent.ParsedRecord, encoded []byte) {
	if record.Provenance.Generation != "" {
		r.generation = record.Provenance.Generation
		r.end = int64(len(encoded) + 1)
		r.records = nil
	} else if record.Provenance.Source == agent.RecordSourceRelay {
		r.end += int64(len(encoded) + 1)
	}
	if record.Provenance.Source == agent.RecordSourceRelay {
		r.records = append(r.records, agent.RelayRecordBoundary{
			Generation: r.generation, RelayEnd: r.end, Fingerprint: sha256.Sum256(encoded),
		})
	}
}

// scanSemanticLog parses a complete task log with a fresh header-authorized
// parser and folds each physical record before reusing the scanner buffer.
// It retains no intermediate message or physical-record arrays.
func scanSemanticLog(path string, resolver ParserResolver, visit func(agent.ParsedRecord)) (retErr error) {
	retErr = taskslog.ScanFile(path, true, func(_ os.FileInfo, scanner *taskslog.Scanner, _ v3.MetaMessage) error {
		wire, err := resolver.ResolveParser(harness.Name(scanner.Harness))
		if err != nil {
			return fmt.Errorf("resolve native parser for harness %q: %w", harness.Name(scanner.Harness), err)
		}
		parser, err := agent.NewLogRecordParser(wire)
		if err != nil {
			return fmt.Errorf("construct log parser: %w", err)
		}
		record, err := parser.DecodeFrame(&scanner.HeaderFrame)
		if err != nil {
			return fmt.Errorf("parse task log bootstrap %s: %w", path, err)
		}
		visit(record)
		for scanner.Scan() {
			record, err := parser.DecodeFrame(scanner.Frame())
			if err != nil {
				return fmt.Errorf("parse task log %s: %w", path, err)
			}
			visit(record)
		}
		return scanner.Err()
	})
	return retErr
}

// ExportDiscussion loads one physical task log with its header-authorized native
// parser and renders the resulting task data as markdown.
func ExportDiscussion(path string, resolver ParserResolver) (string, error) {
	var meta *agent.MetaMessage
	var result *agent.MetaResultMessage
	var pr *agent.MetaPRMessage
	var messages []agent.Message
	err := scanSemanticLog(path, resolver, func(record agent.ParsedRecord) {
		for _, parsed := range record.Messages {
			switch message := parsed.Message.(type) {
			case *agent.MetaMessage:
				meta = message
			case *agent.MetaResultMessage:
				result = message
			case *agent.MetaPRMessage:
				pr = message
			default:
				messages = append(messages, message)
			}
		}
	})
	if err != nil {
		return "", err
	}
	if meta == nil {
		return "", fmt.Errorf("%s: no caic_meta header", path)
	}
	return agent.RenderDiscussion(meta, result, pr, messages), nil
}

// taskMetadataProjection folds shared startup and lazy-load metadata. Native
// Claude accounting survives a zero-cost revival trailer until EOF, when the
// latest authoritative trailer and last completed root result are both known.
type taskMetadataProjection struct {
	lastResult *agent.ResultMessage
}

func (p *taskMetadataProjection) apply(loaded *LoadedTask, messages []agent.TimedMessage) {
	for _, parsed := range messages {
		loaded.Apply(parsed.Message)
		switch msg := parsed.Message.(type) {
		case *agent.MetaMessage:
			loaded.Harness = harness.Name(msg.Harness)
		case *agent.ResultMessage:
			// Claude's native result is the inventory accounting source. Other
			// harnesses preserve accounting in task completion trailers.
			if loaded.Harness == harness.Claude && !msg.Source.IsChild() {
				p.lastResult = msg
			}
		case *agent.MetaPRMessage:
			if msg.ForgePR > 0 {
				loaded.ForgeOwner = msg.ForgeOwner
				loaded.ForgeRepo = msg.ForgeRepo
				loaded.ForgePR = msg.ForgePR
			}
		case *agent.MetaResultMessage:
			applyMetaResult(loaded, msg)
		case *agent.DiffStatMessage:
			if len(msg.DiffStat) > 0 {
				loaded.DiffCreated = true
			}
			if msg.Ts > 0 {
				if t := tsToTime(msg.Ts); t.After(loaded.LastStateUpdateAt) {
					loaded.LastStateUpdateAt = t
				}
			}
		}
	}
}

func (p *taskMetadataProjection) finish(loaded *LoadedTask) {
	if loaded.LastTrailer != nil && loaded.LastTrailer.CostUSD == 0 && p.lastResult != nil && p.lastResult.TotalCostUSD > 0 {
		loaded.LastTrailer.CostUSD = p.lastResult.TotalCostUSD
		loaded.LastTrailer.Duration = time.Duration(p.lastResult.DurationMs) * time.Millisecond
		loaded.LastTrailer.NumTurns = p.lastResult.NumTurns
		loaded.LastTrailer.Usage = p.lastResult.Usage
	}
}

func isInventoryOnlyMessage(msg agent.Message) bool {
	switch msg.(type) {
	case *agent.MetaMessage, *agent.MetaPRMessage, *agent.MetaResultMessage:
		return true
	default:
		return false
	}
}

func applySemanticTask(lt, loaded *LoadedTask, messages bool) {
	lt.SessionMetadata = loaded.SessionMetadata
	if loaded.Title != "" {
		lt.Title = loaded.Title
	}
	if loaded.LastTrailer != nil {
		lt.State = loaded.State
		lt.LastTrailer = loaded.LastTrailer
	}
	if loaded.LastStateUpdateAt.After(lt.LastStateUpdateAt) {
		lt.LastStateUpdateAt = loaded.LastStateUpdateAt
	}
	if loaded.ForgePR > 0 {
		lt.ForgeOwner = loaded.ForgeOwner
		lt.ForgeRepo = loaded.ForgeRepo
		lt.ForgePR = loaded.ForgePR
	}
	if loaded.DiffCreated {
		lt.DiffCreated = true
	}
	if messages {
		lt.Timeline = loaded.Timeline
		lt.messagesLoaded = true
	}
}

// capSettledPaths keeps the newest limitPerRepo log paths per repo (by mtime).
// Each repo is read from the cheap first-line header (or its cache), bounded to
// maxParallelLogHeaderLoads concurrent reads so a large cache does not blow up
// transient memory; the caller then fully decodes only the kept paths.
func capSettledPaths(log *slog.Logger, paths []string, limitPerRepo int) []string {
	type cand struct {
		path  string
		repo  string
		mtime time.Time
	}
	cands := make([]cand, len(paths))
	workers := min(len(paths), max(goruntime.GOMAXPROCS(0), 1), maxParallelLogHeaderLoads)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				info, err := os.Stat(paths[i])
				var mtime time.Time
				if info != nil {
					mtime = info.ModTime().UTC()
				} else if err != nil && !os.IsNotExist(err) {
					// A stat failure leaves mtime zero, which sorts the path
					// oldest and can drop it under the cap; surface the cause.
					log.Warn("stat settled task log for capping", "path", paths[i], "err", err)
				}
				cands[i] = cand{path: paths[i], repo: repoFromHeader(log, paths[i]), mtime: mtime}
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// Stable: equal mtimes (batch writes, copied files) must keep the
	// deterministic ReadDir order instead of an arbitrary sort outcome.
	slices.SortStableFunc(cands, func(a, b cand) int { return b.mtime.Compare(a.mtime) })
	perRepo := make(map[string]int)
	kept := make([]string, 0, len(cands))
	for _, c := range cands {
		if perRepo[c.repo] < limitPerRepo {
			perRepo[c.repo]++
			kept = append(kept, c.path)
		}
	}
	return kept
}

// repoFromHeader returns the primary repo name for a task log, from the header
// cache when present and otherwise the cheap first-line header. It is used to
// cap settled logs per repo without a full decode.
func repoFromHeader(log *slog.Logger, path string) string {
	if lt, ok := readHeaderCache(path); ok {
		return primaryRepoName(lt)
	}
	lt, err := loadLogMetaHeader(path)
	if err != nil {
		// Unattributable logs share the "" bucket with no-repo tasks; surface
		// the cause instead of silently consuming that bucket's cap slots.
		log.Warn("read task-log header for repo attribution", "path", path, "err", err)
		return ""
	}
	return primaryRepoName(lt)
}

func primaryRepoName(lt *LoadedTask) string {
	if p := lt.Primary(); p != nil {
		return p.Name
	}
	return ""
}

// logPaths lists task log paths in logDir restricted to one storage form
// (plain or compressed) and an optional mtime age cutoff. When taskIDs is
// non-empty only matching task IDs are returned. For the compressed form, a base
// that also has a plain source is skipped so the plain source stays
// authoritative after an interrupted compression. When cutoff is non-zero,
// logs with an mtime older than cutoff are skipped before any decode.
func logPaths(log *slog.Logger, logDir string, taskIDs map[string]struct{}, compressed bool, cutoff time.Time) ([]string, error) {
	paths, err := taskslog.Paths(logDir, compressed)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(logDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	bases := make(map[string]struct{})
	for _, e := range entries {
		if !e.IsDir() && taskslog.IsLogName(e.Name()) {
			bases[taskslog.TrimExt(e.Name())] = struct{}{}
		}
	}
	reapHeaderCaches(logDir, entries, bases)
	filtered := paths[:0]
	for _, path := range paths {
		if len(taskIDs) > 0 {
			if _, ok := taskIDs[taskIDFromLogBase(taskslog.TrimExt(filepath.Base(path)))]; !ok {
				continue
			}
		}
		if !cutoff.IsZero() {
			info, err := os.Stat(path)
			if err != nil {
				if !os.IsNotExist(err) {
					log.Warn("stat task log during scan", "path", path, "err", err)
				}
			} else if info.ModTime().UTC().Before(cutoff) {
				continue
			}
		}
		filtered = append(filtered, path)
	}
	return filtered, nil
}

// readOnlyLogPaths lists every retained log without modifying the task-log
// directory. Plain sources take precedence over compressed siblings, and the
// combined lexical order keeps streaming consumers deterministic.
func readOnlyLogPaths(logDir string) ([]string, error) {
	plain, err := taskslog.Paths(logDir, false)
	if err != nil {
		return nil, err
	}
	settled, err := taskslog.Paths(logDir, true)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(plain)+len(settled))
	paths = append(paths, plain...)
	paths = append(paths, settled...)
	slices.Sort(paths)
	return paths, nil
}

type logLoadMode uint8

const (
	logLoadRequired logLoadMode = iota
	logLoadHistory
	logLoadSettlement
)

func loadLogsFromPaths(log *slog.Logger, paths []string, mode logLoadMode, cacheHeader bool, resolver ParserResolver) ([]*LoadedTask, error) {
	// Parse headers concurrently, but retain only a bounded number of scanner
	// buffers and decompressor/file handles when a cache has many old logs.
	type result struct {
		lt  *LoadedTask
		err error
	}
	results := make([]result, len(paths))
	workers := min(len(paths), max(goruntime.GOMAXPROCS(0), 1), maxParallelLogHeaderLoads)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				lt, err := loadLogHeader(log, paths[i], cacheHeader, resolver)
				results[i] = result{lt, err}
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	var tasks []*LoadedTask
	var errs []error
	for i, r := range results {
		if r.err != nil {
			switch mode {
			case logLoadRequired:
				errs = append(errs, fmt.Errorf("load task log %s: %w", paths[i], r.err))
			case logLoadHistory:
				// Historical logs are best-effort. Keep failures in the server
				// logs without making a usable task list fail for an old task.
				log.Error("skipping unreadable task log", "path", paths[i], "err", r.err)
			case logLoadSettlement:
				// The subsequent history scan owns ERROR reporting. Settlement
				// also scans old logs that are outside history's retention cutoff.
				log.Debug("skipping unreadable task log during settlement", "path", paths[i], "err", r.err)
			}
			continue
		}
		tasks = append(tasks, r.lt)
	}

	slices.SortFunc(tasks, func(a, b *LoadedTask) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return tasks, errors.Join(errs...)
}

func taskIDFromLogBase(base string) string {
	if before, _, ok := strings.Cut(base, "-"); ok {
		return before
	}
	return base
}

var errTaskLogStreamStopped = errors.New("task log stream consumer stopped")

// isHistoryStreamControlMessage reports controls delivered alongside native
// messages while streaming task history.
func isHistoryStreamControlMessage(msg agent.Message) bool {
	switch msg := msg.(type) {
	case *agent.DiffStatMessage, *agent.ExitMessage, *agent.LogMessage, *agent.TurnCommitSnapshotMessage:
		return true
	case *agent.SystemMessage:
		return msg.Subtype == "context_cleared"
	default:
		return false
	}
}

// scanInventoryRecords projects canonical controls and provider-owned native
// metadata without retaining conversation history. Claude reports session and
// completed-run accounting in its native init/result records.
func scanInventoryRecords(path string, scanner *taskslog.Scanner, lt *LoadedTask, resolver ParserResolver) error {
	native, err := resolver.ResolveMetadataParser(lt.Harness)
	if err != nil {
		return err
	}
	parser, err := agent.NewLogRecordParser(native)
	if err != nil {
		return err
	}
	bootstrap, err := parser.DecodeFrame(&scanner.HeaderFrame)
	if err != nil {
		return fmt.Errorf("parse task log bootstrap %s: %w", path, err)
	}
	var projection taskMetadataProjection
	projection.apply(lt, bootstrap.Messages)
	for scanner.Scan() {
		record, err := parser.DecodeFrame(scanner.Frame())
		if err != nil {
			return fmt.Errorf("parse task log %s: %w", path, err)
		}
		projection.apply(lt, record.Messages)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	projection.finish(lt)
	return nil
}

// tsToTime converts a Unix epoch float64 (seconds with sub-second precision)
// to a time.Time in UTC.
func tsToTime(ts float64) time.Time {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// parseState converts a persisted state string back to a State value.
// An unrecognized value (corrupt or from a newer version) is treated as
// failed rather than rejected, so one bad historical log cannot block startup.
func parseState(s string) task.State {
	state := task.State(s)
	if state.Validate() != nil {
		return task.StateFailed
	}
	return state
}
