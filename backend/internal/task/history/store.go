// Owns task inventory, semantic trailers, terminal settlement, and usage projections.

package history

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
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

// Store owns semantic history projections over one physical journal.
type Store struct {
	Journal *taskslog.Store

	log               *slog.Logger
	resolver          ParserResolver
	cutoff            time.Time
	maxSettledPerRepo int
}

// NewStore supplies fresh native parser factories for every independent scan.
func NewStore(log *slog.Logger, journal *taskslog.Store, resolver ParserResolver) *Store {
	if journal == nil || resolver == nil {
		panic("history requires a journal and parser resolver")
	}
	return &Store{Journal: journal, log: log, resolver: resolver, cutoff: time.Now().UTC().Add(-SettledRetention), maxSettledPerRepo: MaxSettledPerRepo}
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
	paths, err := logPaths(s.log, s.Journal.LogDir, nil, false, s.cutoff)
	if err != nil {
		return nil, err
	}
	return loadLogsFromPaths(s.log, paths, logLoadHistory, true, s.resolver)
}

// LoadSettled scans LogDir for compressed (settled) task logs and loads their
// headers, capping the load at the Store's per-repo limit (MaxSettledPerRepo
// by default) in the scan before full decode so capped logs cost only a
// header read. A base that also has a plain source is skipped so the plain
// source stays authoritative after an interrupted compression. Logs last
// modified before the Store's age cutoff are skipped before decode, which
// keeps cold start cheap as terminal history grows.
// Unreadable historical logs are logged at ERROR and skipped without failing
// the scan; runtime restoration uses the strict LoadForTaskIDs path instead.
func (s *Store) LoadSettled() ([]*LoadedTask, error) {
	paths, err := logPaths(s.log, s.Journal.LogDir, nil, true, s.cutoff)
	if err != nil {
		return nil, err
	}
	if s.maxSettledPerRepo > 0 {
		paths = capSettledPaths(s.log, paths, s.maxSettledPerRepo)
	}
	return loadLogsFromPaths(s.log, paths, logLoadHistory, true, s.resolver)
}

// LoadAllReadOnly loads every retained plain or compressed task log without
// modifying the task-log directory. Plain sources win over compressed siblings
// left by interrupted settlement. It is intended for background consumers
// that need complete retained history rather than the bounded startup view.
func (s *Store) LoadAllReadOnly() ([]*LoadedTask, error) {
	paths, err := readOnlyLogPaths(s.Journal.LogDir)
	if err != nil {
		return nil, err
	}
	// A path that vanishes or becomes unreadable after ReadDir is unresolved
	// historical input, not a harmless omission: callers must retry instead of
	// treating the one-pass scan as complete and writing their sentinel.
	return loadLogsFromPaths(s.log, paths, logLoadRequired, false, s.resolver)
}

// LoadForTaskIDs loads plain or archived logs for retained runtime task IDs.
// Plain sources take precedence over archives left by interrupted compression.
// Unrelated historical tasks are excluded from startup restoration.
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
	paths, err := logPaths(s.log, s.Journal.LogDir, idSet, false, time.Time{})
	if err != nil {
		return nil, err
	}
	archives, err := logPaths(s.log, s.Journal.LogDir, idSet, true, time.Time{})
	if err != nil {
		return nil, err
	}
	paths = append(paths, archives...)
	found := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		found[taskIDFromLogBase(taskslog.TrimExt(filepath.Base(path)))] = struct{}{}
	}
	missing := make([]string, 0)
	for id := range idSet {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)

	tasks, loadErr := loadLogsFromPaths(s.log, paths, logLoadRequired, true, s.resolver)
	if len(missing) > 0 {
		loadErr = errors.Join(loadErr, fmt.Errorf("missing task logs for IDs: %s", strings.Join(missing, ", ")))
	}
	return tasks, loadErr
}

// ResultMessage projects terminal task accounting into its persisted control.
func ResultMessage(title string, res *task.Result) *agent.MetaResultMessage {
	mr := &agent.MetaResultMessage{
		MessageType:    "caic_result",
		State:          res.State.String(),
		Title:          title,
		CostUSD:        res.CostUSD,
		Duration:       res.Duration.Seconds(),
		NumTurns:       res.NumTurns,
		DiffStat:       res.DiffStat,
		DiskUsedBytes:  res.DiskUsedBytes,
		AgentResult:    res.AgentResult,
		StartupFailure: res.StartupFailure,
	}
	mr.SetUsage(res.Usage)
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	return mr
}

// Compress closes log and compresses path only when state is non-revivable.
// Closing the live writer first ensures the compressed source contains every
// completed append. A failed compression leaves the plain source for retry.
func (s *Store) Compress(path string, log agent.LogSink, state task.State) (string, error) {
	if log != nil {
		if err := log.Close(); err != nil {
			return path, fmt.Errorf("close task log before compression: %w", err)
		}
	}
	if !state.IsTerminal() {
		return path, nil
	}
	return s.Journal.Compress(path)
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
// terminal path; the journal's compression mutex makes concurrent
// settlement of the same path safe regardless. Exclude keys are
// filepath.Clean-ed absolute log paths.
func (s *Store) SettleTerminal(exclude map[string]struct{}) error {
	paths, err := logPaths(s.log, s.Journal.LogDir, nil, false, time.Time{})
	if err != nil {
		return err
	}
	logs, err := loadLogsFromPaths(s.log, paths, logLoadSettlement, false, s.resolver)
	if err != nil {
		return err
	}
	return s.compressTerminalLogs(logs, exclude)
}

// UsageRows streams reconstructed rows from every retained task log.
//
// Loading and parsing begin only when the sequence is iterated. The resolver
// supplies a fresh native parser for each physical log's validated harness.
// Per-log read or parse failures are logged and skipped; a non-nil error is
// yielded only for source-wide failures and terminates the sequence.
func (s *Store) UsageRows(ctx context.Context) iter.Seq2[data.UsageRow, error] {
	return storeUsageRows(s, ctx)
}

// Open creates or reopens a semantic recorder over a physical journal.
func (s *Store) Open(name string, header *agent.MetaMessage) (agent.LogSink, string, error) {
	meta := v3.MetaMessage(*header)
	w, path, err := s.Journal.Open(name, &meta)
	if err != nil {
		return nil, "", err
	}
	return agent.NewRecordedLog(w), path, nil
}

// Initialize prepares a closed schema header before the task is published.
func (s *Store) Initialize(name string, header *agent.MetaMessage) (*taskslog.PreparedLog, error) {
	meta := v3.MetaMessage(*header)
	return s.Journal.Initialize(name, &meta)
}

// Reopen transfers an existing physical writer to the semantic recorder and
// invalidates its rebuildable projection before the caller appends.
func (s *Store) Reopen(name string, header *agent.MetaMessage) (agent.LogSink, string, error) {
	meta := v3.MetaMessage(*header)
	w, path, err := s.Journal.Reopen(name, &meta)
	if err != nil {
		return nil, "", err
	}
	if taskslog.IsCompressed(name) {
		if err := os.Remove(logHeaderCachePath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", errors.Join(err, w.Close())
		}
	}
	return agent.NewRecordedLog(w), path, nil
}

// LoadSource validates and loads only a log header for history
// streaming. It intentionally does not scan inventory records or validate
// EOF: the caller's subsequent semantic stream performs that one full scan
// and validation.
func (s *Store) LoadSource(path string) (loaded *LoadedTask, retErr error) {
	if path == "" {
		return nil, taskslog.ErrNoLog
	}
	retErr = taskslog.ScanFile(path, false, func(info os.FileInfo, _ *taskslog.Scanner, meta v3.MetaMessage) error {
		base := taskslog.TrimExt(filepath.Base(path))
		loaded = loadedTaskFromMeta(path, taskIDFromLogBase(base), &meta, info.ModTime().UTC(), info.Size())
		return nil
	})
	if retErr != nil {
		return nil, retErr
	}
	loaded.resolver = s.resolver
	return loaded, nil
}

// LoadRelayBoundaries validates a complete task log and returns only physical
// relay positions for import reconciliation. Native payloads are validated as
// JSON envelopes; provider decoding follows once the final history is persisted.
func (*Store) LoadRelayBoundaries(path string) (records []agent.RelayRecordBoundary, generation string, retErr error) {
	var relay relayBoundaries
	retErr = taskslog.ScanFile(path, true, func(_ os.FileInfo, scanner *taskslog.Scanner, _ v3.MetaMessage) error {
		for scanner.Scan() {
			relay.observe(agent.ParsedRecord{Provenance: agent.FrameProvenance(scanner.Frame())}, scanner.Bytes())
		}
		return scanner.Err()
	})
	if retErr != nil {
		return nil, "", retErr
	}
	return relay.records, relay.generation, nil
}

func (s *Store) compressTerminalLogs(logs []*LoadedTask, exclude map[string]struct{}) error {
	var errs []error
	for _, lt := range logs {
		if lt == nil || !lt.State.IsTerminal() || taskslog.IsCompressed(lt.path) {
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
