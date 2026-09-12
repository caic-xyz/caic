# Task hierarchy and delegated child tasks

## Goal

Allow a CAIC-MCP-enabled task to delegate bounded work to child tasks
through a task-scoped MCP connection, without granting it the user's general
MCP authority or access to unrelated tasks.

## Model

A task has two independent relationships:

- **Fork provenance** (`ForkedFromTaskID`) identifies the task whose runtime
  snapshot and branches seeded this task. It remains the meaning of the
  existing fork API.
- **Delegation hierarchy** (`ParentTaskID`) identifies the task that delegated
  responsibility for this task. It is absent for root tasks and ordinary
  user-created forks.

A delegated child currently has both values set to its source parent. The
fields must not be conflated: a user may fork a task without creating a child,
and future creation modes may create a child from a source other than its
logical parent.

The hierarchy is a directed tree. A child has one immutable parent, selected
by the server—not a client-provided argument—for task-initiated creation.
No operation changes it. A root task is identified by following
`ParentTaskID`; it need not be stored separately.

Each child is an ordinary isolated task with its own branch, runtime instance,
session, logs, result, and lifecycle. Parent state never cascades: stopping,
purging, failing, or completing a parent does not stop, delete, or otherwise
change a child. The parent remains useful as historical context after it has
settled.

## Delegated-child behavior

The future task-initiated child-creation implementation uses the existing
runtime fork path:
the child starts from the parent task's current container and repository
snapshot, receives a new branch and clean agent session, and runs its own
prompt. An agent cannot choose a different source task, owner, repository set,
harness, model, resource limits, mounts, or privileged capabilities.

Tasks may create children only when `CaicMCPEnabled` is true, and the
capability is disabled by default. It enables the task-local CAIC MCP bridge.
Phase 1 exposes only `task_create` and deliberately does not propagate the capability:
each created child has `CaicMCPEnabled` false. Depth, child-count, concurrency,
and budget policies will be added before recursive delegation is enabled.

Phase 1 does not yet provide durable idempotency keys. A subsequent policy
phase will associate a caller-generated key with the parent and resulting
child so retried MCP calls cannot create duplicate work.

The initial task-facing tool set is intentionally one generic tool:

- `task_create(prompt)`

For a task-scoped client, its schema has only `prompt`. It cannot provide a
repository, source, parent, harness, runtime, capability, or CAIC MCP flag.

It does not include arbitrary task creation, arbitrary task fork, stopping or
messaging other tasks, repository cloning, branch pushing, privilege
elevation, or global task listing. Automatic merge, cancellation cascades, and
automatic delivery of a child result into the parent's conversation are also
out of scope for the first release.

## MCP trust boundary

Task containers launch a local stdio MCP server through the persistent relay;
they never contact the CAIC HTTP endpoint or receive a credential. The relay
forwards normal MCP `tools/list` and `tools/call` requests to the server's
task-scoped MCP registry. The server derives the source task, parent, and
snapshot from the registry scope, never from a container-supplied field. The
request cannot select a parent.

The relay bridge contains no bearer token, endpoint URL, or task identity in
the container configuration, task history, runtime metadata, or process
arguments. Each supported harness launches its native local configuration;
Pi uses its equivalent task-local extension.

Phase 1 allows no task-scoped reads: the bridge exposes only `task_create`.
Audit records include the ordinary MCP tool call; dedicated delegation audit
fields and subtree reads are follow-up work.

## API and persistence contract

`ParentTaskID`, `CaicMCPEnabled`, and child-creation idempotency metadata are
durable task metadata. They are restored from task logs and are exposed through
the versioned API and generated SDKs. The API presents both fork lineage and
delegation separately. Task-list responses should expose enough parent data to
render a tree without per-task requests; detailed child summaries and
aggregated cost or status can be added without changing the base relation.

There is no child-specific creation route or user-facing “create child”
control. `POST /tasks` remains the creation route. When its caller is a
task-scoped client, the server reconciles the trusted client properties with
the request and derives the logical parent and permitted source snapshot. The
request body never selects a parent, and a task does not need to know its own
parent ID. Human clients continue to create root tasks unless the server has a
separate trusted reason to derive a relationship.

Existing `ForkedFromTaskID` data remains readable and unchanged. Old task logs
that do not contain hierarchy data represent root tasks.

## Delivery plan

### Phase 1 — task-mcp-bridge: Expose a narrow local MCP bridge to enabled tasks

- **Scope:** relay-backed stdio MCP bridge, task runtime launch configuration,
  native harness adapters, and reconciliation of task-scoped `task_create`
  into a server-derived child fork.
- **Preserve:** no task receives the general MCP endpoint, a user OAuth token,
  or global task permissions.
- **Verify:** an enabled task reaches only prompt-only `task_create`;
  disabled or terminal identities fail closed; the server derives the parent
  and snapshot; secrets are absent from logs, task history, runtime labels,
  and command arguments.

### Phase 2 — delegation-policy: Bound and account for child creation

- **Depends on:** task-mcp-bridge
- **Scope:** durable idempotency, server-side depth/count/concurrency/budget
  enforcement, delegation audit fields, and opt-in recursive delegation.
- **Preserve:** children inherit the source snapshot and only approved
  capabilities; agents cannot select an arbitrary parent or alter inherited
  execution authority.
- **Verify:** duplicate requests create one child; depth, concurrency, and
  root limits are atomic; audit records tie each child to its caller and
  policy decision.

### Phase 3 — hierarchy-operations: Make supervision useful

- **Depends on:** delegation-policy
- **Scope:** hierarchy views, parent/child status, budget rollups,
  notifications, and additional harness adapters.
- **Preserve:** flat task views remain usable; no automatic merge or lifecycle
  cascade is introduced.
- **Verify:** hierarchy navigation works after a server restart; task-scoped
  status reads reveal only the caller's subtree; each supported harness has
  end-to-end delegation coverage.
