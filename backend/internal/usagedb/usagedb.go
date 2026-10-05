// Package usagedb owns the daily usage rollup over coding-agent task activity.
//
// Storage is one plain or zstd-compressed JSONL file of delta rows per UTC day,
// with in-memory aggregates for dashboard reads and per-task watermarks for
// lossless restart resume. Task logs stay the per-task record of truth; this package is the
// only cross-task aggregation surface. A one-time background backfill can
// publish neutral historical rows into missing day files, and a cost pass can
// fill missing amounts in existing files. Dashboard reads never scan task logs.
//
// Callers translate source records into Event, QuotaChange, or data.UsageRow values
// before passing them here, keeping this package independent of those sources.
package usagedb

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
	"github.com/maruel/ksid"
)

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

// Event is one ingest record from a task fold. At is the record's producer
// time (zero only for timestamp-less adopted replay), Replayed identifies a
// retained-history replay, Model is the attribution model (empty when
// unknown), and CostUSD is the task's live priced-cost snapshot at the fold
// — the store derives cost movement from consecutive snapshots and never
// prices itself.
type Event struct {
	At                   time.Time
	Replayed             bool
	Model                string
	CostUSD              float64
	TurnBoundary         bool   // a completed turn; the store flushes on it
	ToolStartID          string // tool-use correlation; not persisted in a usage row
	ToolName             string
	ToolResultID         string
	ToolNativeDurationMs int64
	ToolProducerTime     time.Time // zero when the harness did not timestamp the tool event
	Delta                data.Delta
}

// QuotaChange is one provider quota-window status change observed on a task.
type QuotaChange struct {
	At          time.Time
	Provider    string
	Window      string
	Status      string
	Utilization float64   // fraction in [0, 1]; -1 when unknown
	ResetsAt    time.Time // zero = unknown
}

// ModelRollup is one model's aggregated usage within a day. It carries the
// full delta fold so dashboard drill-down can filter every panel by model;
// Skills and Repos keep the day leaderboards' distinct-task semantics.
type ModelRollup struct {
	Tokens         data.TokenBuckets
	Turns          int
	ErroredTurns   int
	APIMs          int64
	WallMs         int64
	Compactions    int
	SubagentSpawns int
	CostUSD        float64
	ContextWindow  int
	ToolCalls      map[string]int
	ToolTimings    map[string]data.ToolTiming
	Skills         map[string]int // distinct tasks that read the skill under this model
	Repos          map[string]int // distinct tasks that touched the repo under this model
}

// HarnessRollup is one harness's aggregated usage within a day, mirroring
// ModelRollup plus the harness-by-model cross product so a harness filter can
// still list the models used under it.
type HarnessRollup struct {
	Tokens         data.TokenBuckets
	Turns          int
	ErroredTurns   int
	APIMs          int64
	WallMs         int64
	Compactions    int
	SubagentSpawns int
	CostUSD        float64
	ToolCalls      map[string]int
	ToolTimings    map[string]data.ToolTiming
	Skills         map[string]int // distinct tasks that read the skill under this harness
	Repos          map[string]int // distinct tasks that touched the repo under this harness
	Models         map[string]ModelRollup
}

// DayRollup is one UTC day's aggregated usage snapshot for dashboard reads.
// Maps are copies; callers may mutate them freely.
type DayRollup struct {
	Day                      string
	Tokens                   data.TokenBuckets
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
	ToolTimings              map[string]data.ToolTiming
}
