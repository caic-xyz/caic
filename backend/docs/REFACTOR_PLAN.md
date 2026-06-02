# Backend Refactor Plan

This plan tracks the remaining backend design work needed to support execution
environments beyond `md` containers, including virtual machines. Treat `md`
containers as one runtime adapter, not as the backend domain model.

## Target Shape

- `internal/runtime` owns runtime lifecycle, monitoring, inventory, metadata,
  process, and privilege contracts.
- `internal/runtime/mdruntime` is the `md` adapter. Docker, Podman, and labels
  are implementation details there unless an API explicitly exposes them.
- `internal/task` and `internal/tasks` orchestrate caic tasks against
  `runtime.InstanceID` and runtime interfaces, not containers.
- `internal/app` assembles dependencies and starts background services.
- `internal/server` owns HTTP routing, request/response conversion, middleware,
  and API-facing behavior.
- `internal/agent` still assumes md-style SSH targets today. Do not hide that
  with naming cleanup; introduce a runtime connection abstraction when a non-md
  runtime needs a different agent transport.

## Working Rules

- Keep behavior unchanged unless a phase explicitly says otherwise.
- Prefer the best domain model and API shape over backward compatibility.
  Breaking API v1 is acceptable when the current contract exposes the wrong
  abstraction.
- Preserve existing task data only when the migration is simple and does not
  compromise the new design. Do not keep compatibility shims in core code.
- Keep each phase independently shippable. Prefer one structural seam at a time
  over a broad rename plus behavior changes.
- Update tests before changing behavior; update comments and first-line purpose
  comments when a source file's purpose changes.
- Run the validation commands listed for the phase that changed.

## 1. Split `server.Config` Validation By Concern

Goal: keep the nested `server.Config` structure, but move validation logic to
the same concern boundaries as the data.

Current state: `server.Config` is split into `DirsConfig`, `RuntimeConfig`,
`AgentConfig`, `LLMConfig`, `GitHubConfig`, `GitLabConfig`, `AuthConfig`,
`VoiceConfig`, `DebugConfig`, and `IPGeoConfig`. Validation is still centralized
in one large `Config.Validate()` method.

Implementation order:

1. Add focused tests around the current validation matrix before moving code:
   OAuth pairs, OAuth allowed users, GitLab token-vs-OAuth exclusivity,
   `external_url`, `gitlab.url`, and runtime name.
2. Add `Validate()` methods on nested structs for local rules:
   `RuntimeConfig`, `GitHubConfig`, `GitLabConfig`, `AuthConfig`, and
   `IPGeoConfig` if needed.
3. Keep cross-concern rules in `Config.Validate()` only when they require
   multiple sections, such as OAuth requiring `Auth.ExternalURL` and HTTPS.
4. Make normalization explicit. `Config.Validate()` currently strips trailing
   slashes from `Auth.ExternalURL`; keep that behavior, but isolate it so tests
   make the mutation intentional.
5. Keep the external TOML format unchanged only where it still matches the best
   config structure. Rename or reshape config keys when the existing format
   leaks old implementation details.
6. Do not mix this with startup extraction.

Acceptance criteria:

- Local validation rules live beside their config structs.
- `Config.Validate()` reads as orchestration over nested validators plus a short
  set of cross-concern checks.
- Config keys and validation errors describe the new domain model directly.

Validation:

```bash
make lint-go
go test ./backend/cmd/caic ./backend/internal/server
```

## 2. Extract Startup Assembly From `internal/server`

Goal: keep `internal/server` focused on HTTP behavior and move composition and
startup wiring elsewhere.

Current state: `backend/internal/app` exists but only wraps `server.New`.
`server.New` still performs startup assembly, including runtime adapter
construction, repo discovery, settings/auth setup, forge manager creation, task
manager construction, adoption, and maintenance startup.

Implementation order:

1. Introduce a narrower `server.New` that accepts constructed dependencies,
   for example a `server.Dependencies` or `server.Options` value. Keep route
   registration and HTTP middleware construction in `internal/server`.
2. Keep `app.New(ctx, rootDir, cfg)` as the high-level constructor used by
   `cmd/caic`, fake mode, and smoke setup.
3. Move runtime adapter selection, runtime override wiring, agent backend
   registry construction, repo discovery, task-log loading, settings load,
   auth store setup, forge manager creation, task manager construction,
   adoption, and maintenance goroutine startup into `internal/app`.
4. Move helper code only when the helper belongs to assembly. Leave handler
   helpers and API conversion helpers in `internal/server`.
5. Keep repo discovery parallelism, task-log migration, adoption behavior, and
   startup trace regions equivalent unless deliberately changing them in a
   follow-up.
6. Run the architecture generator and check that `internal/server` imports fewer
   backend concerns. `internal/app` should become the package with composition
   imports.

Acceptance criteria:

- `server.New` can be tested with small HTTP dependencies without constructing
  md clients, runtime inventory, auth stores, forge clients, or task managers
  internally.
- `internal/server` no longer imports `internal/runtime/mdruntime` or
  `internal/agent/registry`.
- `internal/app` is the only package responsible for backend assembly.
- Existing startup behavior remains covered by app-level tests and smoke/fake
  setup.

Validation:

```bash
make lint-go
go test ./backend/cmd/caic ./backend/internal/app ./backend/internal/server ./backend/internal/tasks
./scripts/update_backend_architecture.py
make lint-docs
```

## 3. Split `server.Server` By Concern

Goal: keep `Server` as the HTTP router and lifecycle owner, not the concrete
implementation of every backend-facing role.

Current state: `Server` still owns auth, repo registry access, task HTTP
handlers, webhook handlers, bot client methods, CI service adapter methods,
usage streaming, voice handlers, maintenance helpers, forge management, and
runtime operation helpers. Extracting startup assembly will reduce construction
coupling, but it will not by itself reduce the number of responsibilities on
`Server`.

Implementation order:

1. Group route handlers by concern: tasks, repos/config/preferences, auth,
   webhooks, bot actions, usage streams, voice, and runtime process operations.
2. Introduce small concern structs with explicit dependencies. The first pass
   can keep them in `internal/server` to avoid premature package splits.
3. Move CI service adapter methods out of `Server` into a dedicated adapter that
   takes only the repo registry, task manager, forge manager, preferences, and
   warning sink it needs.
4. Move bot client methods out of `Server` into a dedicated `bot.Client`
   implementation with explicit task, repo, forge, and warning dependencies.
5. Keep shared stores and managers as shared objects, but pass them directly to
   each concern instead of reaching through the whole `Server`.
6. Keep route registration centralized until split registration reduces
   concrete complexity. Do not hide route definitions behind package-level side
   effects.
7. Update tests to target extracted concern structs where that gives smaller
   fixtures, while keeping route-level tests for public HTTP behavior.

Acceptance criteria:

- `Server` owns the HTTP server, middleware, router setup, and lifecycle state.
- CI and bot packages no longer need `Server` as their adapter/client type.
- Handler tests can construct the concern under test without unrelated auth,
  voice, runtime, and forge fixtures.

Validation:

```bash
make lint-go
go test ./backend/internal/server ./backend/internal/ci ./backend/internal/bot
./scripts/update_backend_architecture.py
make lint-docs
```

## Deferred Runtime Connection Work

The lifecycle abstraction is not the same as the agent transport abstraction.
Current agent backends call `ssh <container> ...`, deploy relay files through
md-style helpers, and pass `agent.Options.Container`. That is acceptable while
`mdruntime` is the only runtime adapter, but non-md runtimes need a transport
contract before this is runtime-neutral.

Design direction:

- Define a runtime-owned connection target for agent sessions, plan-file reads,
  relay attach, relay log diagnostics, and file deployment.
- Keep agent backends focused on harness command construction and wire parsing.
- Let runtime adapters decide how to execute commands and transfer files into a
  runtime instance.
- Convert md container names to the connection target inside `mdruntime`, not in
  task orchestration.

Do this after the lifecycle naming cleanup, unless a VM adapter requires it
earlier.

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
- Do not rename md-specific adapter internals merely to remove the word
  container. If a type wraps `md.Container`, the adapter name can say so.
- Do not keep API or config compatibility as a design constraint. Use migration
  code only when it is small, isolated, and does not affect the target model.

## Suggested Order

1. Split `server.Config` validation by concern.
2. Extract startup assembly from `internal/server`.
3. Split `server.Server` by concern.
