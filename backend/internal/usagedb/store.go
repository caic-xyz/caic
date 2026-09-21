// Store is the usage rollup sink: append-only daily JSONL files, in-memory aggregates, restart watermarks, and purge discard.
//
// It implements the task.RollupSink ingestion contract. Task folds call
// Observe under their own task mutex, and the store serializes all file and
// aggregate access behind a single writer mutex.

package usagedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// dayFileRe matches a rollup day file name.
var dayFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

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

	mu         sync.Mutex
	files      map[string]*os.File      // day -> open append handle
	days       map[string]*dayAggregate // day -> aggregates over flushed rows
	watermarks map[string]time.Time     // task id -> newest producer time covered by flushed rows
	// flushedCost and the task's pending.costInFlight always sum to the
	// newest accounted cost snapshot: flushedCost alone is only the movement
	// confirmed written to disk.
	flushedCost map[string]float64
	pending     map[string]*taskPending // task id -> unflushed deltas
	lastQuota   map[quotaKey]quotaSeen  // provider window -> last written quota state
	closed      bool
	stopFlush   chan struct{}
	flushDone   chan struct{}
}

// New opens the rollup store: it recovers aggregates and watermarks from any
// existing day files and starts the background flush ticker.
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
		log:         cfg.Log,
		dir:         cfg.Dir,
		files:       make(map[string]*os.File),
		days:        make(map[string]*dayAggregate),
		watermarks:  make(map[string]time.Time),
		flushedCost: make(map[string]float64),
		pending:     make(map[string]*taskPending),
		lastQuota:   make(map[quotaKey]quotaSeen),
		stopFlush:   make(chan struct{}),
		flushDone:   make(chan struct{}),
	}
	s.recover()
	go s.flushLoop()
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
// Flushed rows remain immutable in their daily files and in-memory
// aggregates. Callers must stop forwarding the task's events before calling
// Discard; the task package enforces that boundary when a purge begins.
func (s *Store) Discard(meta TaskMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || meta.TaskID.IsZero() {
		return
	}
	id := meta.TaskID.String()
	delete(s.pending, id)
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

// recover rebuilds aggregates and resume bookkeeping from the existing day
// files. Row-level corruption (a truncated trailing line) is tolerated:
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
		data, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // name is a directory entry validated against dayFileRe.
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
	day := s.dayAggregate(row.Day)
	day.fold(&row.Delta)
	if row.TaskID != "" {
		for _, repo := range row.Repos {
			if day.repos[repo] == nil {
				day.repos[repo] = make(map[string]struct{})
			}
			day.repos[repo][row.TaskID] = struct{}{}
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
}

func (s *Store) dayAggregate(day string) *dayAggregate {
	d := s.days[day]
	if d == nil {
		d = &dayAggregate{
			models:    make(map[string]*bucket),
			harnesses: make(map[string]*bucket),
			repos:     make(map[string]map[string]struct{}),
		}
		s.days[day] = d
	}
	return d
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

// appendRowLocked appends one row to the day file, opening the file on
// demand. Write errors are logged and returned, never fatal: the caller
// keeps the delta pending so aggregates only ever reflect rows on disk. The
// caller holds s.mu.
func (s *Store) appendRowLocked(day string, row any) error {
	f, ok := s.files[day]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(s.dir, day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is derived from the store directory and a validated day key.
		if err != nil {
			s.log.Warn("open usage rollup day file", "day", day, "err", err)
			return err
		}
		s.files[day] = f
	}
	data, err := json.Marshal(row)
	if err != nil {
		s.log.Warn("encode usage rollup row", "err", err)
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		s.log.Warn("append usage rollup row", "day", day, "err", err)
		return err
	}
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
		Skills:                   cloneCounts(d.SkillReads),
		Tools:                    cloneCounts(d.ToolCalls),
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
	return out
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
