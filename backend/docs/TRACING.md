# Execution Tracing

caic instruments its hot paths with Go's [`runtime/trace`](https://pkg.go.dev/runtime/trace)
to provide visibility into server startup, task lifecycle, container operations,
and session management.

## Enabling

Set `trace` in the `[debug]` section of `config.toml` to a file path:

```toml
[debug]
trace = "/tmp/caic-trace.out"
```

The server writes a binary trace on startup that captures all activity until
shutdown. On SIGINT the trace is finalized via `defer trace.Stop()`.

CPU and heap profiles use the companion keys `cpuprofile` and `memprofile`:

```toml
[debug]
cpuprofile = "/tmp/caic-cpu.prof"
memprofile = "/tmp/caic-mem.prof"
trace      = "/tmp/caic-trace.out"
```

## Viewing

### Browser viewer (recommended)

```bash
go tool trace -http=:8080 /tmp/caic-trace.out
```

Opens a local web UI with goroutine analysis, GC timeline, network/sync
blocking profiles, and — critically — the **User tasks** and **User regions**
panels where caic's named trace annotations appear.

### JSON export

```bash
curl -s 'http://localhost:8080/jsontrace?trace=' > trace.json
```

Produces a Chrome Trace Event JSON file. **Note:** `trace.NewTask` and
`trace.StartRegion` boundaries are not rendered as individual events in this
format (Go 1.26 limitation). Only `trace.Log` / `trace.Logf` calls appear as
`cat='user event'` entries. Use the browser viewer to see the full task tree.

## Instrumented paths

### Startup (`backend/internal/server/startup.go`)

| Scope | Type | Name |
|---|---|---|
| Whole startup | `trace.NewTask` | `server.startup` |
| Phase 1 — repo discovery | `trace.StartRegion` | `discover-repos` |
| Phase 1 — log loading | `trace.StartRegion` | `load-logs` |
| Phase 1 — container listing | `trace.StartRegion` | `list-containers` |
| Phase 2 — per-repo init | `trace.StartRegion` | `repo-executor-init` |
| Phase 3 — load purged tasks | `trace.StartRegion` | `load-purged-tasks` |
| Phase 4 — adopt containers | `trace.StartRegion` | `adopt-containers` |
| Per-container adoption | `trace.NewTask` | `adopt-container` |
| Container label check | `trace.StartRegion` | `caic-label` |
| Relay liveness probe | `trace.StartRegion` | `relay-status` |
| Relay output read | `trace.StartRegion` | `relay-read` |
| Log message restore | `trace.StartRegion` | `load-messages` |
| Adoption metadata | `trace.Logf` | container/repo/branch per container |

### Task lifecycle (`backend/internal/server/tasks.go`)

| Trigger | Type | Name |
|---|---|---|
| `POST /tasks` goroutine | `trace.NewTask` | `task.create:{id}` |
| `POST /tasks/{id}/stop` goroutine | `trace.NewTask` | `task.stop:{id}` |
| `POST /tasks/{id}/revive` goroutine | `trace.NewTask` | `task.revive:{id}` |
| `POST /tasks/{id}/fork` goroutine | `trace.NewTask` | `task.fork:{src}->{dst}` |
| `watchSession` goroutine | `trace.NewTask` | `session.watch:{id}` |

### Repo executor operations (`backend/internal/task/repoexecutor.go`)

| Method | Type | Name |
|---|---|---|
| `Start` | `trace.NewTask` | `task.start:{id}` |
| `Start` — container setup | `trace.StartRegion` | `setup` |
| `Start` — SSH + git push | `trace.StartRegion` | `phase-a-launch` |
| `Start` — agent launch | `trace.StartRegion` | `phase-b-connect` |
| `Start` — agent process | `trace.StartRegion` | `agent-session` |
| `Cleanup` | `trace.NewTask` | `task.cleanup:{id}` |
| `StopTask` | `trace.NewTask` | `task.stop:{id}` |
| `ReviveTask` | `trace.NewTask` | `task.revive:{id}` |
| `Reconnect` | `trace.NewTask` | `task.reconnect:{id}` |
| `RestartSession` | `trace.NewTask` | `task.restart:{id}` |
| `ClearContextSession` | `trace.NewTask` | `task.clear-context:{id}` |
| `StartSession` | `trace.NewTask` | `task.start-session:{id}` |
| `ForkTask` | `trace.NewTask` | `task.fork:{src}->{dst}` |
| `SyncToOrigin` — fetch | `trace.StartRegion` | `sync-fetch` |
| `SyncToDefault` — fetch | `trace.StartRegion` | `sync-default-fetch` |

### Container operations (`backend/internal/runtime/mdruntime/backend.go`)

| Method | Type | Name |
|---|---|---|
| `Launch` | `trace.StartRegion` | `container.launch` |
| `Connect` | `trace.StartRegion` | `container.connect` |
| `Diff` | `trace.StartRegion` | `container.diff` |
| `Fetch` | `trace.StartRegion` | `container.fetch` |
| `Stop` | `trace.StartRegion` | `container.stop` |
| `Purge` | `trace.StartRegion` | `container.purge` |
| `Revive` | `trace.StartRegion` | `container.revive` |
| `Fork` | `trace.StartRegion` | `container.fork` |

### Background loops (`backend/internal/server/server.go`)

| Loop | Type | Name |
|---|---|---|
| `pushStats` (every 5 s) | `trace.StartRegion` | `poll-stats` |

### Canary (`backend/cmd/caic/main.go`)

| When | Type | Name |
|---|---|---|
| Right after `trace.Start` | `trace.Log` | `[trace] started` |

Used to confirm that user annotations are being captured by the trace writer.

## Adding new instrumentation

### Choosing scope

- **`trace.NewTask`** — for long-lived operations that span goroutines
  (e.g. task creation, session monitoring). Tasks appear in the browser
  viewer's "User tasks" panel and group goroutine activity.

- **`trace.StartRegion`** — for sub-phases within a task (e.g. container
  launch, agent session start). Regions appear nested inside their parent
  task in the "User regions" panel.

- **`trace.Log` / `trace.Logf`** — for discrete data points that should
  survive JSON export (e.g. container IDs, branch names, error conditions).
  These are the only annotations visible in `jsontrace` output.

### Patterns

```go
// Long-running goroutine:
go func() {
    ctx, task := trace.NewTask(s.ctx, "task.create:"+id.String())
    defer task.End()
    // ... work ...
}()

// Sub-phase within an existing task:
region := trace.StartRegion(ctx, "phase-a-launch")
defer region.End()

// Data annotation (visible in jsontrace):
trace.Logf(ctx, "container", "%s repo=%s branch=%s", c.Name, relPath, branch)
```

### Context threading

Trace tasks and regions inherit from the Go context. Always pass the
context carrying the trace task down to callees so their `StartRegion`
calls nest correctly:

```go
func (r *RepoExecutor) Start(ctx context.Context, t *Task) (*SessionHandle, error) {
    ctx, task := trace.NewTask(ctx, "task.start:"+t.ID.String())
    defer task.End()
    // Pass ctx to setup() so its regions nest under task.start.
    sr, err := r.setup(ctx, t, labels)
    ...
}
```

### What not to trace

- Very short operations (< 100 µs) — trace overhead dominates.
- Tight loops — each region begin/end writes to the trace buffer.
- Mutex-locked sections where the region's parent context holds the lock.

## Known limitations

- **Go 1.26 `jsontrace` does not export user tasks/regions.** The Chrome
  Trace Event JSON omits `trace.NewTask` and `trace.StartRegion` boundaries.
  Only `trace.Log`/`trace.Logf` appear. Use the browser viewer for the full
  task tree.

- **Trace file grows with runtime.** Long server sessions produce large
  trace files. The trace is only finalized on shutdown; consider snapshotting
  with a short-lived test run.

- **Task names carry IDs.** Names like `task.start:01jq...` are long.
  The trace viewer truncates them. Use the ID prefix (first 6 chars) for
  readability if needed.
