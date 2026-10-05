// Package data defines the persisted daily usage and quota row schemas.
//
// Field names and meanings are stable across released files. Evolve the
// unversioned format additively through optional fields.
package data

import "time"

// Time is a Unix millisecond timestamp that encodes as a plain int64.
// Zero means unknown; guard zero time.Time values before calling NewTime.
type Time int64

// NewTime returns t as a Time. Callers should avoid passing the zero
// time.Time, which predates the epoch and encodes as a large negative
// number; guard with IsZero and leave the Time at zero instead.
func NewTime(t time.Time) Time { return Time(t.UnixMilli()) }

// AsTime returns the wall-clock time of the timestamp.
func (t Time) AsTime() time.Time { return time.UnixMilli(int64(t)) }

// RowKind identifies a row before decoding its kind-specific fields.
type RowKind struct {
	Kind string `json:"kind"`
}

// TokenBuckets contains disjoint input buckets and output token counters.
// Unknown cache-write TTLs use the five-minute bucket.
type TokenBuckets struct {
	Input        int64 `json:"input_tokens,omitzero"`
	CacheWrite5m int64 `json:"cache_write_5m,omitzero"`
	CacheWrite1h int64 `json:"cache_write_1h,omitzero"`
	CacheRead    int64 `json:"cache_read,omitzero"`
	Output       int64 `json:"output_tokens,omitzero"`
	Reasoning    int64 `json:"reasoning_tokens,omitzero"`
}

// TotalInput returns the call's full input context size.
func (b TokenBuckets) TotalInput() int64 {
	return b.Input + b.CacheWrite5m + b.CacheWrite1h + b.CacheRead
}

// plus returns the field-wise sum of b and o.
func (b TokenBuckets) plus(o TokenBuckets) TokenBuckets {
	b.Input += o.Input
	b.CacheWrite5m += o.CacheWrite5m
	b.CacheWrite1h += o.CacheWrite1h
	b.CacheRead += o.CacheRead
	b.Output += o.Output
	b.Reasoning += o.Reasoning
	return b
}

// Delta contains counters accumulated since the previous flushed row.
// Rows are additive; ContextWindow retains its maximum during aggregation.
type Delta struct {
	TokenBuckets

	Turns            int                   `json:"turns,omitzero"`
	ErroredTurns     int                   `json:"errored_turns,omitzero"`
	APIMs            int64                 `json:"api_ms,omitzero"`
	WallMs           int64                 `json:"wall_ms,omitzero"`
	Compactions      int                   `json:"compactions,omitzero"`
	SkillReads       map[string]int        `json:"skill_reads,omitzero"`
	ToolCalls        map[string]int        `json:"tool_calls,omitzero"`
	ToolTimings      map[string]ToolTiming `json:"tool_timings,omitzero"`
	Spawns           int                   `json:"subagent_spawns,omitzero"`
	SpawnsBackground int                   `json:"subagent_spawns_background,omitzero"`
	ContextWindow    int                   `json:"context_window,omitzero"`
	CostUSD          float64               `json:"cost_usd,omitzero"`
}

// Add merges one delta into the accumulator: counters sum, maps merge, and
// the context window keeps its maximum.
func (d *Delta) Add(o *Delta) {
	d.TokenBuckets = d.plus(o.TokenBuckets)
	d.Turns += o.Turns
	d.ErroredTurns += o.ErroredTurns
	d.APIMs += o.APIMs
	d.WallMs += o.WallMs
	d.Compactions += o.Compactions
	d.Spawns += o.Spawns
	d.SpawnsBackground += o.SpawnsBackground
	if o.ContextWindow > d.ContextWindow {
		d.ContextWindow = o.ContextWindow
	}
	d.CostUSD += o.CostUSD
	for name, n := range o.SkillReads {
		if d.SkillReads == nil {
			d.SkillReads = make(map[string]int)
		}
		d.SkillReads[name] += n
	}
	for name, n := range o.ToolCalls {
		if d.ToolCalls == nil {
			d.ToolCalls = make(map[string]int)
		}
		d.ToolCalls[name] += n
	}
	for name, timing := range o.ToolTimings {
		if d.ToolTimings == nil {
			d.ToolTimings = make(map[string]ToolTiming)
		}
		current := d.ToolTimings[name]
		current.Count += timing.Count
		current.DurationMs += timing.DurationMs
		d.ToolTimings[name] = current
	}
}

// ToolTiming sums completed calls with a measured duration.
// Calls without a usable native duration or producer-time pair are excluded.
type ToolTiming struct {
	Count      int   `json:"count"`
	DurationMs int64 `json:"duration_ms"`
}

// UsageRow records a task/model delta for one UTC day.
// CostEstimated marks a historical estimate that a live cost may correct.
type UsageRow struct {
	Delta

	Kind          string   `json:"kind"` // always "usage"
	Day           string   `json:"day"`  // UTC day; matches the file the row lives in
	Ts            Time     `json:"ts"`   // Newest producer time covered by this delta.
	TaskID        string   `json:"task_id"`
	Harness       string   `json:"harness,omitempty"`
	Repos         []string `json:"repos,omitzero"`
	Model         string   `json:"model,omitempty"`
	CostEstimated bool     `json:"cost_estimated,omitempty"`
}

// QuotaRow records a provider quota-window status change.
type QuotaRow struct {
	Kind        string  `json:"kind"` // always "quota"
	Day         string  `json:"day"`
	Ts          Time    `json:"ts"` // When the change was observed.
	Provider    string  `json:"provider"`
	Window      string  `json:"window,omitempty"`
	Status      string  `json:"status"`
	Utilization float64 `json:"utilization,omitzero"` // fraction in [0, 1]; -1 when unknown
	ResetsAt    Time    `json:"resets_at,omitzero"`   // When the window resets; 0 = unknown
}
