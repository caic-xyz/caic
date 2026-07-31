# Wrapped task-log format (v2) — design and rollout status

## Current status

The physical authority foundation is shipped at the shared comparison role,
`origin/main`. The accepted local integration state adds the shared versioned
parser, but that work is unshipped:

- `LogVersion` is a closed type supporting exactly v1 and v2; unknown versions
  fail closed.
- All production plain and zstd readers enforce the first non-empty `caic_meta`
  header's version and harness and reject changed later segment headers.
- The rebuildable `.taskmeta.json` cache stores only derived version/harness,
  its cache version is bumped, and its raw-file identity is validated before use.
- Raw append opens and validates the same inode before writing. It covers
  `LogStore.Open`, `LogStore.Reopen`, and the path-based `Task.WriteToLog`
  fallback, and remains byte-compatible for v1.
- Missing, corrupt, mismatched, unknown-version, and valid-v2 logs fail closed
  before raw append. Missing local logs no longer create a replacement header
  during live reconnect.
- `LogRecordParser` provides exact v1/v2 dispatch, the complete shared control
  converter, strict UTF-8 and v2-envelope validation, Pi model-context snapshot
  state, nil-safe ordered state application, and deterministic pending-action
  deduplication.
- `ParsedMessage` has replaced `TimestampMessage` with explicit outer parsed-
  message metadata. That dependency-safe correction passed independent review
  and is accepted, completed, and unshipped; `LogRecordParser` still has no
  production consumer.

For compatibility, breaking-change, and churn review, resolve `origin/main`
immediately before dispatch and freeze its exact commit as `SHIPPED_BASE` in the
ephemeral orchestration manifest. Resolve the accepted local parser state as
`LOCAL_BASE` in that same manifest. The local parser work is an integration
prerequisite, not a published contract; implementation that exists only after
`SHIPPED_BASE` may be freely reshaped or deleted within an approved phase scope.

The remaining production behavior is still legacy v1:

- New physical task-log files start with `{"type":"caic_meta","version":1,...}`.
- Relay stdout is persisted as bare harness-native JSONL. Relay controls use
  `caic_diff_stat`, `caic_exit`, and `caic_stripped_env`.
- Backend controls, metadata, provisioning output, prompt/compact input, Pi
  startup traffic, and other direct writes still use the v1 vocabulary. Codex
  and OpenCode handshake traffic is not persisted in task logs.
- Production disk/live/export call sites do not yet use `LogRecordParser`, and
  harness parsers still recognize selected `caic_*` records. There is no v2
  relay yet.
- `ToolTimingTracker` does not yet consume parsed producer-time metadata, and no
  production relay emits persisted per-message timestamps.
- `eventreplay.CacheVersion` is 4.

Authority validation currently makes multiple full passes over very large plain
logs during startup inventory, session restoration, message restoration, and
reopen. Users have confirmed this adoption latency is already material. The
active `adoption-scan-performance` phase owns a realistic benchmark and a
correctness-preserving removal of redundant passes. Later reader and cutover
phases may not regress its accepted deterministic complete-pass count or logical
read amplification. Wall time, throughput, allocations, physical-read deltas,
and benchstat comparisons are advisory same-host evidence with no fixed
threshold.

New writers must remain on v1 until `v2-producer-cutover` passes its gate. The
current untyped raw append boundary deliberately rejects valid-v2 files; only the
accepted `versioned-log-sink` phase may lift that temporary restriction.

## Goals and non-goals

v1 guesses provenance because bare harness records and caic controls share the
same top-level namespace. v2 removes that guess: every physical line is a
caic-defined record, and every persisted harness-native record is nested in an
`agent` envelope.

This rollout must:

- keep each physical task-log file pure v1 or pure v2;
- preserve existing v1 bytes and behavior until the final producer cutover;
- select readers and writers from the file's authoritative header, never a
  remembered launch flag;
- make harness parsers native-only;
- preserve relay byte offsets across live attach and adoption;
- fail closed rather than inventing a version; and
- keep the product functional at every accepted phase boundary.

This rollout does not change API DTOs, database schemas, migrations, harness
wire protocols, or the event-replay sidecar schema. The sidecar cache version is
bumped because conversion semantics change, not because its encoding changes.

## Locked format decisions

### Versions and header authority

`LogVersion` is a closed typed value with exactly two supported values:

- `LogVersionV1 = 1`
- `LogVersionV2 = 2`

A reader must match these values explicitly. Unknown versions fail closed; a
future version does not silently use the v2 parser.

The first non-empty record in a physical task-log file must be `caic_meta`. Its
`version` and `harness` are authoritative for the entire file. Later
`caic_meta` records are legal segment markers only when both fields exactly
match that first header. A missing first header, a changed version, or a changed
harness is corruption.

`LogStore.Reopen`, path-based `Task.WriteToLog`, and every adoption/resume path
read this first-header authority from the same inode they would append. If the
local header is missing or unreadable, adoption fails closed: no relay attach,
relay restart, replacement header, or append is allowed. This is intentionally
stricter than guessing from runtime metadata or the deployed script. Until the
typed versioned sink is integrated, raw append accepts only valid v1 authority;
valid v2 remains readable but is rejected before append so an untyped writer
cannot create a mixed file.

### v1 and v2 framing

A v1 file remains the frozen legacy format:

- `caic_meta` version 1;
- bare harness-native records;
- `caic_*` controls and metadata; and
- the existing v1 relay script and byte stream.

Do not add `caic_ts` to v1.

A v2 file contains only caic-defined top-level records:

- `caic_meta` version 2 remains the version bootstrap anchor;
- harness-native stdin and stdout records are
  `{"type":"agent","ts":<epoch-seconds>,"msg":<native JSON value>}`;
- caic controls and metadata use unprefixed types; and
- an unknown top-level type is corruption, not a harness fallback.

`agent.msg` preserves the native JSON value without semantic reserialization.
If a logical harness line is not valid JSON or UTF-8, `msg` is a JSON string
containing a bounded diagnostic representation. The v2 relay and backend sink
must size the final encoded physical record, not merely the raw input. The
complete encoded record, including its terminating LF, must be strictly smaller
than the shared 32 MiB scanner limit. An oversized logical line produces one
bounded diagnostic record and is never silently split.

### v2 vocabulary

| Purpose | v1 physical type | v2 physical type | Producer |
| --- | --- | --- | --- |
| Header/segment marker | `caic_meta` | `caic_meta` | backend log store |
| Harness stdin/stdout | bare native value | `agent` | relay or versioned sink |
| Diff stat | `caic_diff_stat` | `diff_stat` | relay/backend |
| Process exit | `caic_exit` | `exit` | relay |
| Stripped environment | `caic_stripped_env` | `stripped_env` | relay |
| Session snapshot | `caic_session` | `session` | backend |
| Legacy session alias | `caic_init` | none | v1 reader only |
| Model context snapshot | `caic_model_info` | `model_info` | backend/shared parser |
| PR snapshot | `caic_pr` | `pr` | backend |
| Result trailer | `caic_result` | `result` | backend |
| Pending user action | `caic_pending_user_action` | `pending_user_action` | backend |
| Provisioning/startup log | `caic_log` | `log` | backend |
| Context reset marker | existing v1 native record | `context_cleared` | backend |

A Pi parser may synthesize a native `caic_log`-shaped semantic message from Pi
payload. In v2 that payload still lives under `agent`; it is not the top-level
backend `log` control.

`pending_user_action` remains a compatibility/import token and may represent an
explicit backend snapshot. New writers do not emit it when the persisted native
Claude `control_request` reconstructs the same action. Pending actions have the
semantic identity `(kind, request ID, tool-use ID)`: compatibility files that
contain both representations are deterministically deduplicated, and a sink may
append a top-level snapshot only when no equivalent native source record is
persisted.

### Reader contract

There are two structural readers selected once from file authority. The shared
parser returns an outer stream value:

```go
type ParsedMessage struct {
	Message      Message
	ProducerTime time.Time
}
```

`ParsedMessage` does not implement `Message` and exposes no `Type()` method.
Consumers must explicitly unwrap `.Message`, preserving the concrete message for
Go type switches and creating compiler pressure at every parsed-stream boundary.
There is no `HasTime` field: absence is exactly `ProducerTime.IsZero()`.
Harness-native parsers keep their existing `[]Message` result; only
`LogRecordParser` adds the outer metadata and returns parsed values.

```text
read authority from first caic_meta
switch version exactly:
  1 -> parseV1
  2 -> parseV2
  default -> unsupported-version error

parseV1(record):
  recognized caic_* control -> shared control conversion
  otherwise                 -> native harness parser
  wrap every semantic result with zero ProducerTime

parseV2(record):
  agent:
    validate ts/msg
    messages = native harness parser(msg)
    wrap every semantic result with ProducerTime = envelope ts
  recognized v2 control:
    messages = shared control conversion
    wrap every semantic result with zero ProducerTime
  otherwise -> corruption error
```

The shared log parser owns control conversion and parsing state. In particular,
it owns the v1 `caic_model_info` / v2 `model_info` context-window snapshot and
applies that snapshot to parsed Pi usage when the native event lacks the value.
The Pi native parser does not route caic records.

Every semantic message produced from one valid v2 `agent` envelope carries that
same immutable producer timestamp, including messages that are irrelevant to
tool timing. V1 records and controls without envelope producer time carry zero
time. Timing relevance is exclusively downstream `ToolTimingTracker` policy,
not parser framing policy. Producer timestamps are metadata only: they never
become semantic or user-visible events, and replay compaction and last-message/
control-boundary searches operate on the explicitly unwrapped message.

### Relay and direct-write contract

`relay.py` remains frozen v1. `relay_v2.py` is a separate, v2-only script; code
is copied rather than shared so a relay process cannot switch vocabularies.

The v2 relay frames every persisted stdout line and every stdin line that the
relay is configured to log. It preserves the v1 stream asymmetry:
`output.jsonl` contains framed stdout, controls, and (when `log_stdin` is
enabled) framed stdin, while the live connected client receives stdout and
controls without an stdin echo. Every record sent to the client is byte-for-byte
identical to its copy in `output.jsonl`, but `output.jsonl` is a superset. Attach
replay from a physical byte offset may therefore replay persisted stdin. No
direction field is added; native payloads retain their existing protocol type.
Timestamps are captured at the producer when the logical record is observed.

A shared Go version-aware relay-record reader:

- consumes physical v1 or v2 records according to `LogVersion`;
- takes an explicit per-consumer task-log persistence policy;
- appends normal live output exactly once and never copies live relay stdin;
- validates v2 top-level framing;
- unwraps native payloads for Codex, OpenCode, and Pi startup handshakes;
- routes controls without presenting them to native handshake parsers; and
- advances physical relay offsets by bytes consumed from the physical stream.

This reader is required for `DefaultReadMessages`, Pi's startup response loop,
Codex and OpenCode handshakes, tail replay, and attach. Persistence policy stays
compatible with v1: Pi startup records that are currently written to the task log
remain persisted through the sink; Codex and OpenCode handshake requests and
responses remain unpersisted in both versions. Codex and OpenCode's custom
deployment/launch paths must use the same version selection as `PrepareRelay`.
Attach replay and task-log restoration must deduplicate overlap so a relay copy
of persisted stdin cannot duplicate the sink's live prompt/compact/`SendRaw`
record.

The task-log append boundary is an enforcing versioned sink, not an untyped
`io.Writer`. Its authority is the physical file's `LogVersion`. It provides
explicit operations for native records and caic controls, validates vocabulary,
and refuses mixed writes. The sink is the exclusive live task-log persistence
path for prompt, compact, and `SendRaw` input. Together with the version-aware
relay reader, it covers all current write paths, including:

- relay stdout and controls, plus attach replay after overlap removal;
- prompt, compact, and `SendRaw` native input;
- persisted Pi startup commands and responses;
- session and model-context snapshots;
- `context_cleared` and non-duplicated pending-user-action snapshots;
- result and PR metadata;
- `Task.WriteToLog`;
- provisioning and startup output; and
- reopen/adoption appends.

While the sink and callers are introduced, all active producers remain v1 and
must preserve their previous bytes. Only `v2-producer-cutover` changes the new
file version and selects v2 producer vocabulary.

## Authority model

- **Authoritative:** the first non-empty `caic_meta.version` and
  `caic_meta.harness` in each physical task-log file.
- **Derived:** the selected parser, selected relay script, control vocabulary,
  sink behavior, cached in-memory `LogVersion`, and `.taskmeta.json` compressed-
  log metadata cache. A taskmeta cache may copy log version/harness only after
  raw-log identity validation; it is rebuildable and never an authority.
- **Immutable snapshot:** session, model context, PR, result, pending action,
  provisioning log, context-reset, and native protocol records already appended
  to a file.
- **Provenance:** `agent` framing identifies native harness protocol content;
  top-level controls identify caic-owned content. Provenance is not identity.
- **Presentation:** rendered events, exported markdown, warnings, and bounded
  malformed-line diagnostics.
- **Redundant:** launch flags, runtime metadata, deployed filenames, and relay
  liveness are not alternate version authorities and must never be used as a
  fallback.

Forbidden additions and fallbacks:

- no second authoritative version field outside `caic_meta`; the sole approved
  persisted copy is the identity-bound, rebuildable `.taskmeta.json` cache;
- no version inference from type prefixes, task age, binary version, runtime
  metadata, relay filename, or live process;
- no `version >= 2` compatibility shortcut;
- no per-line v1/v2 guessing;
- no v2 fallback to a native harness parser for an unknown top-level type;
- no rewriting a missing header during adoption;
- no mixed-format recovery append; and
- no schema migration or public API field for log version.

## Preflight manifest

The living plan records symbolic prerequisites; exact Git identities are
volatile execution evidence. The base roles and complete manifest must be
freshly captured and recorded immediately before each phase dispatch,
integration, and review:

- **Shipped/shared comparison role:** resolve `origin/main` to an exact commit and
  record it as `SHIPPED_BASE` in the ephemeral orchestration manifest.
  Compatibility, breaking-change, and combined parser churn use that frozen
  value, never the later value of a moving `origin/main`.
- **Local dispatch/integration role:** confirm that the accepted unshipped shared
  parser is present in exactly
  `backend/internal/agent/{agent.go,agent_test.go,types.go}`, then record actual
  HEAD as `LOCAL_BASE` in the fresh ephemeral manifest. The parser has no
  production consumers and does not establish a published API contract.
- **Completed phase role:** after implementation and again after integration,
  freshly capture and record the exact resulting HEAD as `PHASE_FINAL` in
  ephemeral status and the phase handoff. Immediately before review, freshly
  capture and record all three exact values in the reviewer prompt.

Focused and full agent tests, the changed-package race test, vet, mandatory lint,
method placement, diff, and status checks passed for the accepted local parser.
The full `agent/...` race run still has the pre-existing Pi one-second timeout
reproduced at its clean comparison base; it is not evidence against the local
parser and remains outside its phase. `ParsedMessage` replaced `TimestampMessage`
and passed independent review. The dependency-safe `parsed-message-metadata`
phase is accepted and completed out of order ahead of Phase 1; it remains listed
until its predecessor is removed and is not eligible for redispatch.

No next phase may dispatch unless the worktree condition matches its symbolic
Base state. Immediately before dispatch, integration, and review, freshly
capture and record actual HEAD, exact resolved base/upstream commits, dirty/
staged/untracked paths, and sensitive-file statistics in a dispatch manifest,
run artifact/status, phase handoff, or reviewer prompt as appropriate. Reusing
or merely verifying an earlier manifest is insufficient. Any rebase, other
history change, or worktree/index status change invalidates the ephemeral
manifest and requires fresh capture and recording before continuing; it does not
require a plan edit while the symbolic prerequisites still hold.

- **Expected worktree condition:** clean or limited to the exact user-accepted
  local parser delta and the declared writer-owned phase scope; no unexplained
  staged, untracked, or generated paths
- **VCS authority:** the user owns staging and commits. Subagents must not stage,
  commit, reset, rebase, merge, or push. No history rewrite, amend, or squash may
  occur without explicit user authorization. Restore operations require
  coordinator approval and may touch only executor-created output.
- **Canonical command cwd:** `/home/user/src/caic`, unless a phase explicitly
  gives another cwd
- **Generated artifacts:** none expected for this docs rewrite. Implementation
  phases must declare any generator and exact output before running it.
- **Shipped contracts:** no migration, schema, or public API DTO change is
  approved by this plan.
- **Sensitive baseline:** freshly capture and record this document and every
  phase-sensitive file immediately before dispatch, integration, and review.

Global phase rules:

1. One phase-executor subagent owns each phase. One writer may operate in a
   worktree at a time unless isolated worktrees and the recorded concurrency
   contract are both in use.
2. Immediately before dispatch, integration, and review, freshly capture and
   record exact actual HEAD, base/upstream commits, dirty/staged/untracked paths,
   per-file baseline statistics for sensitive files, and the pre-integration
   target commit in ephemeral orchestration state. Freeze `SHIPPED_BASE`,
   `LOCAL_BASE`, and `PHASE_FINAL` for comparisons and review; never substitute a
   live moving ref or reuse an earlier manifest. Any rebase, other history
   change, or worktree/index status change invalidates the ephemeral manifest and
   requires fresh capture and recording before continuing.
3. Focused validation runs first. Every implementation phase then runs
   `make lint-fix`, `git diff --check`, and staged/filesystem-delta checks from
   the canonical cwd. Any unexpected generated or out-of-scope change fails the
   gate. When an approved generator discovers inputs only through
   `git ls-files`, the coordinator must first obtain a user-owned `git add -N`
   checkpoint for the exact approved new inputs. This is intent-to-add only:
   executors and integrators may not stage content, `git diff --cached
   --exit-code` must stay clean, status must account explicitly for every intent
   entry, and the user owns later index cleanup, staging, and history.
4. In serial mode, the active-worktree phase executor is the sole writer; its
   validated output is already on the integration target, so there is no merge
   or transfer step. With approved isolated-worktree concurrency, a separately
   designated integration subagent is the sole target writer and may use only
   `git apply` to transfer accepted output. It may not stage, commit, merge,
   reset, rebase, or push; the user owns staging and history. Integrated
   validation and a fresh read-only `PASS` review are required before removal.
5. Compatibility and breaking-change review considers only contracts present at
   frozen `SHIPPED_BASE`. For work built on accepted unshipped local changes,
   review both `SHIPPED_BASE...PHASE_FINAL` for the final combined change and
   `LOCAL_BASE...PHASE_FINAL` for the focused phase delta; local-only
   implementation may be reshaped or deleted inside the phase's exact write
   scope without churn objections or compatibility adapters.
6. The plan-maintenance subagent applies learnings, moves unresolved work into a
   named phase, deletes the accepted leading phase, and renumbers the remainder.
   Stable IDs never change.
7. `make check` is required at cutover and final gates. Runtime validation uses
   the real md/container path, never the fake server or `smoketest` backend.

## Dependency graph

```text
adoption-scan-performance -> persistent-read-paths
parsed-message-metadata -> persistent-read-paths
parsed-message-metadata -> live-relay-read-path
relay-v2 -> live-relay-read-path

persistent-read-paths -> pure-harness-parsers
live-relay-read-path -> pure-harness-parsers
persistent-read-paths -> timestamp-cache-semantics
live-relay-read-path -> timestamp-cache-semantics
persistent-read-paths -> versioned-log-sink
versioned-log-sink -> agent-direct-write-migration
live-relay-read-path -> agent-direct-write-migration
pure-harness-parsers -> agent-direct-write-migration

pure-harness-parsers -> v2-producer-cutover
timestamp-cache-semantics -> v2-producer-cutover
agent-direct-write-migration -> v2-producer-cutover
v2-producer-cutover -> runtime-adoption-smoke
```

After a clean base is recorded, `adoption-scan-performance` remains the next
performance priority. `parsed-message-metadata` completed dependency-safely
before it, is not eligible for redispatch, and remains listed only until Phase 1
is integrated and removed. Its accepted result remains a prerequisite for both
reader phases. `persistent-read-paths` starts only after the performance phase is
integrated; `live-relay-read-path` starts only after `relay-v2` is integrated. A
still-pending `relay-v2` may run with `persistent-read-paths` in an isolated
worktree.
`versioned-log-sink` may not run with persistent-reader work because its reopen
constructor consumes the accepted authority scan result. `pure-harness-parsers`
and `timestamp-cache-semantics` may run together after both read paths are
integrated. Integration remains one phase at a time.

## Active phased rollout

### Phase 1: Bound adoption scan amplification

- **Stable ID:** `adoption-scan-performance`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** none; `parsed-message-metadata` is already accepted/completed
  and is not eligible for redispatch
- **Base state:** no stable phase dependency; the physical authority foundation
  is assumed shipped at the resolved shared-base role, the accepted local parser
  remains unshipped, and the worktree matches the declared clean/accepted-delta
  condition. Immediately before dispatch, resolve actual HEAD as `LOCAL_BASE`,
  resolve and freeze `origin/main` as `SHIPPED_BASE`, record any configured
  upstream and status, and capture plain/zstd loader file statistics, the
  app/taskmgr live-adoption call graph, and current full-pass behavior in the
  ephemeral manifest. The coordinator also records an absolute phase artifact
  directory outside the repository; a relative or repository-contained path
  blocks dispatch.
- **VCS authority:** an active-worktree executor writes directly; approved
  isolated output is transferred only by a designated integration subagent using
  `git apply`; the user owns staging and history; no subagent may stage, commit,
  merge, reset, rebase, or push. Before generated-index replay, the coordinator
  must obtain a user-owned intent-to-add checkpoint for exactly
  `backend/internal/task/load_benchmark_test.go`,
  `backend/internal/task/load_benchmark_cache_linux_test.go`, and
  `backend/internal/task/load_benchmark_cache_other_test.go`; the executor
  verifies no staged content with `git diff --cached --exit-code`.
- **Write scope:** production changes are limited to task-log inventory, session,
  message, tail, reopen, and local adoption-preparation paths under
  `backend/internal/task` plus the narrow `backend/internal/task/taskmgr` caller
  boundary where passes can be fused. The exact approved test files are the three
  new benchmark/helper files above plus existing
  `backend/internal/task/load_test.go`,
  `backend/internal/task/log_store_test.go`, and
  `backend/internal/task/taskmgr/manager_test.go`. Generated output is limited to
  `backend/AGENTS.md`.
- **Data authority:** first-header version/harness and same-inode validation stay
  authoritative; reused scan/snapshot state is derived, identity-bound, and valid
  only for the exact inode/size/mtime it observed; `.taskmeta.json` remains a
  rebuildable cache rather than an alternate authority.
- **Purpose:** establish a realistic adoption baseline and remove redundant
  full-file passes that already cause material startup latency.
- **Deliverables:** `BenchmarkTaskAdoption` has independent warm and Linux-cold
  subbenchmarks for each operation and for the exact combined live-adoption
  sequence used by app/taskmgr: `LoadLogsForTaskIDs` for the target task IDs,
  matching harness parser setup, conditional `LoadSessionMetadata` when the
  session/version fields are absent, full `LoadMessages`, then a validated
  `LogStore.Reopen` open/close. `LoadLogs` purged-task inventory and
  `LoadMessagesTail` on-demand loading remain separate subbenchmarks and never
  substitute for the combined sequence. One bounded shared fixture per benchmark
  process contains a realistic NDJSON mix of small deltas, normal events, large
  tool outputs, controls, and repeated matching segment headers. It is generated
  and synced in `b.TempDir` before timing; each operation resets its own state.
  Warm mode primes that exact operation, then resets only its in-memory operation
  state with the timer stopped before every measured iteration. Cold mode closes
  operation handles, opens a dedicated fixture descriptor, syncs it, invokes
  per-fixture `unix.Fadvise(..., unix.FADV_DONTNEED)`, and closes it with the timer
  stopped before every measured iteration. Subbenchmarks have no execution-order
  dependency.
  A kernel refusal marks cold results unsupported/advisory rather than silently
  making them warm; non-Linux reports cold mode unsupported. Neither mode uses
  root nor global `drop_caches`.
- **Measurement boundary:** optimization may introduce only package-private,
  natural production boundaries for opened physical readers or identity-bound
  snapshot reuse. Where production benefits, those boundaries accept an
  `io.Reader` or `io.ReadSeeker`; benchmark and correctness tests wrap the actual
  readers with counting readers. No global hook, public instrumentation API, or
  test-only seam is allowed. For each physical file, deterministic logical bytes
  are the sum returned by its counting readers; read amplification is that sum
  divided by the fixture's on-disk byte length, and a complete pass is a reader
  that reaches EOF after consuming that length. Only the deterministic read-
  amplification ratios and complete-pass counts are the hard performance gate;
  logical-byte totals are evidence used to compute the ratios. The combined Linux
  path also records `/proc/self/io` `rchar` and `read_bytes` deltas as advisory
  corroboration. Wall time, throughput, allocations, physical-read deltas, and
  benchstat comparisons are advisory same-host evidence with no fixed threshold.
- **Benchmark checkpoint:** implementation starts with only the three new
  benchmark/helper files and generated `backend/AGENTS.md`. After the user-owned
  intent-to-add checkpoint, the executor runs `make lint-fix`, confirms that the
  phase has zero production-file diff, and records a SHA-256 manifest over all
  three byte-identical benchmark/helper sources (the fixture generator lives in
  `load_benchmark_test.go`) in the external artifact directory. Only then may it
  run the before baseline. Production optimization begins after that baseline.
  The after run is invalid unless the same three-source manifest verifies
  byte-for-byte unchanged.
- **Generated artifacts:** all three new Go files share the
  `adoption_benchmark` build tag and remain excluded from ordinary test builds.
  Their exact leading layout is a first-line purpose comment for the generated
  file index, then the build constraint, then a blank line before `package`: the
  main benchmark uses `//go:build adoption_benchmark`, the Linux helper uses
  `//go:build adoption_benchmark && linux`, and the fallback helper uses
  `//go:build adoption_benchmark && !linux` and reports cold mode unsupported.
  Despite those constraints, all three files remain authoritative index inputs.
  After intent-to-add, `make lint-fix` from `/home/user/src/caic` (canonical
  generator `scripts/update_agents_file_index.py`) may add exactly their three
  first-line purpose-comment entries to `backend/AGENTS.md`. The Linux helper
  uses the existing `golang.org/x/sys/unix` dependency. `go.mod` and `go.sum` are additionally
  approved only if `go mod tidy` changes direct-dependency classification or
  checksums because of that import; the executor must declare and review their
  exact diff before continuing. The `b.TempDir` lifecycle removes the fixture.
  No fixture, benchmark result, profile, cache, or tool binary lives in the
  repository.
- **Change budget:** at most 5 production files within the write scope; exactly
  the six named test files in the write scope; generated `backend/AGENTS.md`; and
  conditional `go.mod`/`go.sum` changes only under the rule above. No unnamed test
  budget, broad manager/runtime refactor, parser change, or wire change.
- **Boundary:** preserve same-inode authority, mixed/later-header rejection,
  bounded streaming behavior, taskmeta derivation, fail-closed missing/changed-
  file semantics, and byte-identical v1 output; do not weaken validation, add a
  second authority, change log format, route through `LogRecordParser`, or
  measure real containers, networks, or LLMs.
- **Resource and evidence protocol:** `CAIC_ADOPTION_BENCH_BYTES` has a hard
  4 GiB maximum and the canonical gate is exactly 1 GiB. Before a canonical run,
  require at least 3 GiB free on the fixture filesystem and at least 6 GiB
  available RAM for full-message restoration; otherwise block and escalate
  rather than shrinking the gate. Reuse the one fixture and never create
  concurrent copies. Baseline and current run on the same host with the same Go
  runtime, power, and load controls and the verified benchmark-source hash. The
  coordinator-provided external directory holds hashed canonical before/after
  evidence, the source manifest, resource/host metadata, `/proc/self/io` samples,
  and comparison output through all dependent reviews. Duplicate scratch output
  is removed after each review; after the phase review, raw transcripts are
  reduced to their SHA-256 manifest and normalized evidence. All retained
  external evidence is removed at rollout completion.
- **Reproducible comparison:** preflight must prove network access or a warmed
  module cache for `golang.org/x/perf`, require an enabled Go checksum database,
  and successfully obtain checksum-bearing module metadata before measurement.
  The only comparison command is `go run
  golang.org/x/perf/cmd/benchstat@v0.0.0-20260709024250-82a0b07e230d`; `@latest`
  is forbidden. Benchstat/timing conclusions remain advisory.
- **Decision checkpoints:** stop before implementation if pass reuse would outlive
  its inode/size/mtime identity; stop and split/escalate for resource-preflight
  failure, a changed benchmark hash, unsupported required instrumentation, or an
  optimization that needs a second authority. If safe material deterministic
  reduction cannot be achieved inside the task loader/adoption boundary, stop
  for a split and user decision; benchmark-only work cannot pass.
- **Validation intent:** hard evidence reports deterministic logical read
  amplification and complete physical-reader pass counts for many-small-line and
  large-tool-output fixtures; logical-byte totals are retained as the ratio
  inputs. Every corrupt, mixed, replaced, and truncated case still fails closed;
  plain and compressed memory stays bounded. The combined exact live-adoption
  sequence must materially reduce complete passes/logical read amplification;
  material means eliminating at least one complete fixture-length pass while all
  authority tests remain correct. Advisory measurements have no fixed threshold.
- **Validation commands:** cwd `/home/user/src/caic`: preflight the recorded
  external artifact directory, 3 GiB disk, 6 GiB available RAM, Go checksum
  database, and pinned module with `go mod download -json
  golang.org/x/perf@v0.0.0-20260709024250-82a0b07e230d`, requiring non-empty
  `Sum` and `GoModSum`. Run `make lint-fix`, verify only the three benchmark/helper
  sources and their three generated index entries differ, verify `git diff
  --cached --exit-code`, and write/verify their SHA-256 manifest. With
  `CAIC_ADOPTION_BENCH_BYTES=$((1024*1024*1024))`, run `go test
  -tags adoption_benchmark ./backend/internal/task -run '^$' -bench
  '^BenchmarkTaskAdoption' -benchmem -benchtime=1x -count=3` to hashed before
  output; only then implement and run the byte-identical command to hashed after
  output. Compare with `go run
  golang.org/x/perf/cmd/benchstat@v0.0.0-20260709024250-82a0b07e230d BEFORE AFTER`.
  Every benchmark invocation, including before/after and carry-forward runs, must
  use `-tags adoption_benchmark`. Compile without executing test binaries for
  Linux and representative non-Linux with
  `CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go test -tags
  adoption_benchmark -c ./backend/internal/task -o
  "$ARTIFACT_DIR/task-linux.test"` and
  `CGO_ENABLED=0 GOOS=darwin GOARCH=$(go env GOARCH) go test -tags
  adoption_benchmark -c ./backend/internal/task -o
  "$ARTIFACT_DIR/task-darwin.test"`, then remove both binaries. Run focused
  deterministic pass-count/read-amplification and authority tests from the three
  named existing test files, `go test
  ./backend/internal/agent/... ./backend/internal/task/...`, `go mod tidy` if
  needed, `make lint-fix`, replay the benchmark after lint, `make lint-docs`,
  `git diff --check`, `git diff --cached --exit-code`, `git status --short`, and
  `git ls-files --others --exclude-standard`.
- **Review:** a fresh performance/correctness reviewer receives the integrated
  target, pre-integration SHA, exact call sequence, fixture mix, source/hash
  manifest, resource/host controls, hashed 1 GiB warm/cold before/after evidence,
  pinned comparison, deterministic natural-boundary pass/byte counts,
  `/proc/self/io` corroboration, exact artifact delta, cross-platform compile
  evidence, cleanup state, and all authority/corruption results; require `PASS`.
- **Exit gate:** benchmark implementation alone cannot pass. The exact combined
  live-adoption sequence eliminates at least one complete fixture-length logical
  pass, thereby showing a material deterministic read-amplification reduction;
  every authority/corruption gate passes, and benchmark/helper hashes match
  before/after. Wall time, throughput, allocations, physical-read deltas, and
  benchstat comparisons are advisory same-host evidence with no fixed threshold.
  Exact generated delta is three index entries; conditional module-file diffs
  obey the declared rule; no staged content or repository artifact survives. If the
  reduction is not safely achievable, stop for split/user decision.
- **Handoff:** report exact operation/caller sequence, fixture composition/size,
  platform and cold-cache support, source/evidence hashes and external artifact
  location, resource/host controls, before/after commands and pinned comparison,
  per-operation logical pass/byte counts plus advisory timing/allocations/physical
  reads, old/new call graph, authority proof, exact files/generated/module diff,
  intent-to-add/index state, cleanup, validation, unresolved host noise, and risks.

**Carry-forward performance contract:** every later phase that touches a measured
reader, adoption, or reopen path (`persistent-read-paths`,
`pure-harness-parsers`, `timestamp-cache-semantics`, `versioned-log-sink`,
`agent-direct-write-migration`, `v2-producer-cutover`, and
`runtime-adoption-smoke` where its environment supports the local gate) reruns
the focused deterministic counting-reader tests. Only the accepted combined
`LoadLogsForTaskIDs` through validated reopen deterministic logical read-
amplification and complete-pass results are a hard non-regression gate. Wall
time, throughput, allocations, physical-read deltas, and benchstat comparisons
remain advisory same-host evidence with no fixed threshold. Preflight for each
such phase records the coordinator-supplied absolute external artifact
directory, accepted three-source benchmark SHA-256 manifest, and accepted
evidence hashes. The benchmark sources may not be regenerated or changed. Every
carry-forward benchmark invocation uses `-tags adoption_benchmark`. A same-host
control/current run uses identical Go/power/load settings, the canonical 1 GiB
fixture, and pinned `go run
golang.org/x/perf/cmd/benchstat@v0.0.0-20260709024250-82a0b07e230d` when host
resources permit; resource failure blocks/escalates rather than shrinking the
gate. Each result and normalized comparison is hashed in that external
directory, transient raw transcripts are removed after review, and retained
hash manifests/evidence are removed only at rollout completion. No performance
artifact enters the repository.

### Phase 2: Correct parsed-message producer-time metadata

- **Stable ID:** `parsed-message-metadata`
- **Status:** accepted/completed dependency-safely out of order ahead of Phase 1;
  retained only until Phase 1 is integrated and removed; not eligible for
  redispatch
- **Responsible:** one phase-executor subagent (completed)
- **Depends on:** none
- **May run with:** none; this phase is complete
- **Base state:** completed from the accepted unshipped shared-parser state after
  fresh ephemeral capture of `LOCAL_BASE`, `SHIPPED_BASE`, configured upstream,
  status, and exact parser/type/test statistics. The accepted result remains
  unshipped and has no production consumer
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push
- **Write scope:** exactly
  `backend/internal/agent/{agent.go,agent_test.go,types.go}`; no production reader,
  harness parser, timing tracker, replay, task, relay, or API file
- **Data authority:** a valid v2 `agent.ts` is immutable producer metadata for
  every semantic message parsed from that envelope; zero producer time is the
  sole absence representation
- **Purpose:** establish explicit outer parsed-message metadata before any
  production read path adopts the shared parser; the accepted result gates both
  persistent and live read-path adoption
- **Deliverables:** accepted outer `ParsedMessage` with exactly `Message Message`
  and `ProducerTime time.Time`; `LogRecordParser.ParseRecord` returns parsed
  values while native harness parser callbacks continue returning `[]Message`;
  every semantic result from a valid v2 `agent` envelope carries the same
  producer time; every v1 result and every control without envelope producer
  time carries zero time; ordered state application, nil rejection, context-
  window propagation, and pending-action deduplication are preserved;
  `TimestampMessage` and parser-side timing-relevance filtering are absent
- **Generated artifacts:** none
- **Change budget:** exactly the three named files; no new file, dependency,
  fixture, or public API DTO. Local-only churn within that exact accepted scope
  was not a compatibility constraint
- **Boundary:** `ParsedMessage` has no `Type()` method, does not implement
  `Message`, and has no presence boolean; consumers must explicitly unwrap
  `.Message`. No compatibility adapter, persistent/live reader integration,
  `ToolTimingTracker` change, native parser signature change, physical framing
  change, or semantic timestamp event is included
- **Decision checkpoints:** stop if a native harness parser signature must
  change, metadata cannot remain outside `Message`, parser state would become
  non-transactional, or any production consumer must be migrated in this phase
- **Validation intent:** accepted evidence proves one-to-many native parsing gives
  every result the exact envelope time, including text and other timing-
  irrelevant messages; v1 native and v1/v2 controls use zero time; empty native
  results stay empty; concrete Go type switches work after `.Message` unwrapping;
  value and pointer forms of `ParsedMessage` fail a runtime `Message` interface
  assertion; `TimestampMessage` and the parser timing-relevance classifier are
  absent; all accepted parser corruption, state, deduplication, and nil-safety
  tests are green
- **Accepted validation commands (record only; do not redispatch):** cwd
  `/home/user/src/caic`: `go test
  ./backend/internal/agent -run '^TestLogRecordParser$'`; `go test
  ./backend/internal/agent/...`; `go test -race ./backend/internal/agent`;
  `make lint-fix`; `git diff --check`; `git diff --cached --exit-code`; `git
  status --short`; `git ls-files --others --exclude-standard`; repository audits
  for removed marker symbols, forbidden presence fields, `ParsedMessage` method
  sets, `ParseRecord` result consumers, and native parser callback signatures.
  Separately, record focused parser evidence with `git diff --stat
  "$SHIPPED_BASE...$PHASE_FINAL" -- backend/internal/agent/agent.go
  backend/internal/agent/agent_test.go backend/internal/agent/types.go` and `git
  diff "$SHIPPED_BASE...$PHASE_FINAL" -- backend/internal/agent/agent.go
  backend/internal/agent/agent_test.go backend/internal/agent/types.go` for the
  final combined three-file parser result, plus `git diff --stat
  "$LOCAL_BASE...$PHASE_FINAL" -- backend/internal/agent/agent.go
  backend/internal/agent/agent_test.go backend/internal/agent/types.go` and `git
  diff "$LOCAL_BASE...$PHASE_FINAL" -- backend/internal/agent/agent.go
  backend/internal/agent/agent_test.go backend/internal/agent/types.go` for the
  focused phase delta
- **Review:** an independent Go API/type-system reviewer checked both the
  combined parser result against frozen `SHIPPED_BASE` and the focused delta
  against frozen `LOCAL_BASE`, including explicit-unwrapping pressure, metadata
  propagation, the unchanged native parser contract, and focused/full
  validation; the review passed with no findings
- **Exit gate:** passed for the exact three-file scope: each parsed value contains
  one non-nil concrete semantic message; all v2 envelope results carry exact
  producer time; all v1/control results carry zero time; `ParsedMessage` cannot
  satisfy `Message`; `TimestampMessage`, parser timing filtering, and compatibility
  adapters are absent; no production read path consumes `LogRecordParser`; both
  frozen shared-base combined and local-phase diffs were reviewed; validation
  and empty staged/untracked checks passed
- **Handoff:** accepted evidence records old/new method signatures, metadata
  cases, interface/method-set evidence, removed symbols/helpers, exact combined
  `SHIPPED_BASE...PHASE_FINAL` and focused `LOCAL_BASE...PHASE_FINAL` diffstats,
  commands, filesystem status, and residual risks

### Phase 3: Build the v2-only relay

- **Stable ID:** `relay-v2`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** `persistent-read-paths` in an isolated worktree
- **Base state:** no stable phase dependency; the physical authority foundation
  is assumed shipped at the resolved shared-base role, the v2 relay is not yet
  implemented, and the accepted local parser and rollout-plan work remain
  unshipped. The worktree matches the declared clean/accepted-delta condition.
  Immediately before dispatch, resolve actual HEAD, `origin/main`, and any
  configured upstream into the ephemeral manifest and capture status plus byte/
  hash baselines for `relay.py` and `test_relay.py`
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; v1 files are
  read-only. Before generated-file replay, the coordinator must obtain the
  user-owned intent-to-add checkpoint for exactly `relay_v2.py` and
  `test_relay_v2.py`; the executor verifies no staged content with `git diff
  --cached --exit-code`, and the user owns later index cleanup/staging/history
- **Write scope:** new `backend/internal/agent/relay/relay_v2.py`, new
  `backend/internal/agent/relay/test_relay_v2.py`, latent
  `backend/internal/agent/relay/embed.go` wiring plus its first-line purpose
  comment, and generated `backend/AGENTS.md` file-index output only
- **Data authority:** script version is derived from backend selection; relay
  timestamps are immutable producer observations, not version authority
- **Purpose:** create a single-purpose v2 physical stream without activating it
- **Deliverables:** copied v1 infrastructure with v2 stdout/logged-stdin framing;
  `output.jsonl` as a client-stream superset; byte identity for every record sent
  to both destinations; no live stdin echo; unprefixed controls; newline carry
  and EOF flush; final-encoded-record cap; invalid JSON/UTF-8 and oversized
  diagnostics; no v1-emitting branch; update `embed.go`'s first-line purpose
  comment from one-script wording to describe the two version-specific relay
  embeds
- **Generated artifacts:** `backend/AGENTS.md` is generated from all three
  affected purpose inputs: the first-line purpose comments of the two new Python
  files (after any shebang) add `relay_v2.py` and `test_relay_v2.py` entries, and
  `embed.go`'s updated first line refreshes its existing entry. Run
  `make lint-fix` (canonical underlying command:
  `scripts/update_agents_file_index.py`) from `/home/user/src/caic`. The
  generator reads `git ls-files`, so it must run only after the user-owned
  intent-to-add checkpoint above; replay mode is the same command from the same
  cwd, with no hand edit of the index output
- **Change budget:** exact allowed delta is the two new Python files, one latent
  `embed.go` edit, and generated `backend/AGENTS.md`; zero-byte delta to
  `relay.py` and `test_relay.py`; copied infrastructure churn is expected only
  in `relay_v2.py`
- **Boundary:** no backend selection, no v2 task logs, no shared Python module,
  and no edit to frozen v1 behavior
- **Decision checkpoints:** stop if the final-record limit cannot fit existing
  scanner bounds, if a record sent to both client and output must differ between
  destinations, or if sharing v1 code is proposed
- **Validation intent:** chunk boundaries, blank lines, partial EOF, logged and
  unlogged stdin, no live stdin echo, attach replay from a physical offset,
  stdout, concurrent controls, valid scalar/object JSON, invalid UTF-8,
  oversized input, per-record client/file byte identity, output-superset framing,
  and absence of v1 type emissions
- **Validation commands:** cwd `/home/user/src/caic`: after the coordinator
  records the user-owned `git add -N` checkpoint for the two approved new files,
  run `git status --short`; `git diff --cached --exit-code`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; `python3
  backend/internal/agent/relay/test_relay.py`; `ruff check
  backend/internal/agent/relay`; `make lint-fix`; `git diff --check`; `git
  diff --cached --exit-code`; `git status --short`; `git ls-files --others
  --exclude-standard`; compare v1 file hashes and verify the generated index
- **Review:** fresh Python/concurrency reviewer inspects the two new scripts,
  latent embed wiring, `embed.go`'s two-version first-line purpose comment, all
  three purpose inputs reflected in generated `backend/AGENTS.md`, intent-to-add
  status, empty cached-content diff, v1 hashes, final-size math, and integrated
  gate; require `PASS`
- **Exit gate:** direct tests cover all named failure paths and prove the
  output-superset/client-subset contract; exact delta is the two new Python files,
  latent `embed.go` edit including its two-version purpose comment, and generated
  `backend/AGENTS.md` with both new entries and the refreshed embed entry;
  intent-to-add entries are explicitly accounted for with no staged content; v1
  hashes match the baseline; and no production path references the v2 embed
- **Handoff:** report test cases, size-bound proof, v1 hashes, exact four-file
  delta/diffstat, the three purpose-input changes and generated index entries,
  generated-index replay command/result, intent-to-add entries, `git diff
  --cached --exit-code` result, user-owned index cleanup status, commands, and
  remaining relay risks

### Phase 4: Route persistent readers through version dispatch

- **Stable ID:** `persistent-read-paths`
- **Responsible:** one phase-executor subagent
- **Depends on:** `adoption-scan-performance`, `parsed-message-metadata`
- **May run with:** `relay-v2`
- **Base state:** accepted stable phases `adoption-scan-performance` and
  `parsed-message-metadata` are integrated, all rollout changes remain unshipped,
  and the worktree is clean. Immediately before dispatch, resolve actual HEAD,
  `origin/main`, and any configured
  upstream into the ephemeral manifest and capture accepted adoption benchmark/
  call-graph evidence, effective pass count, and loader fixture statistics
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; no fixture
  recording or regeneration without coordinator approval
- **Write scope:** `backend/internal/task` loaders/tail/session metadata,
  `backend/internal/agent/export.go`, replay input adapters, and adjacent tests
- **Data authority:** reuse the accepted common physical authority scanner and
  same-file identity result; full reads derive pending-action identities while
  version-dispatching that same stream, and tail reads receive authority already
  validated from the same physical file; the identity set is scan output, never
  a persisted authority
- **Purpose:** eliminate per-line format guesses from disk/replay/export paths
  without creating another header/file-identity implementation or reintroducing
  the redundant adoption scans removed by `adoption-scan-performance`
- **Deliverables:** plain/compressed/full/tail/session/export record semantics use
  the shared versioned parser on top of the accepted authority scanner and
  accepted scan-reuse boundaries; parsed-stream consumers explicitly unwrap
  `.Message` and preserve `ProducerTime` until the downstream conversion boundary;
  no metadata wrapper is inserted into semantic message collections; matching
  later-header, unknown-version,
  taskmeta identity, same-inode, corruption, and adoption pass-count behavior
  stays unchanged; full/reopen scan results expose already-persisted
  pending-action identities for later sink construction; offset-tail APIs cannot
  invent authority or the identity set from a tail fragment; the accepted
  realistic adoption benchmark remains runnable against v1 and synthetic v2
- **Generated artifacts:** no committed artifacts; taskmeta/replay cache tests use
  temporary directories and existing golden recordings are not rewritten
- **Change budget:** at most 6 production files and 5 tests because physical
  authority, taskmeta identity, and same-inode validation are already complete;
  fixture additions are limited to one minimal v1 and one minimal v2 file
- **Boundary:** live relay reads and writers stay unchanged; harness parser caic
  cases remain until all read paths migrate
- **Decision checkpoints:** stop for compatibility weakening, skipped corruption,
  fixture rewrites, any second physical authority scanner, any bypass of the
  accepted same-file identity checks, invalidation of identity-bound scan reuse,
  or any extra effective full-file pass
- **Validation intent:** equivalent v1/v2 semantic full and tail results, zero v1
  producer times, exact v2 envelope producer times on every parsed message, and
  explicit unwrapping with no timestamp event; compressed parity;
  taskmeta hit/miss/stale/corrupt integration; export parity; exact unknown-
  version, changed-segment, bare-v2, and prefixed-v2 errors; v1 and synthetic-v2
  full scans derive the same pending-action identity from a persisted native
  source; no fallback when a tail omits the first header. Under the carry-forward
  performance contract, counting-reader correctness tests hard-gate the accepted
  combined live-adoption logical read amplification and complete-pass count for
  v1 and synthetic v2; same-host wall time, throughput, allocations, physical-
  read deltas, and benchstat comparisons remain advisory with no fixed threshold.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run the focused deterministic
  pass-count/read-amplification tests, `go test ./backend/internal/agent/...
  ./backend/internal/task/... ./backend/internal/eventreplay/...`, and the
  canonical same-host 1 GiB control/current benchmark with pinned benchstat under
  the carry-forward contract; hash external results and remove transient raw
  transcripts after review; `make lint-fix`; `git diff --check`; `git status
  --short`; `git diff --cached --name-only`
- **Review:** fresh reviewer checks all persistent entry points against the
  integrated SHA and fixture delta; require `PASS`
- **Exit gate:** an entry-point inventory proves every persistent path reuses the
  accepted physical authority scanner and dispatches records by its exact
  `LogVersion`; reopen consumers receive the authoritative scan's pending-action
  identity set, no duplicate scanner or identity index is introduced, the hard
  deterministic combined-adoption logical read amplification and complete-pass
  count do not regress, advisory same-host measurements are reported, existing
  same-inode/taskmeta/mixed-header tests stay green, and the exact allowed fixture
  delta is two files
- **Handoff:** list migrated entry points, pending-action scan-output API and
  identity cases, fixture hashes, behavior/error evidence, filesystem delta,
  validation, and risks

### Phase 5: Route live and startup relay reads

- **Stable ID:** `live-relay-read-path`
- **Responsible:** one phase-executor subagent
- **Depends on:** `relay-v2`, `parsed-message-metadata`
- **May run with:** none due to overlap across agent launch/read paths
- **Base state:** accepted stable phases `parsed-message-metadata` and `relay-v2`
  are integrated, all rollout changes remain unshipped, and the worktree is
  clean. Immediately before dispatch, resolve
  actual HEAD, `origin/main`, and any configured upstream into the ephemeral
  manifest and capture existing live-read/handshake test baselines
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push
- **Write scope:** shared relay record reader; `DefaultReadMessages`; relay
  tail/attach; Pi startup loops; Codex/OpenCode handshake/custom launch paths;
  adjacent tests
- **Data authority:** caller supplies validated file `LogVersion`; physical relay
  offsets count encoded bytes; decoded payload never becomes an authority
- **Purpose:** make every live/startup consumer understand v1 and latent v2 while
  still running v1 producers
- **Deliverables:** relay reader with an explicit per-consumer persistence policy;
  normal-live log-once behavior; semantic consumers receive parsed values and
  explicitly unwrap `.Message`; every v2 envelope result retains its
  `ProducerTime`; v2 native-payload unwrap for handshakes; Pi startup persistence;
  unpersisted Codex/OpenCode handshakes; control routing; physical offsets;
  attach-overlap deduplication; common script deployment/selection seam used by
  custom launchers; v1 byte preservation
- **Generated artifacts:** none
- **Change budget:** at most 10 production files and 8 tests; no protocol DTO or
  handshake sequence changes
- **Boundary:** selected version remains v1 in production; parsed metadata must
  not implement or be inserted as `Message`; no header bump, v2 deployment, sink
  migration, or harness-parser cleanup
- **Decision checkpoints:** stop if a handshake must receive a caic control as
  native data, if a consumer cannot state its persistence policy, if logging
  twice appears necessary, or if offset semantics would change for v1
- **Validation intent:** split reads, interleaved controls, handshake buffering,
  non-zero exits, attach physical offsets, relay-stdin overlap deduplication, no
  live stdin double write, v1 exact logged bytes and zero producer time, v2
  physical-byte counts and exact per-result producer time without a semantic
  timestamp event, Pi startup persistence, Codex/OpenCode handshake
  non-persistence, and all three
  startup paths succeeding with synthetic streams
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/... ./backend/internal/task/...`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; `make lint-fix`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`
- **Review:** independent concurrency/protocol reviewer inspects the integrated
  reader, all custom launch paths, offsets, and log-once evidence; require `PASS`
- **Exit gate:** entry-point tests prove v1 byte identity, latent v2 decoding,
  current per-harness persistence policy, and no duplicate stdin across live and
  attach paths; no production task selects v2
- **Handoff:** report migrated call graph, offset/log-once evidence, protocol
  tests, files, commands, and residual risks

### Phase 6: Make harness parsers native-only

- **Stable ID:** `pure-harness-parsers`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`, `live-relay-read-path`
- **May run with:** `timestamp-cache-semantics` in an isolated worktree
- **Base state:** accepted stable phases `persistent-read-paths` and
  `live-relay-read-path` are integrated, all rollout changes remain unshipped,
  and the worktree is clean. Immediately before dispatch, resolve actual HEAD,
  `origin/main`, and any configured upstream into the ephemeral manifest and
  baseline each harness parser/test file
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; golden recordings
  are read-only
- **Write scope:** Claude, Codex, Pi, and OpenCode native parser files/tests plus
  stale parser documentation
- **Data authority:** native parser state derives only from native payloads;
  shared parser owns caic controls and model-context snapshot
- **Purpose:** remove caic vocabulary and provenance guessing from harness code
- **Deliverables:** remove all caic control branches/comments/tests from native
  parsers; retain native Codex duration and harness-specific behavior; prove
  native callbacks still return `[]Message` and shared entry points preserve
  parsed metadata plus explicitly unwrapped semantic output
- **Generated artifacts:** none
- **Change budget:** only the four parser families and adjacent tests/docs;
  expected net deletion; no golden fixture rewrite
- **Boundary:** no writer, relay, header, cache-version, or API change
- **Decision checkpoints:** stop if any production caller still sends a control to
  a native parser, or if removing model-info loses Pi replay semantics
- **Validation intent:** native fixtures parse unchanged through shared entry;
  direct native parsers reject/ignore caic records consistently; all control
  coverage lives in shared parser tests; regression/audit coverage proves the
  local shared parser's pending-action deduplication remains at the shared entry
  point; Codex native duration remains. Because the shared entry is
  on the measured message path, focused counting-reader tests hard-gate the
  accepted combined live-adoption logical read amplification and complete-pass
  result; same-host measurements remain advisory under the carry-forward contract.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run the focused deterministic
  pass-count/read-amplification tests, `go test ./backend/internal/agent/...
  ./backend/internal/task/...`, and the canonical same-host 1 GiB control/current
  benchmark with pinned benchstat under the carry-forward contract; hash external
  results and remove transient raw transcripts after review; `make lint-fix`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`
- **Review:** fresh harness-focused reviewer checks for remaining `caic_` parser
  branches and semantic regressions on integrated target; require `PASS`
- **Exit gate:** repository audit finds no caic routing in native parsers, all
  four golden suites pass without recording changes, and deterministic counting-
  reader evidence proves no regression in the accepted combined-adoption logical
  read amplification and complete-pass count; advisory evidence follows the
  external artifact hash and cleanup protocol
- **Handoff:** provide audit command/result, per-harness tests, deletions/delta,
  validation, and unresolved risks

### Phase 7: Make producer-time metadata and replay caches version-correct

- **Stable ID:** `timestamp-cache-semantics`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`, `live-relay-read-path`
- **May run with:** `pure-harness-parsers`
- **Base state:** accepted stable phases `persistent-read-paths` and
  `live-relay-read-path` are integrated, all rollout changes remain unshipped,
  and the worktree is clean. Immediately before dispatch, resolve actual HEAD,
  `origin/main`, and any configured upstream into the ephemeral manifest and
  capture cache version and replay-
  fixture baselines
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; no generated
  replay recording without approval
- **Write scope:** parsed producer-time consumption, `ToolTimingTracker`, replay
  filters, `backend/internal/eventreplay`, server replay tests, minimal fixtures
- **Data authority:** `ParsedMessage.ProducerTime` copied from `agent.ts` is
  immutable producer time; conversion clock is derived; replay sidecars remain
  rebuildable caches
- **Purpose:** consume exact v2 producer-time metadata without changing v1 files
  or exposing a timestamp event
- **Deliverables:** explicit `.Message` unwrapping; metadata-transparent
  conversion and compaction; stable tool-duration replay; fallback for zero
  producer time; timing-relevance gating owned only by `ToolTimingTracker`;
  `CacheVersion` bump; pure-v1/v2 parity fixtures
- **Generated artifacts:** no committed generated sidecars; tests generate in
  temporary directories only
- **Change budget:** at most 8 production files and 7 tests; two minimal raw-log
  fixtures maximum; one intentional cache constant change
- **Boundary:** no parser-side timing relevance, metadata-to-message adapter, API
  DTO field, v1 timestamp retrofit, relay change, or producer cutover
- **Decision checkpoints:** stop if producer time must be persisted twice, becomes
  a semantic/user-visible event, is lost before `ToolTimingTracker`, or changes
  native harness duration precedence
- **Validation intent:** exact v2 tool spans; first-producer-time fallback; native
  duration precedence; zero-time v1 safe fallback; tracker ignores metadata on
  timing-irrelevant messages while semantic consumers still receive those
  messages; producer metadata is transparent to deltas, control searches,
  exports, and the measured full-message restore path; stale
  cache rejection and regeneration. Focused counting-reader tests hard-gate the
  accepted combined live-adoption logical read amplification and complete-pass
  result; same-host measurements remain advisory under the carry-forward contract.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run focused deterministic
  pass-count/read-amplification tests, `go test
  ./backend/internal/server/api/v1conv/... ./backend/internal/eventreplay/...
  ./backend/internal/server/... ./backend/internal/task/...`, and the canonical
  same-host 1 GiB control/current benchmark with pinned benchstat under the
  carry-forward contract; hash external results and remove transient raw
  transcripts after review; `make lint-fix`; `git diff --check`; `git status
  --short`; `git diff --cached --name-only`
- **Review:** fresh timing/replay reviewer checks integrated semantics, cache bump,
  and fixture delta; require `PASS`
- **Exit gate:** v1/v2 fixture semantic events are equivalent; v1 parsed values
  carry zero time, v2 parsed values carry exact producer metadata, and only
  downstream durations may differ; cache rebuild tests pass; deterministic
  combined-adoption
  logical read amplification and complete-pass count do not regress; advisory
  performance evidence follows the external artifact hash/cleanup protocol; no
  sidecar or benchmark artifact is committed
- **Handoff:** report timing cases, old/new cache version, fixture hashes,
  filesystem delta, commands, and risks

### Phase 8: Introduce the enforcing versioned log sink

- **Stable ID:** `versioned-log-sink`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`
- **May run with:** none
- **Base state:** accepted stable phase `persistent-read-paths` is integrated,
  all rollout changes remain unshipped, and the worktree is clean. Immediately
  before dispatch, resolve actual HEAD,
  `origin/main`, and any configured upstream into the ephemeral manifest and
  capture the accepted same-inode append constructor, direct `LogW.Write`
  inventory, authoritative-scan outputs, and adoption benchmark/pass-count
  baseline
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push
- **Write scope:** versioned sink API/implementation in `backend/internal/agent`
  and `backend/internal/task`, with focused tests; callers remain v1-compatible
- **Data authority:** extend the accepted same-inode v1 append constructor rather
  than replacing it; the sink is constructed only from its validated header
  authority and, on reopen, receives the authoritative scan's derived set of
  already-persisted pending-action identities; it owns no second version value or
  persisted duplicate identity index
- **Purpose:** create one append policy before migrating many direct writers and
  safely lift the temporary valid-v2 append rejection only for typed sink writes
- **Deliverables:** typed native/control append operations layered on the accepted
  same-inode constructor; v1 byte-preserving encoding; latent v2 envelope/control
  encoding; mixed-token rejection; valid-v2 append enabled only through typed
  operations; constructor/reopen plumbing that receives the authoritative scan's
  pending-action identity set; snapshot guard keyed by
  `(kind, request ID, tool-use ID)` across process restarts; serialized writes;
  explicit close/error behavior
- **Generated artifacts:** none
- **Change budget:** at most 7 production files and 5 tests; do not migrate more
  than the minimum constructor/reopen plumbing
- **Boundary:** all production callers still select v1; the existing untyped raw
  path continues rejecting valid v2; no removal of raw writer paths yet, header
  bump, relay selection, parser cleanup, or redundant full scan on reopen
- **Decision checkpoints:** stop if the accepted same-inode constructor would be
  bypassed, if valid-v2 append becomes possible through an untyped writer, if the
  sink stores an independent version or pending-action identity index, if a
  reopen caller cannot supply the authoritative scan result, or if v1 bytes
  differ
- **Validation intent:** byte-for-byte v1 native/control writes; exact v2 envelope
  and unprefixed controls through typed operations; valid-v2 rejection remains on
  every untyped append path; v1 and synthetic-v2 reopen cases where a native
  source predates reopen and the sink refuses an equivalent top-level pending-
  action snapshot from the supplied authoritative identity set; unsupported/
  mixed refusal before write; same-inode and concurrent append integrity;
  counting-reader tests hard-gate the accepted combined live-adoption and reopen
  logical read amplification and complete-pass counts; errors surfaced. Same-host
  measurements remain advisory under the carry-forward contract.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run focused deterministic
  pass-count/read-amplification and reopen tests, `go test
  ./backend/internal/agent/... ./backend/internal/task/...`, and the canonical
  same-host 1 GiB control/current benchmark with pinned benchstat under the
  carry-forward contract; hash external results and remove transient raw
  transcripts after review; `make lint-fix`; `git diff --check`; `git status
  --short`; `git diff --cached --name-only`
- **Review:** independent API/authority reviewer inspects the integrated sink and
  writer escape hatches; require `PASS`
- **Exit gate:** sink tests prove accepted same-inode behavior, v1 byte identity,
  typed-only v2 append/enforcement, continued untyped-v2 rejection, and restart-
  safe refusal of pending-action duplicates based on authoritative scan output,
  while all produced repository logs remain v1, deterministic combined-adoption
  and reopen logical read amplification and complete-pass counts do not regress,
  and advisory evidence follows the external artifact hash/cleanup protocol
- **Handoff:** include direct-write inventory, constructor/reopen API contract,
  authoritative identity-set evidence, v1/v2 reopen cases, byte fixtures,
  commands, exact delta, and remaining migration sites

### Phase 9: Migrate every direct log writer

- **Stable ID:** `agent-direct-write-migration`
- **Responsible:** one phase-executor subagent
- **Depends on:** `versioned-log-sink`, `live-relay-read-path`,
  `pure-harness-parsers`
- **May run with:** none; it crosses all agent backends and task writers
- **Base state:** accepted stable phases `versioned-log-sink`,
  `live-relay-read-path`, and `pure-harness-parsers` are integrated, all rollout
  changes remain unshipped, and the worktree is clean. Immediately before
  dispatch, resolve actual HEAD, `origin/main`, and
  any configured upstream into the ephemeral manifest and capture the complete
  physical reader/append inventory, direct-write inventory, and v1 trace hashes
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; no golden
  recording regeneration without approval
- **Write scope:** all `Options.LogW`/task-log callers, backend handshakes,
  prompt/compact/SendRaw, session/model-info/context markers, provisioning,
  trailers, `Task.WriteToLog`, and adjacent tests
- **Data authority:** callers receive one sink bound to file authority and, on
  reopen, the authoritative scan's pending-action identity set; no caller chooses
  vocabulary, manually wraps native data, or rebuilds that set from process-local
  memory
- **Purpose:** eliminate bypasses before v2 can be enabled
- **Deliverables:** migrate the full inventory, including the accepted
  `Task.WriteToLog` path-based fallback and every production physical entry
  point; v1 bytes stay identical; latent v2 direct native records use `agent`;
  v2 controls use approved tokens; context reset is top-level
  `context_cleared`; live prompt/compact/`SendRaw` persistence is sink-exclusive;
  Pi startup persistence is retained while Codex/OpenCode handshakes remain
  unpersisted; pending-action snapshots obey the exactly-once rule; no raw
  task-log writer escapes
- **Generated artifacts:** none; existing traces remain unchanged
- **Change budget:** cross-cutting but limited to inventoried write sites and
  tests; no unrelated backend refactor; every added file must map to an inventory
  row
- **Boundary:** new headers and selected relays remain v1; no cutover or API
  change; relay v1 files remain frozen
- **Decision checkpoints:** stop for any writer without clear native/control
  provenance, any v1 byte change, or any need to expose sink internals publicly
- **Validation intent:** per-writer v1 golden bytes and v2 latent encoding;
  `Task.WriteToLog` with and without a live handle uses the sink and retains
  same-inode authority; persisted Pi startup traffic logs once; Codex/OpenCode
  handshake requests and responses do not enter task logs; prompt/compact/
  `SendRaw` framing has no relay duplicate; native/top-level pending actions
  deduplicate by semantic identity; model/session state and context/trailer/
  provisioning behavior remain correct; an inventory audit finds no physical
  reader or append bypass; focused counting-reader tests hard-gate the accepted
  combined live-adoption/reopen logical read amplification and complete-pass
  counts. Same-host measurements remain advisory under the carry-forward contract.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run focused deterministic
  pass-count/read-amplification and reopen tests, `go test
  ./backend/internal/agent/... ./backend/internal/task/...`, `python3
  backend/internal/agent/relay/test_relay.py`, and the canonical same-host 1 GiB
  control/current benchmark with pinned benchstat under the carry-forward
  contract; hash external results and remove transient raw transcripts after
  review; `make lint-fix`; `git diff --check`; `git status --short`; `git diff
  --cached --name-only`; audit direct `.Write` calls against an allowlist
- **Review:** fresh cross-backend reviewer receives inventory before/after,
  integrated SHA, v1 trace hashes, and gate; require `PASS`
- **Exit gate:** every task-log write, including both `Task.WriteToLog` branches,
  flows through the versioned sink or version-aware relay reader under an
  explicit persistence policy; a full production physical entry-point inventory
  has no authority/parser/append bypass; v1 trace hashes match; stdin and pending-
  action semantics occur exactly once; deterministic combined-adoption/reopen
  logical read amplification and complete-pass counts do not regress; advisory
  evidence follows the external artifact hash/cleanup protocol; production still
  emits v1
- **Handoff:** report inventory closure, byte hashes, backend-specific tests,
  scoped exceptions, commands, and risks

### Phase 10: Cut new producers over to pure v2

- **Stable ID:** `v2-producer-cutover`
- **Responsible:** one phase-executor subagent
- **Depends on:** `pure-harness-parsers`, `timestamp-cache-semantics`,
  `agent-direct-write-migration`
- **May run with:** none
- **Base state:** accepted stable phases `pure-harness-parsers`,
  `timestamp-cache-semantics`, and `agent-direct-write-migration` are integrated;
  the worktree is clean and producer cutover remains unshipped. Immediately
  before dispatch, resolve actual HEAD, `origin/main`, and any configured
  upstream into the ephemeral manifest and capture the full production physical
  entry-point inventory, v1/v2 fixture statistics, and accepted 1 GiB adoption
  benchmark/pass-count baseline
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; any unexpected
  generated delta fails the phase
- **Write scope:** new-file header version, single relay selection site, task
  creation/reopen/resume/adoption plumbing, producer vocabulary, and guard tests
- **Data authority:** new physical file header is v2; existing file header
  remains authoritative v1/v2; selected relay and sink derive from it
- **Purpose:** enable v2 only after every reader and writer is ready
- **Deliverables:** new files write version 2; relay script selection is exact;
  Codex/OpenCode custom paths share it; existing v1 resume starts v1; alive
  adoption never restarts relay; all backend controls use file vocabulary;
  purity guard audits every physical line
- **Generated artifacts:** none expected; `make check` may verify generated files
  but an actual delta must be explained and separately approved
- **Change budget:** at most 8 production files and 8 tests; header change occurs
  in exactly one constructor/default; no v1 relay edit
- **Boundary:** no migration of existing logs, per-line conversion, public API
  change, or automatic repair; missing-header adoption still fails closed
- **Decision checkpoints:** stop if more than one new-version default appears, if
  existing files need rewriting, or if any producer cannot derive from header
- **Validation intent:** pure new v2 across relay/direct/provisioning/trailer and
  both `Task.WriteToLog` branches; resumed v1 remains byte-compatible; resumed v2
  remains v2; wrong script/type selection rejected; unknown/mixed files fail
  before append; every production physical entry point derives authority/parser/
  sink behavior from the header; prompt and pending-action identities occur
  exactly once across live/attach/replay; taskmeta cannot override the header;
  all line sizes fit; focused counting-reader tests hard-gate the accepted
  combined live-adoption/reopen logical read amplification and complete-pass
  counts for both v1 and v2. Same-host measurements remain advisory under the
  carry-forward contract.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; run focused deterministic
  pass-count/read-amplification and reopen tests, `go test
  ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/... ./backend/internal/server/...`, `python3
  backend/internal/agent/relay/test_relay.py`, `python3
  backend/internal/agent/relay/test_relay_v2.py`, and canonical same-host 1 GiB
  v1/v2 control/current benchmarks with pinned benchstat under the carry-forward
  contract; hash external results and remove transient raw transcripts after
  review; `make lint-fix`; `make check`; `git diff --check`; `git status
  --short`; `git diff --cached --name-only`
- **Review:** fresh full-diff reviewer receives pre-integration SHA, authority
  contract, producer inventory, exact fixtures/artifact delta, and all command
  results; require `PASS` before integration acceptance
- **Exit gate:** automated purity and entry-point audits find only `caic_meta`
  prefixed in v2, no bare native line, no `Task.WriteToLog` or other physical
  append bypass, and no reader using alternate authority; controlled existing v1
  stays entirely v1; deterministic combined-adoption/reopen logical read
  amplification and complete-pass counts do not regress for v1 or v2; advisory
  evidence follows the external artifact hash/cleanup protocol; full `make check`
  passes with no
  unexpected filesystem or staged delta
- **Handoff:** report new/existing cases, relay selection proof, purity audit,
  full validation, generated-state check, diffstat, and risks

### Phase 11: Prove real-runtime creation and adoption

- **Stable ID:** `runtime-adoption-smoke`
- **Responsible:** one phase-executor subagent
- **Depends on:** `v2-producer-cutover`
- **May run with:** none
- **Base state:** accepted stable phase `v2-producer-cutover` is integrated; the
  worktree is clean and the cutover is not treated as shipped until release
  authority confirms it. Immediately before dispatch, resolve actual HEAD,
  `origin/main`, and any configured upstream into the ephemeral manifest and
  capture md, container, caic binary, and controlled fixture identities
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; runtime containers
  and temporary logs may be created only under the smoke harness cleanup contract
- **Write scope:** real-runtime smoke test/harness and Makefile target only; no
  product implementation changes
- **Data authority:** smoke cases use inspected physical headers; runtime labels
  and script files are evidence only
- **Purpose:** prove purity and restart behavior against real md/container relay
  processes rather than fakes
- **Deliverables:** controlled pre-cutover v1 case and new v2 case; backend
  restart with live relay adoption; dead-relay restart/resume where safe; physical
  log and relay-output audits; deterministic cleanup; exact `smoke-wrapped-log`
  target
- **Generated artifacts:** temporary runtime instances/logs only, deleted by the
  harness; no committed recordings, `coverage.out`, generated output, or fixtures
- **Change budget:** one smoke test file plus minimal Makefile target wiring; no
  product-source edit
- **Boundary:** do not use fake server, fake runtime, or `smoketest` backend; do
  not depend on external LLM credentials for format assertions beyond the real
  harness/runtime path's guarded prerequisites
- **Decision checkpoints:** stop if a preserved environment may be modified, if a
  v1 fixture cannot be created reproducibly, or if cleanup cannot be guaranteed
- **Validation intent:** new v2 round-trip; alive v1 adoption remains v1; alive v2
  adoption remains v2; restart selects matching script for existing files;
  missing-header and mixed-file cases refuse attach/append; no duplicate log
  line. Where the smoke host satisfies the canonical resource gate, focused
  counting-reader tests hard-gate the accepted combined live-adoption/reopen
  logical read amplification and complete-pass counts. The same-host 1 GiB
  control/current result is recorded as advisory evidence with no fixed threshold.
- **Validation commands:** cwd `/home/user/src/caic`: verify the accepted benchmark
  source/evidence hashes and external artifact path; where the smoke host meets
  the resource gate, run focused deterministic pass-count/read-amplification and
  reopen tests plus the canonical same-host 1 GiB control/current benchmark with
  pinned benchstat under the carry-forward contract; otherwise block/escalate
  that local gate rather than shrinking it. Hash external results and remove
  transient raw transcripts after review; run `make smoke-wrapped-log`; `make
  lint-fix`; `make check`; rerun `make smoke-wrapped-log`; `git diff --check`;
  `git status --short`; `git diff --cached --name-only`; verify no runtime, temp,
  generated, staged, untracked, `coverage.out`, or repository performance
  artifact remains
- **Review:** fresh runtime/reliability reviewer inspects the integrated smoke
  harness, command logs, physical samples, cleanup evidence, and exact test-only
  delta; require `PASS`
- **Exit gate:** controlled v1/v2 creation and adoption pass on real md through
  `make smoke-wrapped-log`; byte-level audits show no mixed file; the hard
  deterministic combined-adoption/reopen logical read amplification and complete-
  pass result does not regress on a resource-qualified smoke host; advisory
  evidence follows the
  external artifact hash/cleanup protocol, and real restart timing is recorded;
  full `make check` passes; cleanup leaves no container, log, temp, generated,
  staged, untracked, `coverage.out`, or performance artifact
- **Handoff:** report environment, exact commands, sample hashes/vocabularies,
  restart results, cleanup proof, changed files, and residual host risks

## Completion condition

The rollout is complete only after every phase has been integrated in dependency
order, independently reviewed clean, validated on the integrated target, removed
from this active plan by the plan-maintenance subagent, and the real-runtime gate
has passed. Git history and durable tests/contracts are the changelog; completed
phase prose is deleted rather than accumulated here.
