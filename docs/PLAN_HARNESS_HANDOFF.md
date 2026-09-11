# Goal

Let users continue a task in a new agent through either an intentional fork or
a guided quota-recovery flow, while preserving the source runtime state and
making handoffs diagnosable. Current prompt and fork semantics are defined by
the [handoff builder](../backend/internal/task/handoff.go) and
[fork lifecycle](../backend/internal/task/taskmgr/lifecycle.go).

## Phase 1 — handoff-provenance: Identify handoff-created tasks

- **Scope:** Fork API metadata, persisted task provenance, task-info projection,
  and audit logging.
- **Preserve:** Ordinary forks remain distinguishable from generated handoffs;
  logs never contain the handoff prompt body.
- **Verify:** API coverage distinguishes ordinary forks, user-requested
  handoffs, and quota-recovery handoffs; provenance survives task reload and is
  visible in task info.

## Phase 2 — quota-recovery-entry: Offer a guided recovery action

- **Depends on:** handoff-provenance
- **Scope:** Task-level quota presentation and the web fork dialog entry flow.
- **Preserve:** The normal user-initiated Fork action and editable prompt remain
  available; successful recovery leaves the source task unchanged.
- **Verify:** A blocked task exposes a recovery action that opens an editable
  quota-aware handoff, creates a marked fork with the selected harness, and
  navigates to it in representative desktop and mobile flows.

## Phase 3 — quota-aware-targeting: Recommend safer target harnesses

- **Depends on:** quota-recovery-entry
- **Scope:** Harness quota-group metadata, candidate ordering, and target
  selection in the handoff dialog.
- **Preserve:** Selection requires user confirmation; unknown quota status is
  presented as unknown rather than available; normal fork selection remains
  unrestricted.
- **Verify:** Known candidates sharing the exhausted quota group are not
  recommended, preferred viable alternatives sort first, and unknown candidates
  remain selectable without availability claims.
