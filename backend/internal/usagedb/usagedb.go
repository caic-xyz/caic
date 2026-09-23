// Package usagedb owns the daily usage rollup over coding-agent task activity.
//
// Storage is one JSONL file of delta rows per UTC day, with in-memory
// aggregates for dashboard reads and per-task watermarks for lossless restart
// resume. Task logs stay the per-task record of truth; this package is the
// only cross-task aggregation surface. A one-time background backfill can
// publish neutral historical rows into missing day files, and a cost pass can
// fill missing amounts in existing files. Dashboard reads never scan task logs.
//
// Callers translate source records into Event, QuotaChange, or UsageRow values
// before passing them here, keeping this package independent of those sources.
package usagedb

import (
	"time"

	"github.com/maruel/ksid"
)

// Rollup row schema stability: rows are durable records. Keep JSON field
// names and their meanings backward-compatible with files written by
// released binaries; evolve additively with new optional fields only. The
// "kind" field discriminates usage rows from future row kinds.

const (
	rowKindUsage = "usage"
	rowKindQuota = "quota"

	// dayFormat is the UTC day key used for file names and row day fields.
	dayFormat = "2006-01-02"

	// flushInterval is how often pending deltas flush without a turn boundary.
	flushInterval = 30 * time.Second
)

// TaskMeta is the immutable task identity the rollup needs.
type TaskMeta struct {
	TaskID         ksid.ID
	Harness        string
	Repos          []string // repo names, index 0 = primary
	RequestedModel string   // fallback attribution model when no model was reported
}

// Time is a Unix millisecond timestamp that encodes to JSON as a plain
// int64. The zero value encodes as 0; build one from a wall-clock time with
// NewTime and convert back with AsTime.
type Time int64

// NewTime returns t as a Time. Callers should avoid passing the zero
// time.Time, which predates the epoch and encodes as a large negative
// number; guard with IsZero and leave the Time at zero instead.
func NewTime(t time.Time) Time { return Time(t.UnixMilli()) }

// AsTime returns the wall-clock time of the timestamp.
func (t Time) AsTime() time.Time { return time.UnixMilli(int64(t)) }

// TokenBuckets is a set of token counters. The four input buckets are
// disjoint: total input context for a call is Input + CacheWrite5m +
// CacheWrite1h + CacheRead. Cache writes with an unknown TTL land in the
// five-minute bucket.
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

// Delta is one set of usage counters: what one task accumulated for one
// model on one UTC day since its previously flushed delta. It is the shared
// vocabulary of ingest events and usage-row payloads.
type Delta struct {
	TokenBuckets

	Turns            int            `json:"turns,omitzero"`
	ErroredTurns     int            `json:"errored_turns,omitzero"`
	APIMs            int64          `json:"api_ms,omitzero"`
	WallMs           int64          `json:"wall_ms,omitzero"`
	Compactions      int            `json:"compactions,omitzero"`
	SkillReads       map[string]int `json:"skill_reads,omitzero"`
	ToolCalls        map[string]int `json:"tool_calls,omitzero"`
	Spawns           int            `json:"subagent_spawns,omitzero"`
	SpawnsBackground int            `json:"subagent_spawns_background,omitzero"`
	ContextWindow    int            `json:"context_window,omitzero"`
	CostUSD          float64        `json:"cost_usd,omitzero"`
}

// Add merges o into d: counters sum, maps merge, and context window keeps its
// maximum.
func (d *Delta) Add(o *Delta) { d.fold(o) }

// fold merges one delta into the accumulator: counters sum, maps merge, and
// the context window keeps its maximum.
func (d *Delta) fold(o *Delta) {
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
}

// Event is one ingest record from a task fold. At is the record's producer
// time (zero only for timestamp-less adopted replay), Replayed identifies a
// retained-history replay, Model is the attribution model (empty when
// unknown), and CostUSD is the task's live priced-cost snapshot at the fold
// — the store derives cost movement from consecutive snapshots and never
// prices itself.
type Event struct {
	At           time.Time
	Replayed     bool
	Model        string
	CostUSD      float64
	TurnBoundary bool // a completed turn; the store flushes on it
	Delta        Delta
}

// QuotaChange is one provider quota-window status change observed on a task.
type QuotaChange struct {
	At          time.Time
	Provider    string
	Window      string
	Status      string
	Utilization float64   // fraction in [0, 1]
	ResetsAt    time.Time // zero = unknown
}

// UsageRow is one rollup delta: the usage one task accumulated for one model
// on one UTC day since the task's previously flushed row. Rows are deltas,
// not snapshots; the dashboard sums them.
//
// CostUSD is the task's priced-cost movement since its previous row,
// attributed to the delta's newest model. Historical rows without a reported
// amount can hold an estimate instead, marked by CostEstimated.
// CostEstimated marks a missing historical cost filled from current published
// token prices. A later live cumulative snapshot may correct that estimate.
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

// QuotaRow records one provider quota-window status change. Rows are written
// when the status, rounded utilization, or reset time changes for a provider
// window.
type QuotaRow struct {
	Kind        string  `json:"kind"` // always "quota"
	Day         string  `json:"day"`
	Ts          Time    `json:"ts"` // When the change was observed.
	Provider    string  `json:"provider"`
	Window      string  `json:"window,omitempty"`
	Status      string  `json:"status"`
	Utilization float64 `json:"utilization,omitzero"` // fraction in [0, 1]
	ResetsAt    Time    `json:"resets_at,omitzero"`   // When the window resets; 0 = unknown
}

// ModelRollup is one model's aggregated usage within a day.
type ModelRollup struct {
	Tokens        TokenBuckets
	Turns         int
	CostUSD       float64
	ContextWindow int
}

// HarnessRollup is one harness's aggregated usage within a day.
type HarnessRollup struct {
	Tokens  TokenBuckets
	Turns   int
	CostUSD float64
}

// DayRollup is one UTC day's aggregated usage snapshot for dashboard reads.
// Maps are copies; callers may mutate them freely.
type DayRollup struct {
	Day                      string
	Tokens                   TokenBuckets
	Turns                    int
	ErroredTurns             int
	APIMs                    int64
	WallMs                   int64
	Compactions              int
	SubagentSpawns           int
	SubagentSpawnsBackground int
	CostUSD                  float64
	Models                   map[string]ModelRollup
	Harnesses                map[string]HarnessRollup
	Repos                    map[string]int // distinct tasks that touched the repo that day
	Skills                   map[string]int // distinct tasks that read the skill that day
	Tools                    map[string]int
}
