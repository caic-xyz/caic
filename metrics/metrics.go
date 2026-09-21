// Package metrics aggregates operation duration observations.
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
// Names and attribute values must stay low cardinality. Never encode task IDs,
// repository paths, user data, or other unbounded values: exporters such as
// OpenTelemetry and Prometheus treat every distinct name and label combination
// as a time series, so an unbounded set leaks memory and remote storage. A
// varying dimension belongs in an Attr with a bounded value, not in the
// name.
//
// Store is a single-process view of recent observations. Durable sinks, such
// as the metricsdb package, and exporters also implement Recorder.
package metrics

import (
	"cmp"
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

// Outcome classifies a recorded operation result.
type Outcome string

// Operation outcomes.
const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
)

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

// Recorder records one operation duration observation.
//
// Attrs add bounded dimensions to the series; pass none when the
// operation has no dimension worth separating. The context carries the
// caller's trace context so exporters can attach exemplars; recording must
// still succeed when the context is cancelled.
type Recorder interface {
	Record(ctx context.Context, name string, outcome Outcome, d time.Duration, attrs ...Attr)
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
func (Nop) Record(context.Context, string, Outcome, time.Duration, ...Attr) {}

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

// Series is one aggregated operation series.
type Series struct {
	Name    string
	Outcome Outcome
	Attrs   map[string]string
	Calls   int64
	Samples int64
	Min     time.Duration
	P50     time.Duration
	P95     time.Duration
	Max     time.Duration
}

// Store aggregates operation duration observations for one process.
//
// It is safe for concurrent use and keeps memory flat: each series retains at
// most sampleLimit observations.
type Store struct {
	// Since is when the store started recording; it never changes.
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

// Record adds one observation for name, outcome, and attrs. It implements
// Recorder.
func (s *Store) Record(_ context.Context, name string, outcome Outcome, d time.Duration, attrs ...Attr) {
	attrsKey, attrSet := canonicalAttrs(attrs)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := seriesKey{name: name, outcome: outcome, attrs: attrsKey}
	entry := s.series[key]
	if entry == nil {
		entry = &series{attrs: attrSet}
		s.series[key] = entry
	}
	entry.observe(d)
}

// Snapshot returns the aggregated series, slowest p95 first.
func (s *Store) Snapshot() []Series {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]snapshotEntry, 0, len(s.series))
	for key, entry := range s.series {
		entries = append(entries, snapshotEntry{key: key, series: entry.snapshot(key)})
	}
	slices.SortFunc(entries, func(a, b snapshotEntry) int {
		if c := cmp.Compare(b.series.P95, a.series.P95); c != 0 {
			return c
		}
		if c := strings.Compare(a.key.name, b.key.name); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.key.outcome), string(b.key.outcome)); c != 0 {
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

// snapshotEntry pairs an aggregated series with the key it was aggregated
// under, so Snapshot can sort without repeating the aggregation.
type snapshotEntry struct {
	key    seriesKey
	series Series
}

// multi fans observations out to several recorders.
type multi []Recorder

func (m multi) Record(ctx context.Context, name string, outcome Outcome, d time.Duration, attrs ...Attr) {
	for _, rec := range m {
		rec.Record(ctx, name, outcome, d, attrs...)
	}
}

type seriesKey struct {
	name    string
	outcome Outcome
	attrs   string
}

// series accumulates observations for one key.
type series struct {
	attrs   map[string]string
	calls   int64
	next    int
	samples []time.Duration
}

func (s *series) observe(d time.Duration) {
	s.calls++
	if len(s.samples) < sampleLimit {
		s.samples = append(s.samples, d)
		return
	}
	s.samples[s.next] = d
	s.next = (s.next + 1) % sampleLimit
}

func (s *series) snapshot(key seriesKey) Series {
	out := Series{
		Name:    key.name,
		Outcome: key.outcome,
		Attrs:   maps.Clone(s.attrs),
		Calls:   s.calls,
		Samples: int64(len(s.samples)),
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

// canonicalAttrs returns a stable series key and the deduplicated attribute
// set. Later duplicates win, matching OpenTelemetry. Both results are empty
// when attrs is empty, which is the common case.
func canonicalAttrs(attrs []Attr) (key string, set map[string]string) {
	set = DedupAttrs(attrs)
	if set == nil {
		return "", nil
	}
	var b strings.Builder
	for _, attrKey := range slices.Sorted(maps.Keys(set)) {
		b.WriteString(strconv.Quote(attrKey))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(set[attrKey]))
		b.WriteByte(' ')
	}
	return b.String(), set
}

// percentile returns the nearest-rank percentile of ascending samples.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := max(int(math.Ceil(p*float64(len(sorted)))), 1)
	return sorted[rank-1]
}
