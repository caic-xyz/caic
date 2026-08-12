# Wrapped task-log v2 rollout

Finish v2 adoption while reducing the log implementation to one record-decoding
policy and one task-log append path with one owner for physical-log authority.
Existing v1 logs keep their bytes and behavior; new logs move to strict v2 only
after duplicate scanners, parsers, proof carriers, and raw append routes are
removed.

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
- Production continues creating v1 logs until cut-over.

### Phase 1 — unify-log-writing: Use one task-log append path

- **Scope:** replace `Options.LogW`, `SessionHandle.LogW`, free raw-write helpers,
  and both branches of `Task.WriteToLog` with one header-authoritative append
  owner; consolidate reopen validation and replay attachment; and migrate every
  backend write of native records and controls, including the Codex/OpenCode
  session-metadata handoff and metadata-write failure cleanup.
- **Preserve:** serialized writes, relay persistence semantics, pending-action
  uniqueness, and the existing handshake protocol. A matching validated snapshot
  keeps reopen validation bounded to identity and header proof; without one,
  reopen validates the complete log through EOF.
- **Verify:** repository audit finds no task-log `io.Writer`, path-based raw
  append bypass, separate snapshot versus fallback validator, stateful one-use
  replay-proof adapter, or divergent Codex/OpenCode metadata-write path. A failed
  session-metadata write terminates and reaps the relay process, and metadata is
  persisted before the session is returned. V1 golden bytes are unchanged,
  latent v2 writes match the linked fixture, mixed-version writes fail before
  emission, native input and handshake metadata are each persisted exactly once,
  and `go test ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/...` passes.

### Phase 2 — cut-over-to-v2: Enable v2 and prove restart behavior

- **Scope:** the new-file header default, relay selection, task creation,
  reopen/resume/adoption, caller-supplied version plumbing, and the real-runtime
  wrapped-log smoke test.
- **Preserve:** existing v1 files always resume as v1; existing files are never
  rewritten or repaired.
- **Verify:** new files contain only canonical v2 records, and the shared log
  owner supplies the immutable header version to relay selection, readers, and
  writers; `Options.LogVersion`, zero-as-v1 defaults, and other caller-supplied
  version paths are removed. Existing v1 and v2 tasks survive live and
  dead-relay restart without mixed records or duplicate input. Cutover tests
  cover missing-header and mismatched-version files. `go test
  ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/... ./backend/internal/server/...`, `python3
  backend/internal/agent/relay/test_relay.py`, `python3
  backend/internal/agent/relay/test_relay_v2.py`, and `make smoke` pass with
  deterministic cleanup.
