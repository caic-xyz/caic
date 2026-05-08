# Android E2E CI Failure Analysis

## GenScreenshotsTest: first SSE event consistently lost on CI

When the emulator networking works (one successful CI run), the test still fails:

```
ComposeTimeoutException: Condition still not satisfied after 60000 ms
at GenScreenshotsTest.kt:109
```

**Evidence from the semantics tree dump at timeout:**

3 of 4 task cards are rendered, with tags `task-20780IFGM000`, `task-20780I16C000`,
`task-20780HCP8000`.  The first-created task (`20780GEBE000`) is missing from the
very first dump at t+3.6s and every subsequent frame for the full 60s.
`CollectionInfo(rowCount=6)` matches the 6 items present (repo-chips, prompt-input,
clone label, 3 task cards) — the 4th task data simply never arrives.

The task is confirmed to exist and be in its target state via `waitForTaskState`
(API polling).  All 4 tasks are created successfully.  The other 3 arrive via SSE
and render fine.

**Ruled out:**

- Channel buffer overflow: at most ~25 events arrive during the window, buffer
  has 64 slots.
- Server-side ordering: the SSE loop emits snapshot → repos → upserts atomically
  per iteration.
- Connection drops: no IOException or SocketException in the 60s window; the
  `connection-dot` tag is present.
- Compose off-screen culling: the task is absent from the data, not just the
  viewport.

**Working hypothesis:** the first SSE event arrives in the same TCP chunk as the
HTTP response headers.  On the slow CI emulator (first frame 749ms), the initial
`snapshot` event triggers a StateFlow emission → `combine` → recomposition
cascade that blocks the main thread.  The immediately-following upsert for the
first task is queued in some intermediate OkHttp/Flow buffer but gets dropped
before reaching `channelFlow.trySend`.

## Fix applied: patch reconstruction

When a PATCH event arrives for a task not in the local map (because its UPSERT
was dropped), reconstruct a minimal Task from the patch fields instead of
silently ignoring it.  This makes the task recoverable: a subsequent state-change
patch will create the task entry even if the initial upsert was lost.

**File**: `android/app/src/main/java/com/fghbuild/caic/data/TaskRepository.kt`

## Diagnostic logging added

- `Log.d` for every snapshot, upsert, and state-change event
- `Log.w` when a patch arrives for an unknown task (reconstruction)
- `Log.w` when `trySend` returns `false` (channel full)
- `Log.d` when the SSE connection is lost and reconnecting

## CI emulator networking flakiness (ECONNREFUSED)

The `adb reverse` tunnel between the emulator and host is unreliable on GitHub
Actions runners.  Three consecutive CI runs after the initial successful one all
failed with `java.net.ConnectException: ECONNREFUSED` — the app couldn't reach
the fake backend at all, so all 5 e2e tests failed, not just GenScreenshotsTest.

An `adb reverse` retry loop with backoff was added to `scripts/android_e2e.py`
to mitigate this.

## Earlier fixes (pre-existing in the codebase)

### callbackFlow → channelFlow (BUFFERED=64)

`TaskRepository.taskListEvents()` used to use `callbackFlow` (RENDEZVOUS
channel).  Switched to `channelFlow` (BUFFERED=64).

### CancellationException doesn't clear connected flag

`reconnectingFlow()` now clears `flag.value` before re-throwing
`CancellationException`.

### Accelerated OkHttp retries

Exponential backoff: 200ms initial, 1.5× multiplier, 5s cap.

### E2eTestBase: stateIn race in e2eConfigureRule

Added a wait loop to ensure `SettingsRepository.settings` reflects the final URL
before launching the Activity.

### fake_enabled.go: stale task logs across runs

`serveFake` now uses a unique temp dir per run instead of a hardcoded path.

### GenScreenshotsTest: JUnit assertions + semantics tree dump

Replaced Kotlin `assert()` with `org.junit.Assert` (always enabled).  Added
`onRoot().printToLog("E2E")` diagnostics on failure.

## Local testing

All 24 tests pass on Pixel 8 Pro (API 33, ARM) and local emulator (API 36, x86_64).
The bug only reproduces on CI (API 35, x86_64 emulator) — slower frame times
expose the event-drop race.

## Plan

1. **Fix CI networking flakiness** — try `10.0.2.2` (emulator host alias, no
   `adb reverse` needed) or `adb forward` instead of `adb reverse`. Unblocks
   all further observation.
2. **Read diagnostic logs** — the `Log.d` calls already in `TaskRepository.kt`
   will show whether id1's upsert fires, whether `trySend` drops it, and
   whether the patch reconstruction path triggers. This answers the question.
3. **Server-side fallback** — if reconstruction works, have the fake backend
   always emit a second upsert (not just a patch) for the first state change
   after task creation, giving the client two shots.
4. **OkHttp byte-level tracing** — add a `NetworkInterceptor` to log raw SSE
   bytes if the event is confirmed lost before `channelFlow`.
5. **Workflow fix** — someone with a PAT needs to add `backend/**` to the
   Android CI path triggers.
