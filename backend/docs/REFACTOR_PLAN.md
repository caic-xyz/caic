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
- `internal/server` exposes `Router`. It owns HTTP routing, request/response
  conversion, middleware, static assets, SSE framing, and API-facing behavior.
  It does not own application lifecycle, repo
  discovery, task orchestration, runtime state, CI state, forge clients,
  preferences, auth stores, model cache refresh, or maintenance loops.
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

## Runtime Connection Work

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

Sequence this after the lifecycle naming cleanup, unless a VM adapter requires
it earlier.

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
