# Android E2E CI Failure Analysis

## Root cause: LazyColumn viewport culling

`GenScreenshotsTest.generateDocumentationScreenshots` creates 4 tasks and waits
for the first one (`task-$id1`) to appear in the Compose UI.  The check times
out after 60s:

```
ComposeTimeoutException: Condition still not satisfied after 60000 ms
at GenScreenshotsTest.kt:109
```

**The data is correct.**  Diagnostic logs added to `TaskRepository.kt` and
`TaskListViewModel.kt` confirm that all 4 SSE upsert events are received, all 4
tasks are stored in `_tasks`, and the `combine` lambda sees all 4 tasks every
time it runs.

**The bug is viewport culling.**  Tasks are sorted by `taskIdDesc` (newest
first).  The first-created task has the smallest ksid and ends up **last** in
the LazyColumn.  On the CI emulator the viewport only fits the creation form +
3 task cards (~397px of 563px available minus the form height).  The 4th card
is below the viewport, LazyColumn never composes it, and its node is absent
from the Compose semantics tree.  `onAllNodesWithTag("task-$id1")` returns
empty forever.

**Why it passes locally.**  Local emulators and physical devices have larger
screens (or higher pixel density), so all 4 task cards fit in the viewport.
The CI emulator uses a 320×640 skin at 160dpi, yielding only ~397px of
scrollable space for both the creation form and task cards.

**Why it hits only the first task.**  The test checks `task-$id1` specifically.
id1 is the smallest ksid (created first), so it sorts last and falls out of the
viewport.  The other three tasks (ids 2, 3, 4) are visible but the test
doesn't check for them.

## Fix

Either scroll the LazyColumn to reveal id1 before checking, or create only 3
tasks instead of 4, or check for the presence of *any* task card rather than
id1 specifically.

**File**: `android/app/src/androidTest/java/com/fghbuild/caic/e2e/GenScreenshotsTest.kt`

## CI networking: use 10.0.2.2 instead of adb reverse

The `adb reverse` tunnel was flaky on GitHub Actions runners.  Switched to
`10.0.2.2` (the emulator's built-in host loopback alias), which needs no
tunnel setup.  Real devices still use `localhost` + `adb reverse`.

## Diagnostic logging (for future debugging)

- `Log.d` in `TaskRepository.taskListEvents()` for every snapshot, upsert, and
  patch event
- `Log.d` when `_tasks` StateFlow is updated, with task count and IDs
- `Log.d` in `TaskListViewModel.combine` lambda showing task IDs
- `Log.w` when `trySend` returns `false` (channel full) or a patch arrives for
  an unknown task (triggers reconstruction)

## Earlier fixes (pre-existing)

- `callbackFlow` → `channelFlow` (BUFFERED=64)
- `CancellationException` now clears the connected flag
- Accelerated OkHttp retries: 200ms initial, 5s cap
- `E2eTestBase.e2eConfigureRule` waits for `stateIn` to reflect the final URL
- `fake_enabled.go` uses a unique temp dir per run
- `GenScreenshotsTest` uses `org.junit.Assert` + `printToLog("E2E")` diagnostics

## Plan

1. Scroll the LazyColumn or create only 3 tasks in GenScreenshotsTest.
2. Remove the now-unnecessary patch reconstruction code (the events were never
   dropped — the task was just off-screen).
3. Someone with a PAT needs to add `backend/**` to the Android CI path triggers.
