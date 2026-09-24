// Store is the usage rollup sink: daily JSONL files, old-day compression, aggregates, resume watermarks, purge discard, and historical backfill.
//
// It implements the task.RollupSink ingestion contract. Task folds call
// Observe under their own task mutex, and the store serializes all file and
// aggregate access behind a single writer mutex.

package usagedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// dayFileRe matches plain and compressed rollup day file names.
var dayFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl(?:\.zstd)?$`)

// Config holds the store's dependencies.
type Config struct {
	// Log receives recovery warnings and write errors. Required.
	Log *slog.Logger
	// Dir is the rollup data directory, created when missing. Required.
	Dir string
}

// Store owns the usage rollup directory and every file in it. It is safe for
// concurrent use: task folds call Observe under their own task mutex, and the
// store serializes all file and aggregate access behind a single writer
// mutex.
type Store struct {
	log *slog.Logger
	dir string

	backfillMu sync.Mutex // serializes one-pass retained-log reconstruction
	mu         sync.Mutex
	files      map[string]*os.File      // day -> open append handle
	days       map[string]*dayAggregate // day -> aggregates over flushed rows
	watermarks map[string]time.Time     // task id -> newest producer time covered by flushed rows
	// flushedCost is the durable reported-or-estimated cost baseline.
	// pending.costInFlight is the movement assigned to unflushed buckets;
	// a later live snapshot reconciles any estimate in that baseline.
	flushedCost       map[string]float64
	reportedCostTasks map[string]struct{}           // tasks with a recorded non-estimated cost row
	pending           map[string]*taskPending       // task id -> unflushed deltas
	toolTimings       map[string]*ToolTimingTracker // task id -> unmatched tool starts
	lastQuota         map[quotaKey]quotaSeen        // provider window -> last written quota state
	closed            bool
	stopFlush         chan struct{}
	flushDone         chan struct{}
	compressionDone   chan struct{}
}

// New opens the rollup store: it recovers aggregates and watermarks from any
// existing plain or compressed day files and starts background maintenance.
func New(cfg Config) (*Store, error) {
	if cfg.Log == nil {
		return nil, errors.New("usage rollup logger is required")
	}
	if cfg.Dir == "" {
		return nil, errors.New("usage rollup directory is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create usage rollup directory: %w", err)
	}
	s := &Store{
		log:               cfg.Log,
		dir:               cfg.Dir,
		files:             make(map[string]*os.File),
		days:              make(map[string]*dayAggregate),
		watermarks:        make(map[string]time.Time),
		flushedCost:       make(map[string]float64),
		reportedCostTasks: make(map[string]struct{}),
		pending:           make(map[string]*taskPending),
		toolTimings:       make(map[string]*ToolTimingTracker),
		lastQuota:         make(map[quotaKey]quotaSeen),
		stopFlush:         make(chan struct{}),
		flushDone:         make(chan struct{}),
		compressionDone:   make(chan struct{}),
	}
	s.recover()
	go s.flushLoop()
	go s.compressionLoop()
	return s, nil
}

// Observe records one ingest event observed at the event's producer time.
//
// Restart resume: events at or before the task's flushed watermark are
// already reflected in the rollup files and are skipped; the unflushed tail
// of a replayed history is ingested. Timestamp-less replayed events have no
// ordering identity: they are ingested only when the task has no watermark
// at all (adopted tasks), attributed to the current day, and never advance
// the watermark — so one replay pass is complete, but a re-replay of the
// same timestamp-less history can duplicate its rows. Replayed events at or
// before a watermark are skipped, while a live event before it is retained:
// producer clocks can regress and the daily rollup must not silently lose
// that usage.
func (s *Store) Observe(meta TaskMeta, e *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || meta.TaskID.IsZero() {
		return
	}
	id := meta.TaskID.String()
	if e.ToolStartID != "" {
		t := s.toolTimings[id]
		if t == nil {
			t = &ToolTimingTracker{}
			s.toolTimings[id] = t
		}
		t.Start(e.ToolStartID, e.ToolName, e.ToolProducerTime)
	}
	if e.ToolResultID != "" {
		if t := s.toolTimings[id]; t != nil {
			if name, ms, ok := t.Finish(e.ToolResultID, e.ToolProducerTime, e.ToolNativeDurationMs); ok {
				e.Delta.ToolTimings = map[string]ToolTiming{name: {Count: 1, DurationMs: ms}}
			}
			if len(t.pending) == 0 {
				delete(s.toolTimings, id)
			}
		}
		if e.Delta.ToolTimings == nil {
			return
		}
	}
	wm := s.watermarks[id]
	synthetic := e.At.IsZero()
	if synthetic {
		if !wm.IsZero() {
			return
		}
		e.At = time.Now()
	} else if !wm.IsZero() && !e.At.After(wm) {
		if e.Replayed || e.At.Equal(wm) {
			return
		}
		s.log.Warn("retain live usage event before task watermark", "task", id, "producer_time", e.At, "watermark", wm)
	}
	p := s.pending[id]
	if p == nil {
		p = &taskPending{
			meta:    meta,
			buckets: make(map[bucketKey]*bucket),
		}
		p.meta.Repos = slices.Clone(meta.Repos)
		s.pending[id] = p
	}
	p.fold(e, synthetic)
	if e.TurnBoundary {
		// Turn boundary: flush so a crash never loses completed-turn usage.
		s.flushTaskLocked(id)
	}
}

// Discard drops a purged task's unflushed usage and resume bookkeeping.
//
// Flushed rows remain in daily files and aggregates; missing-cost backfill may
// later add an estimate to one. Callers must stop forwarding the task's events
// before calling Discard; the task package enforces that boundary.
func (s *Store) Discard(meta TaskMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || meta.TaskID.IsZero() {
		return
	}
	id := meta.TaskID.String()
	delete(s.pending, id)
	delete(s.toolTimings, id)
	delete(s.watermarks, id)
	delete(s.flushedCost, id)
}

// ObserveQuota records a provider quota-window status change; rows are
// written only when the observed state changes. It implements the sink
// contract's quota method.
func (s *Store) ObserveQuota(c *QuotaChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.recordQuotaLocked(c)
}

// Close flushes all pending deltas and releases the day file handles. It is
// idempotent. It implements the sink contract.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopFlush)
	for id := range s.pending {
		s.flushTaskLocked(id)
	}
	files := s.files
	s.files = make(map[string]*os.File)
	s.mu.Unlock()
	<-s.flushDone
	<-s.compressionDone
	var errs []error
	for day, f := range files {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close usage rollup day file %s: %w", day, err))
		}
	}
	return errors.Join(errs...)
}

// Days returns a sorted snapshot of the per-day aggregates over flushed
// rows. Reads never touch the filesystem.
func (s *Store) Days() []DayRollup {
	s.mu.Lock()
	defer s.mu.Unlock()
	days := make([]string, 0, len(s.days))
	for day := range s.days {
		days = append(days, day)
	}
	slices.Sort(days)
	out := make([]DayRollup, 0, len(days))
	for _, day := range days {
		out = append(out, s.dayRollupLocked(day))
	}
	return out
}

// Backfill streams neutral usage rows into missing daily files once.
// It creates only missing day files, so a completed or interrupted pass cannot
// duplicate an append-only rollup. rows is iterated only after checking the
// done sentinel, so callers can defer expensive source-specific loading and
// parsing. Staging happens outside the writer lock. Before publishing a staged
// day, it flushes matching live pending buckets so their later turn-boundary
// flush cannot repeat records already found by the historical scan.
func (s *Store) Backfill(ctx context.Context, rows iter.Seq2[UsageRow, error]) error {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()

	done, err := s.backfillIsDone()
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if rows == nil {
		return errors.New("usage backfill row source is required")
	}
	staging := newBackfillStaging(s.dir)
	defer staging.cleanup()
	for row, err := range rows {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.Day == "" {
			return errors.New("usage backfill row day is required")
		}
		if err := staging.append(&row); err != nil {
			return err
		}
	}
	if err := staging.close(); err != nil {
		return err
	}
	days := staging.daysSorted()
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.createBackfillDay(day, staging.days[day]); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.markBackfillDone()
}

// BackfillMissingCosts fills zero-cost usage rows using current model prices.
// It leaves reported costs intact and marks estimates so a later live cost
// snapshot can reconcile them. Each changed day is replaced atomically under
// the writer lock; repeating the pass changes no priced row. The estimator
// runs under that lock and must not perform I/O. The row passed to it is
// read-only and valid only for the duration of the call. Rechecking on every
// startup lets a newly published model price fill rows a prior pass could not
// price.
func (s *Store) BackfillMissingCosts(ctx context.Context, estimate func(*UsageRow) (float64, bool)) error {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if estimate == nil {
		return errors.New("usage cost estimator is required")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("scan usage rollup days for missing cost: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !dayFileRe.MatchString(entry.Name()) {
			continue
		}
		if err := backfillDayCosts(s, entry.Name(), estimate); err != nil {
			return err
		}
	}
	return nil
}

// recover rebuilds aggregates and resume bookkeeping from the existing day
// files. A plain copy wins if compression stopped after publishing its zstd
// copy but before removing the plain file. Row-level corruption is tolerated:
// malformed lines are skipped with a warning.
func (s *Store) recover() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.log.Warn("scan usage rollup directory", "dir", s.dir, "err", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !dayFileRe.MatchString(name) {
			continue
		}
		if strings.HasSuffix(name, compressedDaySuffix) {
			plain := strings.TrimSuffix(name, ".zstd")
			if _, err := os.Stat(filepath.Join(s.dir, plain)); err == nil {
				continue // the plain copy wins an interrupted compression
			} else if !os.IsNotExist(err) {
				s.log.Warn("stat plain usage rollup day", "file", plain, "err", err)
				continue
			}
		}
		data, err := readDayFile(filepath.Join(s.dir, name))
		if err != nil {
			s.log.Warn("read usage rollup day file", "file", name, "err", err)
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var probe struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(line), &probe); err != nil {
				s.log.Warn("skip malformed usage rollup line", "file", name, "err", err)
				continue
			}
			switch probe.Kind {
			case rowKindUsage:
				var row UsageRow
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					s.log.Warn("skip malformed usage rollup row", "file", name, "err", err)
					continue
				}
				s.applyUsageRow(&row)
				s.recoverRowState(&row)
			case rowKindQuota:
				var row QuotaRow
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					s.log.Warn("skip malformed quota rollup row", "file", name, "err", err)
					continue
				}
				// Rebuild the write-side dedupe state from the newest row per
				// provider window so a restart does not rewrite an unchanged
				// status as a fresh "change". Files arrive in sorted day
				// order and rows in append order, so the last one wins.
				s.lastQuota[quotaKey{provider: row.Provider, window: row.Window}] = quotaSeen{
					status:        row.Status,
					utilizationPc: int(row.Utilization * 100),
					resets:        row.ResetsAt,
				}
			default:
				s.log.Warn("skip unknown usage rollup row kind", "file", name, "kind", probe.Kind)
			}
		}
	}
}

// applyUsageRow folds one flushed usage row into the per-day aggregates. The
// caller holds s.mu.
func (s *Store) applyUsageRow(row *UsageRow) {
	foldUsageRow(s.dayAggregate(row.Day), row)
}

// foldUsageRow adds row to one day's aggregate.
func foldUsageRow(day *dayAggregate, row *UsageRow) {
	day.fold(&row.Delta)
	if row.TaskID != "" {
		for _, repo := range row.Repos {
			if day.repos[repo] == nil {
				day.repos[repo] = make(map[string]struct{})
			}
			day.repos[repo][row.TaskID] = struct{}{}
		}
		// A skill counts once per task. Codex re-reads a skill every turn
		// where Claude Code loads it once, and one task appends a row per
		// turn boundary, so summing reads would rank the harness, not the
		// skill.
		for skill := range row.SkillReads {
			if day.skills[skill] == nil {
				day.skills[skill] = make(map[string]struct{})
			}
			day.skills[skill][row.TaskID] = struct{}{}
		}
	}
	if row.Model != "" {
		day.modelBucket(row.Model).fold(&row.Delta)
	}
	if row.Harness != "" {
		day.harnessBucket(row.Harness).fold(&row.Delta)
	}
}

// recoverRowState rebuilds the resume bookkeeping from one recovered usage
// row: the per-task flush watermark and the cost total already reflected in
// flushed rows. The caller holds s.mu.
func (s *Store) recoverRowState(row *UsageRow) {
	if row.TaskID == "" {
		return
	}
	if row.Ts > 0 {
		if at := row.Ts.AsTime(); at.After(s.watermarks[row.TaskID]) {
			s.watermarks[row.TaskID] = at
		}
	}
	s.flushedCost[row.TaskID] += row.CostUSD
	if row.CostUSD != 0 && !row.CostEstimated {
		s.reportedCostTasks[row.TaskID] = struct{}{}
	}
}

func (s *Store) dayAggregate(day string) *dayAggregate {
	d := s.days[day]
	if d == nil {
		d = newDayAggregate()
		s.days[day] = d
	}
	return d
}

func newDayAggregate() *dayAggregate {
	return &dayAggregate{
		models:    make(map[string]*bucket),
		harnesses: make(map[string]*bucket),
		repos:     make(map[string]map[string]struct{}),
		skills:    make(map[string]map[string]struct{}),
	}
}

// recordQuotaLocked writes a quota row when the window's observed state
// changes. The caller holds s.mu.
func (s *Store) recordQuotaLocked(c *QuotaChange) {
	var resets Time
	if !c.ResetsAt.IsZero() {
		resets = NewTime(c.ResetsAt)
	}
	seen := quotaSeen{
		status:        c.Status,
		utilizationPc: int(c.Utilization * 100),
		resets:        resets,
	}
	key := quotaKey{provider: c.Provider, window: c.Window}
	if prev, ok := s.lastQuota[key]; ok && prev == seen {
		return
	}
	day := c.At.UTC().Format(dayFormat)
	if err := s.appendRowLocked(day, &QuotaRow{
		Kind:        rowKindQuota,
		Day:         day,
		Ts:          NewTime(c.At),
		Provider:    c.Provider,
		Window:      c.Window,
		Status:      c.Status,
		Utilization: c.Utilization,
		ResetsAt:    resets,
	}); err != nil {
		return
	}
	// Only a written row counts as seen, so a failed append is retried.
	s.lastQuota[key] = seen
}

// flushTaskLocked writes the task's pending buckets as one row per
// (day, model) group and updates the resume bookkeeping. The caller holds
// s.mu.
func (s *Store) flushTaskLocked(id string) {
	p := s.pending[id]
	if p == nil || len(p.buckets) == 0 {
		return
	}
	// Attribute the cost movement since the last flush to the delta's newest
	// model (the active one at flush time). The movement lands on disk with
	// that row: flushedCost advances only when its append succeeds.
	var costKey bucketKey
	if delta := p.costUSD - s.flushedCost[id] - p.costInFlight; delta != 0 {
		var newest *bucket
		for key, b := range p.buckets {
			if newest == nil || b.ts > newest.ts {
				newest, costKey = b, key
			}
		}
		if newest != nil {
			newest.CostUSD += delta
			p.costInFlight += delta
		}
	}

	keys := make([]bucketKey, 0, len(p.buckets))
	for key := range p.buckets {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b bucketKey) int {
		if a.day != b.day {
			return strings.Compare(a.day, b.day)
		}
		return strings.Compare(a.model, b.model)
	})
	dayWatermark := time.Time{}
	for _, key := range keys {
		b := p.buckets[key]
		row := UsageRow{
			Kind:    rowKindUsage,
			Day:     key.day,
			Ts:      b.ts,
			TaskID:  id,
			Harness: p.meta.Harness,
			Repos:   p.meta.Repos,
			Model:   key.model,
			Delta:   b.Delta,
		}
		if err := s.appendRowLocked(key.day, &row); err != nil {
			// Keep the bucket pending; the next flush retries it.
			continue
		}
		s.applyUsageRow(&row)
		if row.CostUSD != 0 {
			s.reportedCostTasks[id] = struct{}{}
		}
		if key == costKey {
			s.flushedCost[id] += p.costInFlight
			p.costInFlight = 0
		}
		if !b.synthetic {
			if ts := b.ts.AsTime(); ts.After(dayWatermark) {
				dayWatermark = ts
			}
		}
		delete(p.buckets, key)
	}
	if dayWatermark.After(s.watermarks[id]) {
		s.watermarks[id] = dayWatermark
	}
}

// appendRowLocked appends one row to the day file, atomically creating a
// missing day with its first complete line. Write errors are logged and
// returned, never fatal: the caller keeps the delta pending so aggregates only
// ever reflect rows on disk. The caller holds s.mu.
func (s *Store) appendRowLocked(day string, row any) error {
	data, err := json.Marshal(row)
	if err != nil {
		s.log.Warn("encode usage rollup row", "err", err)
		return err
	}
	data = append(data, '\n')
	f, ok := s.files[day]
	if !ok {
		target := filepath.Join(s.dir, day+".jsonl")
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if _, compressedErr := os.Stat(target + ".zstd"); compressedErr == nil {
				if err := s.restoreDayFile(target); err != nil {
					s.log.Warn("restore compressed usage day for late append", "day", day, "err", err)
					return err
				}
			} else if !os.IsNotExist(compressedErr) {
				s.log.Warn("stat compressed usage rollup day", "day", day, "err", compressedErr)
				return compressedErr
			} else {
				if err := writeFirstRowLocked(s.dir, target, data); err != nil {
					s.log.Warn("create usage rollup day file", "day", day, "err", err)
					return err
				}
				// The first row is durable after the atomic rename. Reopening the
				// append handle is an optimization; a failure here must not make the
				// caller retry and duplicate that committed row.
				f, err = os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is derived from the store directory and a validated day key.
				if err != nil {
					s.log.Warn("open usage rollup day file after create", "day", day, "err", err)
					return nil
				}
				s.files[day] = f
				return nil
			}
		} else if err != nil {
			s.log.Warn("stat usage rollup day file", "day", day, "err", err)
			return err
		}
		f, err = os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is derived from the store directory and a validated day key.
		if err != nil {
			s.log.Warn("open usage rollup day file", "day", day, "err", err)
			return err
		}
		s.files[day] = f
	}
	if _, err := f.Write(data); err != nil {
		s.log.Warn("append usage rollup row", "day", day, "err", err)
		return err
	}
	return nil
}

// writeFirstRowLocked atomically creates target with its first complete JSONL
// row. The caller holds s.mu.
func writeFirstRowLocked(dir, target string, data []byte) error {
	f, err := os.CreateTemp(dir, ".usage-first-row-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage rollup staging file: %w", err)
	}
	path := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write usage rollup staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close usage rollup staging file: %w", err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("publish first usage rollup row: %w", err)
	}
	cleanup = false
	return nil
}

// flushLoop flushes pending deltas periodically so long turns without a
// result still reach storage bounded by flushInterval.
func (s *Store) flushLoop() {
	defer close(s.flushDone)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopFlush:
			return
		case <-ticker.C:
			s.flushAll()
		}
	}
}

// compressionLoop handles daily maintenance without delaying periodic flushes.
func (s *Store) compressionLoop() {
	defer close(s.compressionDone)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	lastCompressionDay := ""
	for {
		select {
		case <-s.stopFlush:
			return
		case <-ticker.C:
			now := time.Now().UTC()
			if day := now.Format(dayFormat); day != lastCompressionDay {
				ran, err := s.tryCompressOldDays(now)
				if !ran {
					continue // a backfill owns the directory; retry next tick
				}
				if err != nil && !errors.Is(err, os.ErrClosed) {
					s.log.Warn("compress old usage days", "err", err)
				}
				lastCompressionDay = day
			}
		}
	}
}

// flushAll flushes every task's pending deltas.
func (s *Store) flushAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for id := range s.pending {
		s.flushTaskLocked(id)
	}
}

// dayRollupLocked snapshots one day's aggregate. The caller holds s.mu.
func (s *Store) dayRollupLocked(day string) DayRollup {
	d := s.days[day]
	out := DayRollup{
		Day:                      day,
		Tokens:                   d.TokenBuckets,
		Turns:                    d.Turns,
		ErroredTurns:             d.ErroredTurns,
		APIMs:                    d.APIMs,
		WallMs:                   d.WallMs,
		Compactions:              d.Compactions,
		SubagentSpawns:           d.Spawns,
		SubagentSpawnsBackground: d.SpawnsBackground,
		CostUSD:                  d.CostUSD,
		Models:                   make(map[string]ModelRollup, len(d.models)),
		Harnesses:                make(map[string]HarnessRollup, len(d.harnesses)),
		Repos:                    make(map[string]int, len(d.repos)),
		Skills:                   make(map[string]int, len(d.skills)),
		Tools:                    cloneCounts(d.ToolCalls),
		ToolTimings:              maps.Clone(d.ToolTimings),
	}
	for model, b := range d.models {
		out.Models[model] = ModelRollup{
			Tokens:        b.TokenBuckets,
			Turns:         b.Turns,
			CostUSD:       b.CostUSD,
			ContextWindow: b.ContextWindow,
		}
	}
	for harness, b := range d.harnesses {
		out.Harnesses[harness] = HarnessRollup{Tokens: b.TokenBuckets, Turns: b.Turns, CostUSD: b.CostUSD}
	}
	for repo, tasks := range d.repos {
		out.Repos[repo] = len(tasks)
	}
	for skill, tasks := range d.skills {
		out.Skills[skill] = len(tasks)
	}
	return out
}

// backfillIsDone checks the durable one-pass completion marker.
func (s *Store) backfillIsDone() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, os.ErrClosed
	}
	_, err := os.Stat(filepath.Join(s.dir, backfillSentinel))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat usage backfill sentinel: %w", err)
}

// createBackfillDay atomically publishes a complete staged day through the
// store's writer mutex. Streaming, JSON encoding, and staging happen before
// taking that lock, so they never delay live task dispatch.
func (s *Store) createBackfillDay(day string, staged *backfillDay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	target := filepath.Join(s.dir, day+".jsonl")
	if exists, err := dayFileExists(target); err != nil {
		return fmt.Errorf("stat usage rollup day %s: %w", day, err)
	} else if exists {
		return nil
	}
	if err := s.flushBackfillPendingDayLocked(day); err != nil {
		return err
	}
	// A matching pending bucket creates the day through normal append-only
	// ingestion. The staged history may contain the same record, so discard it
	// rather than duplicate the live row.
	if exists, err := dayFileExists(target); err != nil {
		return fmt.Errorf("stat usage rollup day %s: %w", day, err)
	} else if exists {
		return nil
	}
	if err := os.Rename(staged.path, target); err != nil {
		return fmt.Errorf("publish usage backfill day %s: %w", day, err)
	}
	staged.path = ""
	s.days[day] = staged.aggregate
	for id, at := range staged.watermarks {
		if at.After(s.watermarks[id]) {
			s.watermarks[id] = at
		}
	}
	for id, cost := range staged.costs {
		s.flushedCost[id] += cost
	}
	for id := range staged.reportedCosts {
		s.reportedCostTasks[id] = struct{}{}
	}
	return nil
}

func dayFileExists(plain string) (bool, error) {
	for _, path := range []string{plain, plain + ".zstd"} {
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

// flushBackfillPendingDayLocked flushes every task with a pending bucket for
// day. Full-task flushing preserves the usual newest-model cost attribution.
// The caller holds s.mu.
func (s *Store) flushBackfillPendingDayLocked(day string) error {
	for id, p := range s.pending {
		if !p.hasDay(day) {
			continue
		}
		s.flushTaskLocked(id)
		if p.hasDay(day) {
			return fmt.Errorf("flush pending usage before backfill day %s: task %s retains target-day buckets", day, id)
		}
	}
	return nil
}

// markBackfillDone atomically records successful completion after every
// missing day has been published.
func (s *Store) markBackfillDone() error {
	path, err := writeBackfillSentinelTemp(s.dir)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(path)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	target := filepath.Join(s.dir, backfillSentinel)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat usage backfill sentinel: %w", err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("publish usage backfill sentinel: %w", err)
	}
	published = true
	return nil
}

// compressOldDays seals plain days older than the grace window. A late append
// during compression leaves the plain file for a later pass; when both forms
// remain after an interruption, the plain file is authoritative.
func (s *Store) tryCompressOldDays(now time.Time) (bool, error) {
	if !s.backfillMu.TryLock() {
		return false, nil
	}
	defer s.backfillMu.Unlock()
	return true, s.compressOldDaysLocked(now)
}

func (s *Store) compressOldDaysLocked(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("scan usage days for compression: %w", err)
	}
	cutoff := now.UTC().AddDate(0, 0, -compressionGraceDays).Format(dayFormat)
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") || !dayFileRe.MatchString(name) {
			continue
		}
		day := strings.TrimSuffix(name, ".jsonl")
		if day >= cutoff {
			continue
		}
		if err := s.compressOldDay(day, filepath.Join(s.dir, name)); err != nil {
			if errors.Is(err, os.ErrClosed) {
				return err
			}
			errs = append(errs, fmt.Errorf("compress usage day %s: %w", day, err))
		}
	}
	return errors.Join(errs...)
}

// compressOldDay holds the writer lock only for source checks and publication.
func (s *Store) compressOldDay(day, path string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return os.ErrClosed
	}
	before, err := os.Stat(path)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	stagedPath, err := writeCompressedDay(path)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stagedPath) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil // a late append changed the source during compression
	}
	if f := s.files[day]; f != nil {
		delete(s.files, day)
		if err := f.Close(); err != nil {
			return fmt.Errorf("close usage day %s before compression: %w", day, err)
		}
	}
	if err := os.Rename(stagedPath, path+".zstd"); err != nil {
		return err
	}
	if err := syncDayDir(path); err != nil {
		return fmt.Errorf("sync compressed usage day before removing plain copy: %w", err)
	}
	return os.Remove(path)
}

// restoreDayFile publishes a complete plain copy before removing the
// compressed copy. An interrupted restore therefore always has one complete
// authoritative file.
func (s *Store) restoreDayFile(path string) error {
	compressed := path + ".zstd"
	in, err := os.Open(compressed) //nolint:gosec // day path inside the configured rollup directory.
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			s.log.Warn("close compressed usage day after restore", "file", compressed, "err", err)
		}
	}()
	dec, err := zstd.NewReader(in)
	if err != nil {
		return err
	}
	defer dec.Close()
	staged, err := os.CreateTemp(s.dir, ".usage-plain-*.tmp")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	defer func() { _ = os.Remove(stagedPath) }()
	if _, err := io.Copy(staged, dec); err != nil {
		return errors.Join(err, staged.Close())
	}
	if err := staged.Sync(); err != nil {
		return errors.Join(err, staged.Close())
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return err
	}
	if err := syncDayDir(path); err != nil {
		return fmt.Errorf("sync restored usage day before removing compressed copy: %w", err)
	}
	if err := os.Remove(compressed); err != nil {
		s.log.Warn("remove restored compressed usage day", "file", compressed, "err", err)
	}
	return nil
}

// taskPending holds one task's unflushed deltas. It persists for the task's
// lifetime so cost bookkeeping survives flushes.
type taskPending struct {
	meta    TaskMeta
	buckets map[bucketKey]*bucket
	costUSD float64 // newest observed task cost snapshot
	// costInFlight is cost movement already folded into pending rows but not
	// yet confirmed written; it is never re-added on a flush retry. See the
	// flushedCost invariant on Store.
	costInFlight float64
}

// fold records one event into the task's pending buckets.
func (p *taskPending) fold(e *Event, synthetic bool) {
	key := bucketKey{day: e.At.UTC().Format(dayFormat), model: e.Model}
	b := p.buckets[key]
	if b == nil {
		b = &bucket{}
		p.buckets[key] = b
	}
	if synthetic {
		b.synthetic = true
	}
	if ts := NewTime(e.At); ts > b.ts {
		b.ts = ts
	}
	b.fold(&e.Delta)
	p.costUSD = e.CostUSD
}

// hasDay reports whether p retains an unflushed bucket for day.
func (p *taskPending) hasDay(day string) bool {
	for key := range p.buckets {
		if key.day == day {
			return true
		}
	}
	return false
}

// bucketKey groups pending deltas by producer-time day and attribution model.
type bucketKey struct {
	day   string
	model string
}

// bucket accumulates one delta group. synthetic marks groups whose events
// had no producer time (timestamp-less adopted replay): their rows can never
// advance the task watermark.
type bucket struct {
	Delta

	ts        Time // newest producer time folded into the group
	synthetic bool
}

// dayAggregate accumulates flushed rows for one day.
type dayAggregate struct {
	bucket

	models    map[string]*bucket
	harnesses map[string]*bucket
	repos     map[string]map[string]struct{} // repo -> distinct task ids
	skills    map[string]map[string]struct{} // skill -> distinct task ids
}

func (a *dayAggregate) modelBucket(model string) *bucket {
	b := a.models[model]
	if b == nil {
		b = &bucket{}
		a.models[model] = b
	}
	return b
}

func (a *dayAggregate) harnessBucket(harness string) *bucket {
	b := a.harnesses[harness]
	if b == nil {
		b = &bucket{}
		a.harnesses[harness] = b
	}
	return b
}

// quotaKey identifies one provider quota window.
type quotaKey struct {
	provider string
	window   string
}

// quotaSeen is the last written state of one provider window; quota rows are
// written only when this state changes.
type quotaSeen struct {
	status        string
	utilizationPc int  // utilization rounded to hundredths
	resets        Time // 0 = unknown
}

func cloneCounts(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	maps.Copy(out, m)
	return out
}
