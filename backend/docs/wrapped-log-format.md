# Wrapped task-log v2 rollout

Investigate and eliminate independently observable runtime log-integrity and
cleanup failures while preserving the completed v2 task-log cutover. Existing
v1 logs keep their bytes and behavior; corrupt or untrusted logs continue to
fail closed.

The canonical v2 agent-record contract is owned by
[`v2_record_test.go`](../internal/agent/v2_record_test.go),
[`v2_record_benchmark_test.go`](../internal/agent/v2_record_benchmark_test.go),
the [shared fixture](../internal/agent/testdata/v2_agent_records.json), and the
[v2 relay tests](../internal/agent/relay/test_relay_v2.py). Backend controls and
header/segment authority are covered by the shared
[parser tests](../internal/agent/agent_test.go) and task
[loading](../internal/task/load_test.go) and
[log-store](../internal/task/log_store_test.go) tests. Do not duplicate those
contracts here.

Across all phases:

- The first `caic_meta` header is the authority for version and harness, and a
  physical file remains entirely v1 or entirely v2.
- Existing v1 log bytes and behavior, relay behavior, replay event bodies, and
  public APIs remain compatible. Derived taskmeta and replay sidecars may change
  version and rebuild. Unknown or corrupt formats fail closed.
- Unreleased v2 behavior has no compatibility burden: remove superseded v2 code
  instead of adding aliases, fallbacks, or parallel implementations.
- Each phase deletes the paths it replaces. Do not land an unused abstraction or
  retain a raw compatibility path for a later cleanup phase.
- Production creates v2 logs. Existing logs continue according to their
  validated header authority.
- **Acceptance:** run the focused tests for the changed log lifecycle and
  `make frontend-e2e`; the latter verifies the fake harness emits and consumes
  the same v2 physical task-log records as production.

### Phase 1 — snapshot-authority: Explain and resolve replay snapshot mismatches

- **Scope:** task-log validation snapshots, replay-cache regeneration, and their
  focused fixtures/tests.
- **Preserve:** a changed, replaced, or corrupt task log never gains replay
  authority from a stale in-memory snapshot.
- **Verify:** a reproducible test distinguishes safe append growth from each
  rejected mutation; the observed mismatch has a documented cause and either a
  safe recovery path or an actionable terminal error.

### Phase 2 — derived-replay-publication: Establish replay cache publication and cleanup

- **Scope:** terminal and on-demand replay regeneration, cache body-file
  creation, publication, abort, and startup cleanup behavior.
- **Preserve:** terminal cache publication follows a fresh validated raw-log
  scan; incomplete or unproven replay data is never published. Active tasks use
  their in-memory stream and never own a replay sidecar writer.
- **Verify:** focused lifecycle tests cover terminal cache publication and cache
  misses; cancellation, source mutation, and completed/aborted regeneration
  leave no stale derived artifact. Startup removes only safely identifiable
  abandoned temporary files.

### Phase 3 — smoke-runtime-cleanup: Make real-runtime smoke resources self-cleaning

- **Scope:** smoke-test server/container lifecycle, cancellation, and test
  cleanup.
- **Preserve:** smoke continues to exercise the real `md` and relay runtime;
  cleanup never selects containers outside its own fixture.
- **Verify:** normal completion, setup failure, and cancellation leave no
  smoke-owned containers or persistent user cache state.
