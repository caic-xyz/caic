// Entry wraps a task and its result for the Manager registry.

package tasks

import (
	"sync"

	"github.com/caic-xyz/caic/backend/internal/task"
)

// Entry is a single registered task plus its mutable lifecycle state.
//
// Concurrency: the task pointer, loadedTask pointer, cleanupOnce, and
// loadedTaskOnce are immutable references after construction (the values they
// point to may mutate, but the pointers themselves don't); result, done,
// doneClosed, and monitorBranch are guarded by mu and must only be accessed
// through methods. Code outside this package never touches the fields directly.
type Entry struct {
	mu sync.Mutex

	// Immutable after construction.
	task           *task.Task
	loadedTask     *task.LoadedTask
	loadedTaskOnce *sync.Once

	// Guarded by mu.
	result        *task.Result
	done          chan struct{}
	doneClosed    bool
	monitorBranch string
	cleanupOnce   *sync.Once
}

// NewEntry creates an Entry. done is fresh; result is nil.
func NewEntry(t *task.Task, lt *task.LoadedTask) *Entry {
	return &Entry{
		task:           t,
		loadedTask:     lt,
		done:           make(chan struct{}),
		cleanupOnce:    new(sync.Once),
		loadedTaskOnce: new(sync.Once),
	}
}

// newPurgedEntry creates an Entry for a purged task loaded from disk. done is
// pre-closed, result is set, and loadedTask is wired for lazy message loading.
func newPurgedEntry(t *task.Task, r *task.Result, lt *task.LoadedTask) *Entry {
	done := make(chan struct{})
	close(done)
	return &Entry{
		task:           t,
		loadedTask:     lt,
		loadedTaskOnce: new(sync.Once),
		result:         r,
		done:           done,
		doneClosed:     true,
		cleanupOnce:    new(sync.Once),
	}
}

// Task returns the underlying task. The returned *task.Task is itself
// concurrency-safe via its own internal locking.
func (e *Entry) Task() *task.Task { return e.task }

// LoadedTask returns the on-disk log handle for tasks restored from disk, or nil.
func (e *Entry) LoadedTask() *task.LoadedTask { return e.loadedTask }

// Done returns the channel that closes when the task reaches a terminal
// state. After Reset (Revive), this returns a fresh channel; goroutines that
// captured the previous channel will see a stale (already-closed) reference.
func (e *Entry) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// Result returns the completion result, or nil if the task hasn't completed.
func (e *Entry) Result() *task.Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

// SetResult records the completion result.
func (e *Entry) SetResult(r *task.Result) {
	e.mu.Lock()
	e.result = r
	e.mu.Unlock()
}

// MonitorBranch returns the branch being monitored for CI, or "".
func (e *Entry) MonitorBranch() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.monitorBranch
}

// SetMonitorBranch records the branch to monitor for CI.
func (e *Entry) SetMonitorBranch(b string) {
	e.mu.Lock()
	e.monitorBranch = b
	e.mu.Unlock()
}

// CloseDone closes the done channel if it is still open. Failure paths during
// task creation call this directly; the normal cleanup path goes through
// Cleanup which handles it idempotently.
//
// Prefer Finish when a result is being recorded at the same time: it sets the
// result and closes done under a single lock acquisition, so a goroutine that
// observes Done() closed is guaranteed to also observe Result().
func (e *Entry) CloseDone() {
	e.mu.Lock()
	if !e.doneClosed {
		close(e.done)
		e.doneClosed = true
	}
	e.mu.Unlock()
}

// Finish records the completion result and closes the done channel in one
// step: the result is published before done closes, so any goroutine that
// observes Done() closed is guaranteed to also observe Result(). This makes the
// set-result-then-close ordering correct by construction rather than relying on
// callers sequencing SetResult and CloseDone. If another terminal path already
// closed done, Finish only refreshes the result.
func (e *Entry) Finish(r *task.Result) {
	e.mu.Lock()
	e.result = r
	if !e.doneClosed {
		close(e.done)
		e.doneClosed = true
	}
	e.mu.Unlock()
}

// Cleanup runs fn exactly once per incarnation. Used to guard executor.Cleanup
// against racing Stop/Purge calls.
func (e *Entry) Cleanup(fn func()) {
	e.mu.Lock()
	once := e.cleanupOnce
	e.mu.Unlock()
	once.Do(fn)
}

// Reset prepares the entry for Revive: a fresh done channel, cleared result,
// and a new cleanupOnce. Only safe when the task is in a terminal state and
// no goroutine is concurrently touching the entry's mutable fields.
func (e *Entry) Reset() {
	e.mu.Lock()
	e.done = make(chan struct{})
	e.doneClosed = false
	e.result = nil
	e.cleanupOnce = new(sync.Once)
	e.mu.Unlock()
}

// LoadMessagesOnce runs fn exactly once to lazily load on-disk messages for a
// purged task. fn must perform the actual load (and any side effect such as
// RestoreMessages); the once is owned by the Entry.
func (e *Entry) LoadMessagesOnce(fn func()) {
	if e.loadedTask == nil {
		return
	}
	e.loadedTaskOnce.Do(fn)
}
