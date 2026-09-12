# Plan: Task hierarchy and delegation

## Goal

Let people opt a task into bounded CAIC-MCP delegation, then make its
server-derived children predictable to create and useful to supervise. The
work must retain task isolation: a delegated child is an ordinary task with
its own runtime and lifecycle, not an extension of its parent.

## Phase 1 — human-opt-in: Let people enable task MCP

- **Outcome:** A person can choose whether a newly created root task receives
  the local CAIC MCP bridge.
- **Scope:** Add the existing `CaicMCPEnabled` API field to the normal task
  creation UI and request state, with clear capability-oriented copy. Show the
  setting in task detail only where it avoids ambiguity about why delegation is
  or is not available.
- **Preserve:** Disabled remains the default; there is no child-creation UI;
  browsers do not provide a parent or source task; and the server continues to
  derive the relationship from the task-scoped client.
- **Verify:** UI and API tests cover both selections, the selected value
  persists into the created task, and only an enabled task receives the local
  bridge and prompt-only `task_create` tool.

## Phase 2 — delegation-policy: Bound and account for delegated work

- **Depends on:** `human-opt-in`
- **Outcome:** Retried or abusive delegation requests cannot create
  unbounded, duplicate, or unaccounted-for child work.
- **Scope:** Durable caller idempotency keys; atomic depth, child-count,
  concurrency, and budget limits; policy-decision audit data; and an explicit
  recursive-delegation policy.
- **Preserve:** The server selects the parent and fork snapshot. Children do
  not automatically inherit the delegation grant until the recursive policy
  explicitly permits it. No lifecycle cascade or automatic merge is added.
- **Verify:** A repeated request produces one child; each limit holds under
  concurrent requests and after restart; and audit data identifies the source,
  resulting child, and decision.

## Phase 3 — hierarchy-supervision: Make delegated work understandable

- **Depends on:** `delegation-policy`
- **Outcome:** A person can follow a hierarchy and assess child progress,
  status, and resource use without losing the existing flat task workflow.
- **Scope:** Complete parent/child navigation, child summaries, status and
  budget rollups, hierarchy notifications, and authorized task-scoped subtree
  reads.
- **Preserve:** Fork provenance and delegation parentage remain distinct;
  ordinary user-created forks remain roots; and a task-scoped reader cannot
  inspect unrelated tasks.
- **Verify:** A hierarchy restores after restart, flat views remain usable,
  rollups reconcile with child tasks, and scoped reads expose only the caller's
  subtree.

## Later

Automatic result delivery, merges, and lifecycle cascades are out of scope for
this plan.
