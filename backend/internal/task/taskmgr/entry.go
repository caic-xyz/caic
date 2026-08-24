// Entry wraps a task and its result for the Manager registry.

package taskmgr

import (
	"sync"
	"sync/atomic"

	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// entryTerminal is an immutable snapshot of an Entry's terminal state for one
// incarnation. Snapshots are published atomically before the done channel is
// closed, so any goroutine that observes done closed is guaranteed to observe
// a published snapshot (and therefore result) via a later load.
type entryTerminal struct {
	done     chan struct{}    // Closed exactly when terminal first becomes true.
	result   *taskslog.Result // Completion result; non-nil whenever terminal.
	terminal bool             // True once the entry has reached a terminal state.
}

// Entry is a single registered task plus its mutable lifecycle state.
//
// Concurrency: task and lifecycle are immutable after registration. LogPath
// manages its own synchronization. The terminal state is an immutable snapshot
// published through term; see entryTerminal. loadedTask, monitorBranch, and
// cleanupOnce are guarded by mu and must only be accessed through methods.
type Entry struct {
	// Immutable.
	Lifecycle *Lifecycle

	// LogPath owns the current physical log path for this entry lifecycle.
	LogPath taskslog.Path

	task *task.Task

	// Guards lazy message loading for a persisted task.
	loadedTaskOnce sync.Once

	// Terminal state of the current incarnation, published as an immutable
	// snapshot; see entryTerminal.
	term atomic.Pointer[entryTerminal]

	mu            sync.Mutex
	loadedTask    *taskslog.LoadedTask
	monitorBranch string
	cleanupOnce   sync.Once
}

// Task returns the underlying task. The returned *task.Task is itself
// concurrency-safe via its own internal locking.
func (e *Entry) Task() *task.Task { return e.task }

// LoadedTask returns the on-disk log handle, or nil before a task log has been
// validated for disk replay.
func (e *Entry) LoadedTask() *taskslog.LoadedTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadedTask
}

// SetLoadedTask records a freshly validated on-disk log handle for disk replay.
func (e *Entry) SetLoadedTask(lt *taskslog.LoadedTask) {
	e.mu.Lock()
	e.loadedTask = lt
	if lt != nil {
		e.LogPath.Set(lt.LogPath())
	}
	e.mu.Unlock()
}

// Done returns the channel that closes when the task reaches a terminal
// state. After Reset (Revive), this returns a fresh channel; goroutines that
// captured the previous channel will see a stale (already-closed) reference.
func (e *Entry) Done() <-chan struct{} {
	return e.term.Load().done
}

// Result returns the completion result of the current incarnation, or nil if
// the task hasn't completed.
func (e *Entry) Result() *taskslog.Result {
	return e.term.Load().result
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

// Finish records the completion result and closes the done channel in one
// step: the snapshot is stored before done closes, so any goroutine that
// observes Done() closed is guaranteed to also observe Result(). Concurrent
// callers race and the last store wins. If the task is already terminal,
// Finish only refreshes the result.
func (e *Entry) Finish(r *taskslog.Result) {
	for {
		cur := e.term.Load()
		next := &entryTerminal{done: cur.done, result: r, terminal: true}
		if e.term.CompareAndSwap(cur, next) {
			if !cur.terminal {
				close(cur.done)
			}
			return
		}
	}
}

// Cleanup runs fn exactly once per incarnation. Used to guard checkout.Cleanup
// against racing Stop/Purge calls.
func (e *Entry) Cleanup(fn func()) {
	e.mu.Lock()
	once := &e.cleanupOnce
	e.mu.Unlock()
	once.Do(fn)
}

// Reset prepares the entry for Revive: a fresh terminal snapshot with an open
// done channel and no result, plus a new cleanupOnce. Only safe when the task
// is in a terminal state and no goroutine is concurrently touching the entry's
// mutable fields.
func (e *Entry) Reset() {
	e.term.Store(&entryTerminal{done: make(chan struct{})})
	e.mu.Lock()
	e.cleanupOnce = sync.Once{}
	e.mu.Unlock()
}

// LoadMessagesOnce runs fn exactly once to lazily load on-disk messages for a
// purged task. fn must perform the actual load (and any side effect such as
// SeedTimeline); the once is owned by the Entry.
func (e *Entry) LoadMessagesOnce(fn func()) {
	if e.LoadedTask() == nil {
		return
	}
	e.loadedTaskOnce.Do(fn)
}
