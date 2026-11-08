// Entry owns a registered task, journal commands, pending prompts, and lifecycle results.

package taskmgr

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/maruel/ksid"
)

// entryTerminal is an immutable snapshot of an Entry's terminal state for one
// incarnation. Snapshots are published atomically before the done channel is
// closed, so any goroutine that observes done closed is guaranteed to observe
// a published snapshot (and therefore result) via a later load.
type entryTerminal struct {
	done     chan struct{} // Closed exactly when terminal first becomes true.
	result   *task.Result  // Completion result; non-nil whenever terminal.
	terminal bool          // True once the entry has reached a terminal state.
}

// Entry is a single registered task plus its mutable lifecycle state.
//
// Concurrency: task, lifecycle, and historyInMemory are immutable after
// registration. LogPath manages its own synchronization. The terminal state is
// an immutable snapshot published through term; see entryTerminal. pendingInput,
// loadedTask, monitorBranch, and cleanupOnce are guarded by mu and must only be
// accessed through methods. historyMemo is guarded by historyGate.
type Entry struct {
	// Immutable.
	Lifecycle *Lifecycle

	// LogPath owns the current physical log path for this entry lifecycle.
	LogPath taskslog.Path

	task *taskrun.Task

	// New and adopted tasks keep their active timeline in Task. Restored cold
	// tasks leave it empty and read rare history requests from loadedTask.
	historyInMemory bool

	// Terminal state of the current incarnation, published as an immutable
	// snapshot; see entryTerminal.
	term atomic.Pointer[entryTerminal]

	// historyGate holds one cold history query at a time for this entry and
	// guards historyMemo. A cold query decodes the whole task log, so a concurrent
	// query waits for one decode instead of repeating it.
	historyGate chan struct{}
	historyMemo *historyMemo

	mu            sync.Mutex
	pendingInput  *PendingInput
	loadedTask    *taskhistory.LoadedTask
	monitorBranch string
	cleanupOnce   sync.Once
}

// Task returns the underlying task. The returned *taskrun.Task is itself
// concurrency-safe via its own internal locking.
func (e *Entry) Task() *taskrun.Task { return e.task }

// RecordHost commits a backend control through the entry's journal owner.
func (e *Entry) RecordHost(ctx context.Context, m agent.Message) error {
	return e.Lifecycle.agentRuntime.RecordHost(ctx, e.task, m)
}

// RecordRepositorySummary commits a fresh repository observation before publishing it.
func (e *Entry) RecordRepositorySummary(ctx context.Context, read *repo.GitSnapshot) (bool, error) {
	return e.Lifecycle.agentRuntime.RecordRepositorySummary(ctx, e.task, read)
}

// PendingInput returns this task's immutable pending prompt, or nil.
func (e *Entry) PendingInput() *PendingInput {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pendingInput
}

// PendingInputImage returns an original attachment only if the pending identity matches.
func (e *Entry) PendingInputImage(id ksid.ID, index int) (v3.ImageData, bool) {
	q := e.PendingInput()
	if q == nil || q.ID != id || index < 0 || index >= len(q.Prompt.Images) {
		return v3.ImageData{}, false
	}
	return q.Prompt.Images[index], true
}

// LoadedTask returns the on-disk log handle, or nil before a task log has been
// validated for stored-history reads.
func (e *Entry) LoadedTask() *taskhistory.LoadedTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadedTask
}

// SetLoadedTask records a freshly validated on-disk log handle for stored-history reads.
func (e *Entry) SetLoadedTask(lt *taskhistory.LoadedTask) {
	e.mu.Lock()
	e.loadedTask = lt
	if lt != nil {
		e.LogPath.Set(lt.LogPath())
	}
	e.mu.Unlock()
}

// Done returns the channel that closes when the task reaches a terminal
// state. Revival or clearing a result after reconnect returns a fresh channel;
// goroutines that captured the previous channel will see a stale
// (already-closed) reference.
func (e *Entry) Done() <-chan struct{} {
	return e.term.Load().done
}

// Result returns the completion result of the current incarnation, or nil if
// the task hasn't completed.
func (e *Entry) Result() *task.Result {
	return e.term.Load().result
}

// entryInvalidationState combines task metadata with the result owned by Entry.
// A result replacement retries the task sample, so classification never combines
// task metadata with a result superseded while that metadata was captured.
type entryInvalidationState struct {
	taskrun.InvalidationState

	HasError bool
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
// callers race and the last store wins, except that a purged result is final:
// the purge removes the runtime instance, and the stop it superseded can
// publish its older snapshot afterwards. If the task is already terminal,
// Finish only refreshes the result.
func (e *Entry) Finish(r *task.Result) {
	for {
		cur := e.term.Load()
		if cur.result != nil && cur.result.State == task.StatePurged && r.State != task.StatePurged {
			return
		}
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

func (e *Entry) invalidationState() entryInvalidationState {
	for {
		terminal := e.term.Load()
		state := e.task.InvalidationState()
		if e.term.Load() == terminal {
			return entryInvalidationState{InvalidationState: state, HasError: terminal.result != nil && terminal.result.Err != nil}
		}
	}
}

// clearResult reopens completion only if the result captured before reconnect
// is still current. A newer failure or shutdown result must remain visible.
func (e *Entry) clearResult(expected *task.Result) {
	if expected == nil {
		return
	}
	for {
		cur := e.term.Load()
		if cur.result != expected {
			return
		}
		if e.term.CompareAndSwap(cur, &entryTerminal{done: make(chan struct{})}) {
			return
		}
	}
}

// setPendingInput publishes an immutable value under the entry lock.
func (e *Entry) setPendingInput(q *PendingInput) {
	e.mu.Lock()
	e.pendingInput = q
	e.mu.Unlock()
}

// acquireHistory claims the entry's cold history gate, waiting for a running
// query to finish unless ctx ends first.
func (e *Entry) acquireHistory(ctx context.Context) error {
	select {
	case e.historyGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseHistory returns the entry's cold history gate.
func (e *Entry) releaseHistory() { <-e.historyGate }

// coldHistoryLocked returns the retained answers for the entry's current log,
// discarding them when the log changed. The caller holds historyGate.
func (e *Entry) coldHistoryLocked(identity historyIdentity) *historyMemo {
	if e.historyMemo == nil || e.historyMemo.identity != identity {
		e.historyMemo = &historyMemo{identity: identity}
	}
	return e.historyMemo
}
