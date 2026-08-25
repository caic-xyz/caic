// Store owns the task-log directory it is given via NewStore: log segments,
// sidecar header caches, metadata trailers, terminal compression, and the
// trimmed startup scans (plain unsettled load, capped settled load, task-ID
// lookup).

package taskslog

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// SettledRetention bounds how far back a task log's mtime can be and still
// reload at startup. Logs last modified before this are skipped by the scan,
// so their compressed streams are never opened or decoded. It is the Store's
// default age cutoff, selected by the constructors.
const SettledRetention = 14 * 24 * time.Hour

// MaxSettledPerRepo caps how many settled (compressed) task logs load per
// repository, most recently updated first. The cap is applied in the scan,
// before a log is fully decoded, so capped logs cost only a header read. It
// is the Store's default per-repo cap, selected by the constructors.
const MaxSettledPerRepo = 5

// Store owns one task-log directory and its files: JSONL log segments, their
// sidecar header caches, metadata trailers, and terminal compression. Every
// operation on the directory goes through the Store, so the layout stays
// inside this package.
type Store struct {
	LogDir string

	log *slog.Logger

	// cutoff is the mtime cutoff for the LoadUnsettled and LoadSettled
	// scans: logs last modified before it are skipped before decode; zero
	// scans all ages. maxSettledPerRepo caps LoadSettled at that many logs
	// per repository; zero loads every repo's full settled history.
	cutoff            time.Time
	maxSettledPerRepo int

	// compressMu serializes compressPath calls, so concurrent settlements of
	// the same log (the startup pass and a task's own terminal path) cannot
	// interleave on the shared temp file; the loser deterministically adopts
	// the winner's compressed log.
	compressMu sync.Mutex
}

// NewStore creates a Store that owns logDir and every file under it. logDir
// is the final task-log directory (for example <cacheDir>/tasks); the caller
// decides where it sits in the cache layout. The directory is created lazily
// on first write.
//
// The scan fields start at the production configuration — the
// SettledRetention age cutoff and the MaxSettledPerRepo per-repo cap;
// in-package tests that probe other scan configurations override them
// directly.
func NewStore(log *slog.Logger, logDir string) *Store {
	return &Store{
		LogDir:            filepath.Clean(logDir),
		log:               log,
		cutoff:            time.Now().UTC().Add(-SettledRetention),
		maxSettledPerRepo: MaxSettledPerRepo,
	}
}

// LoadUnsettled scans LogDir for plain (uncompressed) task logs and loads
// every header, uncapped. This set is a superset of the live tasks: a task is
// compressed only once terminal, so it also includes recently-terminal logs
// not yet compressed and abandoned logs that were never settled. Only the
// header and result trailer are parsed; call LoadMessages for full
// conversation history. Logs last modified before the Store's age cutoff are
// skipped, and invalid or corrupt logs are skipped so one historical file
// cannot prevent startup.
func (s *Store) LoadUnsettled() ([]*LoadedTask, error) {
	paths, err := logPaths(s.log, s.LogDir, nil, false, s.cutoff)
	if err != nil {
		return nil, err
	}
	return loadLogsFromPaths(s.log, paths, false, true)
}

// LoadSettled scans LogDir for compressed (settled) task logs and loads their
// headers, capping the load at the Store's per-repo limit (MaxSettledPerRepo
// by default) in the scan before full decode so capped logs cost only a
// header read. A base that also has a plain source is skipped so the plain
// source stays authoritative after an interrupted compression. Logs last
// modified before the Store's age cutoff are skipped before decode, which
// keeps cold start cheap as terminal history grows.
func (s *Store) LoadSettled() ([]*LoadedTask, error) {
	paths, err := logPaths(s.log, s.LogDir, nil, true, s.cutoff)
	if err != nil {
		return nil, err
	}
	if s.maxSettledPerRepo > 0 {
		paths = capSettledPaths(s.log, paths, s.maxSettledPerRepo)
	}
	return loadLogsFromPaths(s.log, paths, false, true)
}

// LoadForTaskIDs loads metadata for plain logs whose parsed filename task ID
// matches one of taskIDs. It avoids parsing unrelated purged task logs during
// startup import of live runtime instances, which always own plain logs.
func (s *Store) LoadForTaskIDs(taskIDs []string) ([]*LoadedTask, error) {
	idSet := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		if id != "" {
			idSet[id] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil, nil
	}
	paths, err := logPaths(s.log, s.LogDir, idSet, false, time.Time{})
	if err != nil {
		return nil, err
	}
	found := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		found[taskIDFromLogBase(trimLogExt(filepath.Base(path)))] = struct{}{}
	}
	missing := make([]string, 0)
	for id := range idSet {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)

	tasks, loadErr := loadLogsFromPaths(s.log, paths, true, true)
	if len(missing) > 0 {
		loadErr = errors.Join(loadErr, fmt.Errorf("missing task logs for IDs: %s", strings.Join(missing, ", ")))
	}
	return tasks, loadErr
}

// Open creates a JSONL log segment with a v2 metadata header, or reopens an
// existing segment without rewriting its header. name must be a base
// filename (no path separators or "."/".." elements); Open resolves it under
// LogDir and returns the resolved path.
func (s *Store) Open(name string, header *agent.MetaMessage) (agent.LogSink, string, error) {
	path, err := s.resolveName(name)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("create log dir: %w", err)
	}
	w, created, err := openTaskLogForAppend(path, header, true)
	if err != nil {
		return nil, "", err
	}
	if created {
		w.version = agent.LogVersionV2
		if err := writeMetadataHeader(w, header); err != nil {
			return nil, "", errors.Join(err, w.Close())
		}
	}
	return w, path, nil
}

// Reopen validates and opens an existing task log for appending without a
// header. name must be a base filename (no path separators or "."/".."
// elements); Reopen resolves it under LogDir and returns the resolved path.
func (s *Store) Reopen(name string, header *agent.MetaMessage) (agent.LogSink, string, error) {
	path, err := s.resolveName(name)
	if err != nil {
		return nil, "", err
	}
	w, _, err := openTaskLogForAppend(path, header, false)
	if err != nil {
		return nil, "", err
	}
	return w, path, nil
}

func openTaskLogForAppend(path string, header *agent.MetaMessage, create bool) (w *taskLogWriter, created bool, err error) {
	cleanPath := filepath.Clean(path)
	if create {
		w, err = newTaskLogWriter(cleanPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err == nil {
			return w, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, fmt.Errorf("create log file: %w", err)
		}
	}
	w, err = newTaskLogWriter(cleanPath, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, false, err
	}
	version, err := validateRawLogAppend(w.file, cleanPath, header)
	if err != nil {
		return nil, false, errors.Join(err, w.Close())
	}
	w.version = version
	return w, false, nil
}

// validateRawLogAppend validates the immutable header needed to safely select
// the append encoding. caic owns task-log writes, so reopening does not rescan
// historical records.
func validateRawLogAppend(f *os.File, path string, header *agent.MetaMessage) (agent.LogVersion, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	scanner := newPhysicalLogScanner(f, path)
	if _, err := scanner.ReadHeader(); err != nil {
		return 0, fmt.Errorf("validate task log for append: %w", err)
	}
	if scanner.authority.Harness != header.Harness {
		return 0, fmt.Errorf("append task log: header harness %q does not match task harness %q", scanner.authority.Harness, header.Harness)
	}
	if err := scanner.authority.Version.Validate(); err != nil {
		return 0, fmt.Errorf("validate task log for append: %w", err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return scanner.authority.Version, nil
}

// WriteResultTrailer appends a MetaResultMessage to an active task log.
func (*Store) WriteResultTrailer(log agent.LogSink, title string, res *Result) error {
	if log == nil {
		return ErrNoLog
	}
	return log.AppendMessage(resultTrailer(title, res))
}

func resultTrailer(title string, res *Result) *agent.MetaResultMessage {
	mr := &agent.MetaResultMessage{
		MessageType:              "caic_result",
		State:                    res.State.String(),
		Title:                    title,
		CostUSD:                  res.CostUSD,
		Duration:                 res.Duration.Seconds(),
		NumTurns:                 res.NumTurns,
		InputTokens:              res.Usage.InputTokens,
		OutputTokens:             res.Usage.OutputTokens,
		CacheCreationInputTokens: res.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     res.Usage.CacheReadInputTokens,
		ReasoningOutputTokens:    res.Usage.ReasoningOutputTokens,
		DiffStat:                 res.DiffStat,
		AgentResult:              res.AgentResult,
	}
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	return mr
}

// WriteContextCleared appends a context_cleared system message to the task log.
func (*Store) WriteContextCleared(log agent.LogSink) error {
	if log == nil {
		return ErrNoLog
	}
	return log.AppendMessage(agent.ContextCleared())
}

// Compress closes log and compresses path only when state is non-revivable.
// Closing the live writer first ensures the compressed source contains every
// completed append. A failed compression leaves the plain source for retry.
func (s *Store) Compress(path string, log agent.LogSink, state State) (string, error) {
	if log != nil {
		if err := log.Close(); err != nil {
			return path, fmt.Errorf("close task log before compression: %w", err)
		}
	}
	if !state.IsTerminal() {
		return path, nil
	}
	return s.compressPath(path)
}

// CompressTerminal closes and compresses a terminal log, then publishes the
// caller's already-computed task summary beside the immutable compressed file.
// Cache publication is best-effort: a missing cache only makes the next load
// fall back to scanning the log, while compression remains authoritative.
func (s *Store) CompressTerminal(path string, log agent.LogSink, summary *LoadedTask) (string, error) {
	if summary == nil {
		return path, errors.New("compress terminal task log: summary is nil")
	}
	if !summary.State.IsTerminal() {
		return path, fmt.Errorf("compress terminal task log: state %q is revivable", summary.State)
	}
	if summary.LastTrailer == nil || summary.LastTrailer.State != summary.State {
		return path, errors.New("compress terminal task log: summary result is missing or inconsistent")
	}
	compressed, err := s.Compress(path, log, summary.State)
	if err != nil {
		return compressed, err
	}
	info, err := os.Stat(filepath.Clean(compressed))
	if err != nil {
		return compressed, fmt.Errorf("stat compressed task log: %w", err)
	}
	summary.path = compressed
	summary.LogSize = info.Size()
	if err := writeHeaderCache(compressed, summary); err != nil {
		// The immutable log remains the source of truth. Keep completion
		// successful and make the startup-cost regression visible.
		s.log.Warn("write compressed task log header cache", "path", compressed, "err", err)
	}
	return compressed, nil
}

// SettleTerminal compresses every terminal plain log in LogDir, regardless of
// age, skipping the paths in exclude. It runs at startup so terminal logs that
// fall outside the retention cutoff — which only bounds what the scans
// register — are settled instead of accumulating uncompressed forever.
// Exclusions keep the owner's compression authoritative: the caller passes
// the logs owned by live registry entries so the pass yields to the per-task
// terminal path; compressPath's compression mutex makes concurrent
// settlement of the same path safe regardless. Exclude keys are
// filepath.Clean-ed absolute log paths.
func (s *Store) SettleTerminal(exclude map[string]struct{}) error {
	paths, err := logPaths(s.log, s.LogDir, nil, false, time.Time{})
	if err != nil {
		return err
	}
	logs, err := loadLogsFromPaths(s.log, paths, false, false)
	if err != nil {
		return err
	}
	return s.compressTerminalLogs(logs, exclude)
}

func (s *Store) compressTerminalLogs(logs []*LoadedTask, exclude map[string]struct{}) error {
	var errs []error
	for _, lt := range logs {
		if lt == nil || !lt.State.IsTerminal() || isLogCompressed(lt.path) {
			continue
		}
		if exclude != nil {
			if _, ok := exclude[filepath.Clean(lt.path)]; ok {
				continue
			}
		}
		compressed, err := s.CompressTerminal(lt.path, nil, lt)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lt.path = compressed
	}
	return errors.Join(errs...)
}

// resolveName validates name is a plain task-log filename and joins it under
// LogDir, denying any path that escapes it (separators, "..", or absolute
// paths).
func (s *Store) resolveName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("task log name must be a base filename, got %q", name)
	}
	return filepath.Join(s.LogDir, name), nil
}

// compressPath atomically replaces a plain terminal task log with its zstd
// representation. A failed compression leaves the source log intact. Calls
// for the same path are serialized by compressMu, so a concurrent compressor
// either observes the compressed result and returns it, or sees the plain
// source gone with the compressed sibling present and adopts it.
func (s *Store) compressPath(path string) (string, error) {
	s.compressMu.Lock()
	defer s.compressMu.Unlock()
	if path == "" || isLogCompressed(path) {
		return path, nil
	}
	if !strings.HasSuffix(path, logPlainExt) {
		return "", fmt.Errorf("not a task log path: %s", path)
	}
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			// The plain source vanished after the scan. If the compressed
			// sibling exists the log was already settled by its owner (for
			// example the per-task terminal path); adopt it instead of
			// reporting a spurious compression failure.
			if _, statErr := os.Stat(compressedLogPath(path)); statErr == nil {
				return compressedLogPath(path), nil
			}
		}
		return "", err
	}
	tmp := compressedLogTempPath(path)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale temp compressed log: %w", err)
	}
	in, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(filepath.Clean(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", errors.Join(err, in.Close())
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return "", errors.Join(err, in.Close(), out.Close())
	}
	_, copyErr := io.Copy(enc, in)
	closeErr := errors.Join(enc.Close(), out.Close(), in.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// Preserve the source mtime: the settled age cutoff and the per-repo
	// newest-N cap rank by mtime, so the compressed log must measure as old as
	// its settlement, not as new as its compression.
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("set compressed log mtime: %w", err)
	}
	dst := compressedLogPath(path)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename compressed log: %w", err)
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			// The plain source is gone; the compressed log is the settled
			// result, so report it as success instead of deleting it.
			return dst, nil
		}
		return "", errors.Join(
			fmt.Errorf("remove plain log after compression: %w", err),
			os.Remove(dst),
		)
	}
	return dst, nil
}

func writeMetadataHeader(log agent.LogSink, header *agent.MetaMessage) error {
	meta := *header
	meta.Version = int(log.LogVersion())
	if err := log.AppendMessage(&meta); err != nil {
		return fmt.Errorf("write log metadata: %w", err)
	}
	return nil
}
