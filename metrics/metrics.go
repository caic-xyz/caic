// Package metrics aggregates numeric measurements of named operations.
//
// It is the shared instrumentation contract for caic binaries. The caic server
// records into it, and the standalone voice gateway can use the same package;
// it depends only on the standard library so either can import it.
//
// Operation names are hierarchical dotted identifiers that describe the work
// performed, not the caller that requested it. Starting a container is
// "container.launch" whether the frontend, an MCP tool, a voice command, or
// task automation triggered it; caller-specific work forms its own name, such
// as "mcp.tool.task_create".
//
// A name denotes the thing measured, and its duration is one measurement of it.
// "container.disk_usage" is therefore the query and not the byte count it
// returns; the byte count is a second series named for what it measures.
//
// Kinds mirror the OpenTelemetry instrument kinds, and there is deliberately no
// duration kind: a duration is a histogram measured in seconds, which is why
// Measurement pairs a kind with a unit rather than encoding the unit in the
// kind.
//
// Names and attribute values must stay low cardinality. Never encode task IDs,
// repository paths, user data, or other unbounded values: exporters such as
// OpenTelemetry and Prometheus treat every distinct name and label combination
// as a time series, so an unbounded set leaks memory and remote storage. A
// varying dimension belongs in an Attr with a bounded value, not in the name.
//
// Store is a bounded in-memory aggregation of observations. Durable sinks, such
// as the metricsdb package, and exporters also implement Recorder.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sampleLimit caps retained observations per series. Percentiles describe
// recent behavior once a series exceeds the limit.
const sampleLimit = 1024

// Outcome classifies a recorded result.
type Outcome string

// Operation outcomes.
const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
)

// Kind is how a measurement aggregates, mirroring the OpenTelemetry instrument
// kinds.
type Kind string

// Measurement kinds.
const (
	// KindHistogram aggregates recorded amounts into a distribution. Durations
	// and sizes are histograms.
	KindHistogram Kind = "histogram"
	// KindCounter sums non-negative increments of a monotonically increasing
	// quantity.
	KindCounter Kind = "counter"
	// KindUpDownCounter sums increments that may be negative, such as the
	// number of active tasks.
	KindUpDownCounter Kind = "updowncounter"
	// KindGauge samples a non-additive value, where only the latest amount and
	// the observed range are meaningful.
	KindGauge Kind = "gauge"
)

// Unit is the unit a measurement is expressed in, in OpenTelemetry's UCUM
// notation.
type Unit string

// Measurement units.
const (
	// UnitSeconds is the unit of durations.
	UnitSeconds Unit = "s"
	// UnitBytes is the unit of sizes.
	UnitBytes Unit = "By"
	// UnitCount is the unit of dimensionless counts.
	UnitCount Unit = "1"
)

// Measurement is one recorded amount: how it aggregates, the unit it is
// expressed in, and the amount in that unit.
type Measurement struct {
	Kind   Kind
	Unit   Unit
	Amount float64
}

// Duration returns a histogram measurement of how long an operation took.
//
// The amount is seconds quantized to the nearest microsecond: the measured
// operations run for milliseconds or longer, so nanoseconds would be precision
// they do not carry, and quantizing keeps a stored amount free of
// floating-point noise.
func Duration(d time.Duration) Measurement {
	return Measurement{
		Kind:   KindHistogram,
		Unit:   UnitSeconds,
		Amount: float64(d.Round(time.Microsecond).Microseconds()) / 1e6,
	}
}

// Bytes returns a histogram measurement of a size in bytes.
func Bytes(n int64) Measurement {
	return Measurement{Kind: KindHistogram, Unit: UnitBytes, Amount: float64(n)}
}

// Count returns a counter measurement of n events, which must not be negative.
func Count(n int64) Measurement {
	return Measurement{Kind: KindCounter, Unit: UnitCount, Amount: float64(n)}
}

// UpDownCount returns an updowncounter measurement of n events, which may be
// negative.
func UpDownCount(n int64) Measurement {
	return Measurement{Kind: KindUpDownCounter, Unit: UnitCount, Amount: float64(n)}
}

// Gauge returns a gauge measurement of a non-additive amount in unit.
func Gauge(amount float64, unit Unit) Measurement {
	return Measurement{Kind: KindGauge, Unit: unit, Amount: amount}
}

// Attr is one bounded dimension of an observation, such as the container
// runtime or the forge that performed the work.
//
// Keys are dotted identifiers namespaced by the subsystem that owns them, for
// example "container.runtime". Values must come from a small, closed set:
// exporters turn attributes into label values, so an unbounded value is a
// cardinality bug.
type Attr struct {
	Key   string
	Value string
}

// DedupAttrs returns the attribute set with later duplicates winning, matching
// OpenTelemetry. It returns nil when attrs is empty.
//
// The set is part of a series identity, so every sink must agree on it or the
// same operation would be split differently by the in-memory view and by a
// durable one.
func DedupAttrs(attrs []Attr) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	set := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		set[attr.Key] = attr.Value
	}
	return set
}

// Recorder records one measurement of a named operation.
//
// Attrs add bounded dimensions to the series; pass none when the operation has
// no dimension worth separating. The context carries the caller's trace context
// so exporters can attach exemplars; recording must still succeed when the
// context is cancelled.
//
// TODO(observability): Implement an OpenTelemetry exporter as another Recorder,
// wired beside the local store and log. It maps Kind and Unit onto the matching
// instrument, folds Outcome into an attribute because OpenTelemetry has no
// outcome concept, and accumulates its own histogram buckets rather than
// publishing these percentiles; see Series.
type Recorder interface {
	Record(ctx context.Context, name string, outcome Outcome, m Measurement, attrs ...Attr)
}

// Resource identifies the process that produced observations so one backend
// can separate services. Each binary supplies its own.
//
// Resource describes the process, never a single observation. One process can
// serve several container runtimes or forges at once, so those belong in Attr.
type Resource struct {
	// ServiceName is the emitting binary, such as "caic" or
	// "caic-voice-gateway".
	ServiceName string
	// ServiceVersion is the build version, when known.
	ServiceVersion string
	// Host is the machine the process runs on, when known.
	Host string
}

// OutcomeOf classifies err as an operation outcome.
func OutcomeOf(err error) Outcome {
	if err != nil {
		return OutcomeError
	}
	return OutcomeOK
}

// Nop discards observations. Use it when metrics are not wanted.
type Nop struct{}

// Record implements Recorder.
func (Nop) Record(context.Context, string, Outcome, Measurement, ...Attr) {}

// Multi returns a Recorder that forwards every observation to each recorder,
// so a process can keep a local view and a durable log at the same time.
//
// Every recorder is required. A nil entry is an error rather than a silently
// dropped destination.
func Multi(recs ...Recorder) (Recorder, error) {
	if len(recs) == 0 {
		return nil, errors.New("at least one recorder is required")
	}
	for i, rec := range recs {
		if rec == nil {
			return nil, fmt.Errorf("recorder %d is nil", i)
		}
	}
	return multi(slices.Clone(recs)), nil
}

// Series is one aggregated measurement series.
//
// Sum, Min, P50, P95, Max, and Last are all expressed in Unit. Which of them
// mean anything depends on Kind: a histogram is read through its distribution, a
// counter through its sum, and a gauge through its latest amount and range.
//
// P50 and P95 are exact nearest-rank percentiles over the retained amounts. They
// are a local view and not an aggregation to export: quantiles cannot be merged
// across processes or time windows, which is why OpenTelemetry keeps Summary
// only for compatibility with older formats and recommends a bucketed or
// exponential histogram instead. An exporter must accumulate its own buckets
// from the measurements it receives rather than publishing these.
type Series struct {
	Name    string
	Outcome Outcome
	Kind    Kind
	Unit    Unit
	Attrs   map[string]string

	Count   int64
	Samples int64
	Sum     float64
	Min     float64
	P50     float64
	P95     float64
	Max     float64
	Last    float64
}

// Store aggregates live measurements and retained historical measurements for
// one process.
//
// It is safe for concurrent use and keeps memory flat: each series retains at
// most sampleLimit amounts.
type Store struct {
	// Since is the earliest observation in the store. It is set at creation and
	// can move earlier while retained observations are restored during startup.
	Since time.Time
	// Resource identifies the process that produced the observations. It is
	// set by NewStore and must not change afterwards.
	Resource Resource

	mu     sync.Mutex
	series map[seriesKey]*series
}

// NewStore creates an empty store that attributes observations to resource.
func NewStore(resource Resource) *Store {
	return &Store{
		Since:    time.Now().UTC(),
		Resource: resource,
		series:   make(map[seriesKey]*series),
	}
}

// Record adds one measurement for name and outcome. It implements Recorder.
//
// Kind and Unit are part of the series identity, so recording one name as both,
// say, a duration and a size keeps them apart instead of merging two
// incommensurable quantities.
func (s *Store) Record(_ context.Context, name string, outcome Outcome, m Measurement, attrs ...Attr) {
	s.record(name, outcome, m, attrs)
}

// Restore adds an observation that was recorded at before this process
// started. It moves Since earlier when needed.
//
// Call Restore only during startup, before the store is shared with request
// handlers. Record is the normal path for live observations.
func (s *Store) Restore(at time.Time, name string, outcome Outcome, m Measurement, attrs ...Attr) {
	attrsKey := canonicalKey(attrs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.Before(s.Since) {
		s.Since = at
	}
	s.recordLocked(attrsKey, name, outcome, m, attrs)
}

// Snapshot returns the aggregated series in name order.
//
// The store does not rank series by magnitude, because it holds several units at
// once and seconds are not comparable with bytes.
func (s *Store) Snapshot() []Series {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]snapshotEntry, 0, len(s.series))
	for key, entry := range s.series {
		entries = append(entries, snapshotEntry{key: key, series: entry.snapshot(&key)})
	}
	slices.SortFunc(entries, func(a, b snapshotEntry) int {
		if c := strings.Compare(a.key.name, b.key.name); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.key.outcome), string(b.key.outcome)); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.key.kind), string(b.key.kind)); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.key.unit), string(b.key.unit)); c != 0 {
			return c
		}
		return strings.Compare(a.key.attrs, b.key.attrs)
	})
	out := make([]Series, 0, len(entries))
	for i := range entries {
		out = append(out, entries[i].series)
	}
	return out
}

func (s *Store) record(name string, outcome Outcome, m Measurement, attrs []Attr) {
	attrsKey := canonicalKey(attrs)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(attrsKey, name, outcome, m, attrs)
}

func (s *Store) recordLocked(attrsKey, name string, outcome Outcome, m Measurement, attrs []Attr) {
	key := seriesKey{name: name, outcome: outcome, kind: m.Kind, unit: m.Unit, attrs: attrsKey}
	entry := s.series[key]
	if entry == nil {
		entry = &series{attributes: DedupAttrs(attrs)}
		s.series[key] = entry
	}
	entry.observe(m.Amount)
}

// snapshotEntry pairs an aggregated series with the key it was aggregated
// under, so Snapshot can order series without rebuilding the key.
type snapshotEntry struct {
	key    seriesKey
	series Series
}

// multi fans observations out to several recorders.
type multi []Recorder

func (m multi) Record(ctx context.Context, name string, outcome Outcome, mm Measurement, attrs ...Attr) {
	for _, rec := range m {
		rec.Record(ctx, name, outcome, mm, attrs...)
	}
}

type seriesKey struct {
	name    string
	outcome Outcome
	kind    Kind
	unit    Unit
	attrs   string
}

// series accumulates amounts for one key.
type series struct {
	attributes map[string]string
	count      int64
	sum        float64
	last       float64
	next       int
	samples    []float64
}

func (s *series) observe(amount float64) {
	s.count++
	s.sum += amount
	s.last = amount
	if len(s.samples) < sampleLimit {
		s.samples = append(s.samples, amount)
		return
	}
	s.samples[s.next] = amount
	s.next = (s.next + 1) % sampleLimit
}

func (s *series) snapshot(key *seriesKey) Series {
	out := Series{
		Name:    key.name,
		Outcome: key.outcome,
		Kind:    key.kind,
		Unit:    key.unit,
		Attrs:   maps.Clone(s.attributes),
		Count:   s.count,
		Samples: int64(len(s.samples)),
		Sum:     s.sum,
		Last:    s.last,
	}
	if len(s.samples) == 0 {
		return out
	}
	sorted := slices.Clone(s.samples)
	slices.Sort(sorted)
	out.Min = sorted[0]
	out.Max = sorted[len(sorted)-1]
	out.P50 = percentile(sorted, 0.50)
	out.P95 = percentile(sorted, 0.95)
	return out
}

// canonicalKey builds the series key for attrs, with later duplicates winning so
// the key always agrees with DedupAttrs.
//
// Most observations carry no attribute or exactly one, so those cases skip the
// map and sort the general path needs. The attribute set itself is only
// materialized when a series is seen for the first time.
func canonicalKey(attrs []Attr) string {
	switch len(attrs) {
	case 0:
		return ""
	case 1:
		return strconv.Quote(attrs[0].Key) + "=" + strconv.Quote(attrs[0].Value) + " "
	}
	set := DedupAttrs(attrs)
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(set)) {
		b.WriteString(strconv.Quote(key))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(set[key]))
		b.WriteByte(' ')
	}
	return b.String()
}

// percentile returns the nearest-rank percentile of ascending samples, exact
// over the retained window. See Series for why this is a local view rather than
// something an exporter should publish.
func percentile(sorted []float64, p float64) float64 {
	rank := max(int(math.Ceil(p*float64(len(sorted)))), 1)
	return sorted[rank-1]
}
