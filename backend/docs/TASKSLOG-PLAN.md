# taskslog Extraction and Async Settled Load

Working plan — phases are removed and the rest renumbered as each lands.

## Goal

All task-log I/O — reading, writing, compression, the header cache, and the
decoder pool — lives in one cohesive `taskslog` package that knows nothing
about the live `Task`. The startup load recipe (retention window, scan order,
per-repo cap) lives in `taskslog.Store` (`internal/taskslog/store.go`), called
from `app.go`. The settled (compressed) history now needs to load in a
background pass so the server accepts requests once the live + unsettled logs
are in, with the history filling in over the existing SSE stream.

Plan-level invariant, held by every phase: **`taskslog` is a leaf** — it imports
only existing leaves (`agent`, `runtime`, `harness`, …), never `task` or
`taskmgr`. `task` and `taskmgr` may import `taskslog`. This is what keeps the
graph acyclic.

## Phase 1 — `async-settled`: Settled history loads in the background

- **Scope:** `app.go` (startup + background pass), task-list read path, SSE
  event emission, `v1.TaskListEvent`, and the frontend `ConnectionDot` /
  `AppState` loading signal.
- **Preserve:** live + unsettled logs are fully loaded and the server is up
  before the settled pass; per-repo cap and retention unchanged; reuse the
  existing SSE stream, add no new channel.
- **Change:** the settled (compressed) load + `LoadPurgedTasks` run in a
  background goroutine after the server starts. As entries land, the task list
  grows via the existing upsert events. `taskMgr` exposes `SettledLoading() bool`
  and `SettledError() string`; the list stream carries both in the initial
  snapshot and emits a `kind:"status"` event as the pass transitions
  (in-progress → completed | failed). The frontend `ConnectionDot` becomes
  four-state, worst-wins: red disconnected > orange pass-failed (persistent)
  > yellow pass-in-progress (transient) > green pass-completed; a tooltip on
  orange shows `settledError`. An empty list while `settledLoading` renders
  "loading", not "no tasks". On a pass error there is no retry — the pass runs
  once, the valid partial subset is kept, the error is logged + metric'd, and
  `settledError` is set (orange); startup is not failed, and a restart is a
  clean rebuild because the registry is in-memory, so a pass killed mid-load
  loses nothing.
- **Verify:** server accepts the first request while the settled pass is still
  running; task list grows as settled entries land; the `ConnectionDot` is
  yellow during the pass, green after a clean pass, and orange (with the
  `settledError` tooltip) after a failed pass; an empty list while loading
  renders "loading", not "no tasks"; a full cold start still converges to the
  same final task set as today's synchronous load.
