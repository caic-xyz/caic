# Bounded, supervisable CAIC-MCP delegation

Make server-derived children predictable to create and useful to supervise.
The work must retain task isolation: a delegated child is an ordinary task
with its own runtime and lifecycle, not an extension of its parent.

## Phase 1 — delegation-policy: Bounded, accountable child creation

- **Scope:** Durable caller idempotency keys; atomic depth, child-count,
  concurrency, and budget limits; policy-decision audit data; and an explicit
  recursive-delegation policy.
- **Preserve:** The server selects the parent and fork snapshot. Children do
  not automatically inherit the delegation grant until the recursive policy
  explicitly permits it. No lifecycle cascade or automatic merge is added.
- **Verify:** A repeated request produces one child; each limit holds under
  concurrent requests and after restart; and audit data identifies the source,
  resulting child, and decision.

## Phase 2 — hierarchy-supervision: Supervisable delegated task hierarchies

- **Depends on:** `delegation-policy`
- **Scope:** Complete parent/child navigation, child summaries, status and
  budget rollups, hierarchy notifications, and authorized task-scoped subtree
  reads.
- **Preserve:** Fork provenance and delegation parentage remain distinct;
  ordinary user-created forks remain roots; and a task-scoped reader cannot
  inspect unrelated tasks.
- **Verify:** A hierarchy restores after restart, flat views remain usable,
  rollups reconcile with child tasks, and scoped reads expose only the
  caller's subtree.

## Later

Automatic result delivery, merges, and lifecycle cascades are out of scope for
this plan.
