# Backend Refactor Plan

This plan tracks the remaining backend design work needed to support execution
environments beyond `md` containers, including virtual machines. Treat `md`
containers as one runtime adapter, not as the backend domain model.

## 1. Finish Runtime Boundary Naming Cleanup

Goal: remove remaining container-specific naming and Docker-specific wording
from runtime-facing task orchestration APIs.

Current state: `internal/runtime` owns lifecycle, monitoring, inventory,
metadata, and privilege interfaces. `internal/task` and `internal/tasks` use
runtime-owned types, and the `md` adapter lives under
`internal/runtime/mdruntime`.

Plan:

1. Rename task/server fields and methods that still expose the runtime instance
   as a container, including `Task.Container`, `ContainerName`,
   `SetContainerInfo`, `Runner.Container`, and related test doubles.
2. Keep user-facing compatibility where needed by converting at API boundaries
   instead of preserving container naming in core task orchestration.
3. Rename comments, log messages, test names, and helper names that still say
   container when the concept is a runtime instance.
4. Keep behavior unchanged for the current `md` adapter.

Validation:

```bash
make lint-go
go test ./backend/internal/runtime ./backend/internal/task ./backend/internal/tasks ./backend/internal/server
./scripts/update_backend_architecture.py
make lint-docs
```

## 2. Split `server.Config` Validation By Concern

Goal: keep the nested `server.Config` structure, but move validation logic to
the same concern boundaries as the data.

Current state: `server.Config` is split into `DirsConfig`, `RuntimeConfig`,
`AgentConfig`, `LLMConfig`, `GitHubConfig`, `GitLabConfig`, `AuthConfig`,
`VoiceConfig`, `DebugConfig`, and `IPGeoConfig`. Validation is still centralized
in one large `Config.Validate()` method.

Plan:

1. Add validation methods on the nested config structs where rules are local to
   one concern.
2. Keep cross-concern rules in `Config.Validate()` only when they genuinely need
   multiple config sections, such as OAuth requiring `Auth.ExternalURL`.
3. Keep external TOML compatibility unless intentionally changing config format.
4. Update config tests first, then refactor until they pass.
5. Avoid mixing this with startup extraction; keep it a structural change.

Validation:

```bash
make lint-go
go test ./backend/cmd/caic ./backend/internal/server
```

## 3. Extract Startup Assembly From `internal/server`

Goal: keep `internal/server` focused on HTTP behavior and move composition and
startup wiring elsewhere.

Current state: `backend/internal/app` exists but only wraps `server.New`.
`server.New` still performs startup assembly, including runtime adapter
construction, repo discovery, settings/auth setup, forge manager creation, task
manager construction, adoption, and maintenance startup.

Plan:

1. Move orchestration currently in `internal/server/startup.go` into
   `backend/internal/app`.
2. Keep `server.New` narrower: accept already-constructed dependencies or a
   smaller dependency config.
3. Move repo discovery, settings load, auth store setup, forge manager creation,
   runtime adapter selection, task manager construction, and maintenance startup
   into the app package.
4. Keep HTTP handlers and `Server` methods in `internal/server`.
5. Update `cmd/caic` and fake/smoke setup to call the app constructor.
6. Run the architecture generator and verify `internal/server` dependency count
   drops.

Validation:

```bash
make lint-go
go test ./backend/cmd/caic ./backend/internal/app ./backend/internal/server ./backend/internal/tasks
./scripts/update_backend_architecture.py
make lint-docs
```

## 4. Split `server.Server` By Concern

Goal: keep `Server` as the HTTP router and lifecycle owner, not the concrete
implementation of every backend-facing role.

Current state: `Server` still owns auth, repo registry access, task HTTP
handlers, webhook handlers, bot client methods, CI service adapter methods,
usage streaming, voice handlers, maintenance helpers, forge management, and
runtime operation helpers. Extracting startup assembly will reduce construction
coupling, but it will not by itself reduce the number of responsibilities on
`Server`.

Plan:

1. Group handler dependencies by concern and introduce small structs such as
   task handlers, repo/config handlers, auth handlers, webhook handlers, usage
   handlers, and voice handlers.
2. Move CI service adapter methods out of `Server` into a dedicated adapter that
   takes only the repo registry, task manager, forge manager, preferences, and
   warning sink it needs.
3. Move bot client methods out of `Server` into a dedicated `bot.Client`
   implementation with explicit dependencies.
4. Keep shared state ownership clear: stores and managers may remain shared, but
   each concern should receive them explicitly instead of reaching through the
   whole `Server`.
5. Keep route registration centralized until there is a clear benefit to
   splitting it; avoid hiding routes behind package-level side effects.
6. Update tests to target the extracted concern structs where that gives smaller
   fixtures, while keeping end-to-end route coverage for public HTTP behavior.

Validation:

```bash
make lint-go
go test ./backend/internal/server ./backend/internal/ci ./backend/internal/bot
./scripts/update_backend_architecture.py
make lint-docs
```

## Runtime Design Notes

- Do not require VMs to emulate Docker labels. Containers can implement
  metadata through labels; VMs can use disk metadata, cloud tags, or a local
  registry.
- Keep lifecycle, monitoring, inventory, and privilege lookup separate. VM
  stats, adoption, and lifecycle events may not map cleanly to Docker concepts.
- Keep `md` out of `internal/task` and `internal/tasks`. Those packages should
  depend on caic-owned runtime interfaces and types.
- Treat `internal/runtime/mdruntime` as the current `md` implementation, not as
  the general runtime abstraction.

## Suggested Order

1. Finish runtime boundary naming cleanup.
2. Split `server.Config` validation by concern.
3. Extract startup assembly from `internal/server`.
4. Split `server.Server` by concern.
