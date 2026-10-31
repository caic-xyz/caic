// Task usage rollup contract and discard implementation.
//
// The sink owns accumulation, day bucketing, and flush timing; the task
// only translates timeline messages into rollup events and forwards them.

package task

import (
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

// RollupSink consumes task messages for the cross-task usage rollup. Tasks
// always carry a non-nil sink: taskrun.NewTask defaults to DiscardRollup and the task
// manager replaces it with the wired implementation.
type RollupSink interface {
	// Observe records one translated rollup event. Called with the task
	// mutex held, so implementations must not call back into the task and
	// should keep per-call work bounded.
	Observe(meta usagedb.TaskMeta, e *usagedb.Event)
	// ObserveQuota records one provider quota-window status change.
	ObserveQuota(c *usagedb.QuotaChange)
	// Discard drops a purged task's unflushed data and resume bookkeeping.
	// The task guarantees no later events reach the sink for this task.
	Discard(meta usagedb.TaskMeta)
	// Close flushes all pending deltas and releases resources.
	Close() error
}

// DiscardRollup ignores every rollup record. Task execution defaults to it so tasks
// are always safe to fold without a wired implementation; the task manager
// replaces it with the real sink at construction.
type DiscardRollup struct{}

// Observe implements RollupSink by discarding the event.
func (DiscardRollup) Observe(usagedb.TaskMeta, *usagedb.Event) {}

// ObserveQuota implements RollupSink by discarding the change.
func (DiscardRollup) ObserveQuota(*usagedb.QuotaChange) {}

// Discard implements RollupSink by discarding the purge notification.
func (DiscardRollup) Discard(usagedb.TaskMeta) {}

// Close implements RollupSink with no resources to release.
func (DiscardRollup) Close() error { return nil }
