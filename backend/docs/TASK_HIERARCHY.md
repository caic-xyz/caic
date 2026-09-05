# Task hierarchy and delegated child tasks

## Goal

Allow an explicitly enabled CAIC task to delegate bounded work to child tasks
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

A delegated child initially has both values set to its source parent. The
fields must not be conflated: a user may fork a task without creating a child,
and future creation modes may create a child from a source other than its
logical parent.

The hierarchy is a directed tree. The intended contract is that a child has
one immutable parent, selected by the server—not a client-provided argument—
for task-initiated creation. Phase 1 represents `ParentTaskID` as immutable
creation metadata by convention: task creation and loading set it once, and
no operation changes it. Creation-time enforcement of single assignment and
tree validity begins in Phase 2. A root task is identified by following
`ParentTaskID`; it need not be stored separately in the initial design.

Each child is an ordinary isolated task with its own branch, runtime instance,
session, logs, result, and lifecycle. Parent state never cascades: stopping,
purging, failing, or completing a parent does not stop, delete, or otherwise
change a child. The parent remains useful as historical context after it has
settled.

## Delegated-child behavior

The first child-creation implementation uses the existing runtime fork path:
the child starts from the parent task's current container and repository
snapshot, receives a new branch and clean agent session, and runs its own
prompt. An agent cannot choose a different source task, owner, repository set,
harness, model, resource limits, mounts, or privileged capabilities.

Tasks may create children only under an explicit delegation policy, disabled
by default. The policy has server-enforced limits for maximum depth, total
children per root, and concurrent children per root. Limits are checked
atomically, including concurrent tool calls. A child's ability to delegate is
derived from the remaining depth; at depth zero it receives no delegation
capability.

Child creation is idempotent. Each request supplies a caller-generated
`idempotencyKey`; the server durably associates that key with the parent and
resulting child so retried MCP calls cannot create duplicate work.

The initial task-facing tool set is intentionally narrow:

- `task_create_child(prompt, idempotencyKey)`
- `task_children_list()`
- `task_child_get_detail(childID)`

It does not include arbitrary task creation, arbitrary task fork, stopping or
messaging other tasks, repository cloning, branch pushing, privilege
elevation, or global task listing. Automatic merge, cancellation cascades, and
automatic delivery of a child result into the parent's conversation are also
out of scope for the first release.

## MCP trust boundary

Task containers use a separate task-scoped MCP endpoint, not CAIC's existing
human/client MCP endpoint. A task credential identifies one calling task and
is valid only while that task remains enabled by its delegation policy. The
endpoint binds the credential's task ID to the route and derives the parent
from that identity; it never accepts a caller-selected parent ID.

The credential must be scoped to the task, revocable when delegation is
disabled or the task becomes terminal, and unavailable in logs, task history,
runtime metadata, or process arguments. Runtime launch provisioners inject it
only into a server-controlled MCP client configuration. Harness adapters are
introduced one at a time; unsupported harnesses fail closed and do not receive
the endpoint or credential.

Every delegated action is audited with caller task ID, parent ID, child ID,
idempotency key, policy decision, and outcome. Task-scoped reads are restricted
to the caller and its descendants.

## API and persistence contract

`ParentTaskID`, delegation policy, and child-creation idempotency metadata are
durable task metadata. They are restored from task logs and are exposed through
the versioned API and generated SDKs. The API presents both fork lineage and
delegation separately. Task-list responses should expose enough parent data to
render a tree without per-task requests; detailed child summaries and
aggregated cost or status can be added without changing the base relation.

Existing `ForkedFromTaskID` data remains readable and unchanged. Old task logs
that do not contain hierarchy data represent root tasks.

## Delivery plan

### Phase 1 — hierarchy-contract: Define durable hierarchy semantics

- **Scope:** architecture decision, task/log/API contract.
- **Preserve:** existing `ForkedFromTaskID` meaning and existing fork API
  behavior.
- **Verify:** task-log headers, terminal summaries, and header-cache entries
  preserve `ParentTaskID`; settled loading and runtime import restore it; old
  logs default to root tasks; ordinary forks retain zero `ParentTaskID`; and
  the v1 API and generated SDKs expose the relation.

### Phase 2 — user-managed-children: Make hierarchy real without agent access

- **Depends on:** hierarchy-contract
- **Scope:** task manager, persistence, API/SDK, task detail/list UI.
- **Preserve:** a regular fork remains a fork, not automatically a delegated
  child.
- **Verify:** a user can create and browse a child; a parent cannot be
  reassigned after creation; reload/import retains the tree; direct-child and
  no-cycle rules are enforced; parent stop/purge leaves children intact; API
  generation, frontend lint/E2E, and repository checks pass.

### Phase 3 — task-mcp-identity: Provision a narrow MCP identity to enabled tasks

- **Depends on:** user-managed-children
- **Scope:** task launch configuration, MCP authentication and authorization,
  first supported harness adapter.
- **Preserve:** no task receives the general MCP endpoint, a user OAuth token,
  or global task permissions.
- **Verify:** an enabled task reaches only its task-specific endpoint;
  disabled, expired, or revoked identities fail closed; secrets are absent from
  logs, task history, runtime labels, and command arguments.

### Phase 4 — delegated-children: Expose bounded child creation

- **Depends on:** task-mcp-identity
- **Scope:** task-facing child tools, server-side policy enforcement, auditing.
- **Preserve:** children inherit the source snapshot and only approved
  capabilities; agents cannot select an arbitrary parent or alter inherited
  execution authority.
- **Verify:** duplicate requests create one child; depth, concurrency, and
  root limits are atomic; audit records tie each child to its caller and
  policy decision.

### Phase 5 — hierarchy-operations: Make supervision useful

- **Depends on:** delegated-children
- **Scope:** hierarchy views, parent/child status, budget rollups,
  notifications, and additional harness adapters.
- **Preserve:** flat task views remain usable; no automatic merge or lifecycle
  cascade is introduced.
- **Verify:** hierarchy navigation works after a server restart; task-scoped
  status reads reveal only the caller's subtree; each supported harness has
  end-to-end delegation coverage.
