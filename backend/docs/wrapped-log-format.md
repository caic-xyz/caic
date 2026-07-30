# Wrapped task-log format (v2) — design and rollout status

## Current status

The physical authority foundation is implemented and accepted:

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

The remaining producer and parser behavior is still legacy v1:

- New physical task-log files start with `{"type":"caic_meta","version":1,...}`.
- Relay stdout is persisted as bare harness-native JSONL. Relay controls use
  `caic_diff_stat`, `caic_exit`, and `caic_stripped_env`.
- Backend controls, metadata, provisioning output, prompt/compact input, Pi
  startup traffic, and other direct writes still use the v1 vocabulary. Codex
  and OpenCode handshake traffic is not persisted in task logs.
- Harness parsers still recognize selected `caic_*` records; there is no shared
  per-version record dispatcher or v2 relay yet.
- `ToolTimingTracker` exists, but persisted relay timestamps do not:
  `TimestampMessage` and `caic_ts` are not implemented.
- `eventreplay.CacheVersion` is 4.

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

There are two structural readers selected once from file authority:

```text
read authority from first caic_meta
switch version exactly:
  1 -> parseV1
  2 -> parseV2
  default -> unsupported-version error

parseV1(record):
  recognized caic_* control -> shared control conversion
  otherwise                 -> native harness parser

parseV2(record):
  agent                      -> validate ts/msg, parse native msg
  recognized v2 control      -> shared control conversion
  otherwise                  -> corruption error
```

The shared log parser owns control conversion and parsing state. In particular,
it owns the v1 `caic_model_info` / v2 `model_info` context-window snapshot and
applies that snapshot to parsed Pi usage when the native event lacks the value.
The Pi native parser does not route caic records.

A valid `agent.ts` produces timestamp semantics for timing-relevant native
messages. Timestamp semantics are transparent to replay compaction and
last-message/control-boundary searches: they advance conversion time without
becoming a user-visible event.

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

The accepted integration target currently consists of pre-integration HEAD
`9ef3f0340e58fc5c082a80db0ac0fa61a24d2a33`, the reviewed Phase
`log-authority-foundation` worktree delta, and this plan-maintenance edit. The
Phase implementation changed exactly 11 files (6 production and 5 tests), with
no staged or untracked paths before this edit; its fresh acceptance review
returned `PASS`.

Because the user owns staging and history, **no next phase may dispatch from
this dirty target**. The user must first commit the accepted foundation and plan
maintenance delta, or otherwise establish an equivalent clean recorded base.
Immediately before the next dispatch and every later integration, recapture the
actual HEAD, upstream SHA, dirty/staged/untracked paths, and sensitive-file
statistics. A changed manifest invalidates the phase contract.

- **Pre-integration HEAD:** `9ef3f0340e58fc5c082a80db0ac0fa61a24d2a33`
- **Target upstream/base at acceptance:**
  `aa95b310b9f96d87398ab7b7e3852bd867012887` (`host/caic-0`)
- **Branch:** `caic-0`
- **Accepted worktree delta:** the completed foundation's 11 files plus this
  documentation edit; no staged or untracked paths
- **VCS authority:** the user owns staging and commits. Subagents must not stage,
  commit, reset, rebase, merge, or push. Restore operations require coordinator
  approval and may touch only executor-created output.
- **Canonical command cwd:** `/home/user/src/caic`, unless a phase explicitly
  gives another cwd
- **Generated artifacts:** none expected for this docs rewrite. Implementation
  phases must declare any generator and exact output before running it.
- **Shipped contracts:** no migration, schema, or public API DTO change is
  approved by this plan.
- **Sensitive baseline:** `backend/docs/wrapped-log-format.md` was 544 lines,
  4,396 words, 30,533 bytes, SHA-256
  `f73ac26060e179fa3de797d25f4889085d0c76996a9ecb7000450e69ebc31b7e`.

Global phase rules:

1. One phase-executor subagent owns each phase. One writer may operate in a
   worktree at a time unless isolated worktrees and the recorded concurrency
   contract are both in use.
2. Before edits, record actual HEAD/upstream, dirty/staged/untracked paths,
   per-file baseline statistics for sensitive files, and the pre-integration
   target SHA.
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
5. The plan-maintenance subagent applies learnings, moves unresolved work into a
   named phase, deletes the accepted leading phase, and renumbers the remainder.
   Stable IDs never change.
6. `make check` is required at cutover and final gates. Runtime validation uses
   the real md/container path, never the fake server or `smoketest` backend.

## Dependency graph

```text
shared-versioned-parser -> persistent-read-paths
shared-versioned-parser -> live-relay-read-path
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

After a clean base is recorded, `shared-versioned-parser` and `relay-v2` may run
in isolated worktrees. `relay-v2` may continue with persistent reader work.
After `persistent-read-paths` is integrated, `versioned-log-sink` may run with a
still-pending `relay-v2` only in an isolated worktree; it may not run with parser
or persistent-reader work because its reopen constructor consumes the accepted
authority scan result. `pure-harness-parsers` and `timestamp-cache-semantics`
may run together after both read paths are integrated. Integration remains one
phase at a time.

## Active phased rollout

### Phase 1: Add the shared versioned parser

- **Stable ID:** `shared-versioned-parser`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** `relay-v2` in isolated worktrees after the accepted foundation
  and plan delta is committed as a clean recorded base
- **Base state:** clean recorded post-foundation base captured after the user
  commits the accepted foundation and plan-maintenance delta; recapture actual
  HEAD/upstream/status immediately before dispatch
- **VCS authority:** an active-worktree executor writes directly; approved
  isolated output is transferred only by a designated integration subagent using
  `git apply`; the user owns staging and history; no subagent may stage, commit,
  merge, reset, rebase, or push
- **Write scope:** shared record parser/converter and deterministic pending-action
  identity tracking in `backend/internal/agent`, plus focused tests; no
  read-call-site migration
- **Data authority:** consumes validated `LogVersion`; owns control-token maps,
  the model-context snapshot, and per-parse pending-action identity tracking;
  native parser state remains harness-owned
- **Purpose:** provide explicit `parseV1`/`parseV2` behavior without changing
  production call sites
- **Deliverables:** exact version dispatch; v1/v2 token maps; shared converters
  for every table entry; v2 `agent` validation/unwrapping; model-info state for Pi
  usage; unsupported and unknown-top-level errors; timestamp representation;
  deterministic pending-action deduplication keyed by
  `(kind, request ID, tool-use ID)`, so a compatibility top-level snapshot and
  its persisted native Claude `control_request` produce one semantic action
- **Generated artifacts:** none
- **Change budget:** at most 6 production files and 4 test files under
  `backend/internal/agent`; no harness parser edits
- **Boundary:** production readers/writers remain v1; do not remove caic cases
  from harness parsers or bump caches
- **Decision checkpoints:** stop if a control cannot be represented without a new
  public domain/API field, or if Pi usage requires duplicated persisted state
- **Validation intent:** both vocabularies yield equivalent domain messages;
  v2 rejects bare native and `caic_*` top-level records except `caic_meta`; v1
  preserves fallback; model-info affects only subsequent Pi usage missing native
  context; unknown versions fail before line dispatch; pending-action order is
  deterministic and native/top-level duplicates collapse to exactly one semantic
  action for both v1 and synthetic v2 sequences
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/...`; `make lint-fix`; `git diff --check`;
  `git status --short`; `git diff --cached --name-only`
- **Review:** independent parser/security review against integrated target and
  pre-integration SHA; require `PASS` and exact corruption behavior
- **Exit gate:** synthetic pure-v1 and pure-v2 sequences parse equivalently,
  forbidden mixed records fail, native plus compatibility pending-action records
  yield exactly one semantic identity, and no production reader or writer changes
- **Handoff:** include token coverage matrix, model-info evidence, exact rejected
  fixtures, pending-action identity/order cases, scoped diff, validation output,
  and risks

### Phase 2: Build the v2-only relay

- **Stable ID:** `relay-v2`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** `shared-versioned-parser` or `persistent-read-paths` in an
  isolated worktree
- **Base state:** clean recorded post-foundation base captured after the user
  commits the accepted foundation and plan-maintenance delta; recapture actual
  HEAD/upstream/status and byte/hash baselines for `relay.py` and `test_relay.py`
  immediately before dispatch
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

### Phase 3: Route persistent readers through version dispatch

- **Stable ID:** `persistent-read-paths`
- **Responsible:** one phase-executor subagent
- **Depends on:** `shared-versioned-parser`
- **May run with:** `relay-v2`
- **Base state:** clean integrated shared-parser target based on the recorded
  post-foundation commit; recapture actual HEAD/upstream/status and loader
  fixture statistics immediately before dispatch
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
  without creating another header or file-identity implementation
- **Deliverables:** plain/compressed/full/tail/session/export record semantics use
  the shared versioned parser on top of the accepted authority scanner; matching
  later-header, unknown-version, taskmeta identity, same-inode, and corruption
  behavior stays unchanged; full/reopen scan results expose already-persisted
  pending-action identities for later sink construction; offset-tail APIs cannot
  invent authority or the identity set from a tail fragment
- **Generated artifacts:** no committed artifacts; taskmeta/replay cache tests use
  temporary directories and existing golden recordings are not rewritten
- **Change budget:** at most 6 production files and 5 tests because physical
  authority, taskmeta identity, and same-inode validation are already complete;
  fixture additions are limited to one minimal v1 and one minimal v2 file
- **Boundary:** live relay reads and writers stay unchanged; harness parser caic
  cases remain until all read paths migrate
- **Decision checkpoints:** stop for compatibility weakening, skipped corruption,
  fixture rewrites, any second physical authority scanner, or any bypass of the
  accepted same-file identity checks
- **Validation intent:** equivalent v1/v2 full and tail results; compressed parity;
  taskmeta hit/miss/stale/corrupt integration; export parity; exact unknown-
  version, changed-segment, bare-v2, and prefixed-v2 errors; v1 and synthetic-v2
  full scans derive the same pending-action identity from a persisted native
  source; no fallback when a tail omits the first header
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/...`; `make lint-fix`; `git diff --check`;
  `git status --short`; `git diff --cached --name-only`
- **Review:** fresh reviewer checks all persistent entry points against the
  integrated SHA and fixture delta; require `PASS`
- **Exit gate:** an entry-point inventory proves every persistent path reuses the
  accepted physical authority scanner and dispatches records by its exact
  `LogVersion`; reopen consumers receive the authoritative scan's pending-action
  identity set, no duplicate scanner or identity index is introduced, existing
  same-inode/taskmeta/mixed-header tests stay green, and the exact allowed fixture
  delta is two files
- **Handoff:** list migrated entry points, pending-action scan-output API and
  identity cases, fixture hashes, behavior/error evidence, filesystem delta,
  validation, and risks

### Phase 4: Route live and startup relay reads

- **Stable ID:** `live-relay-read-path`
- **Responsible:** one phase-executor subagent
- **Depends on:** `shared-versioned-parser`, `relay-v2`
- **May run with:** none due to overlap across agent launch/read paths
- **Base state:** clean integrated parser and latent-relay target based on the
  recorded post-foundation commit; recapture actual HEAD/upstream/status and
  existing live-read/handshake test baselines immediately before dispatch
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
  normal-live log-once behavior; v2 unwrap for handshakes; Pi startup persistence;
  unpersisted Codex/OpenCode handshakes; control routing; physical offsets;
  attach-overlap deduplication; common script deployment/selection seam used by
  custom launchers; v1 byte preservation
- **Generated artifacts:** none
- **Change budget:** at most 10 production files and 8 tests; no protocol DTO or
  handshake sequence changes
- **Boundary:** selected version remains v1 in production; no header bump, v2
  deployment, sink migration, or harness-parser cleanup
- **Decision checkpoints:** stop if a handshake must receive a caic control as
  native data, if a consumer cannot state its persistence policy, if logging
  twice appears necessary, or if offset semantics would change for v1
- **Validation intent:** split reads, interleaved controls, handshake buffering,
  non-zero exits, attach physical offsets, relay-stdin overlap deduplication, no
  live stdin double write, v1 exact logged bytes, v2 physical-byte counts, Pi
  startup persistence, Codex/OpenCode handshake non-persistence, and all three
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

### Phase 5: Make harness parsers native-only

- **Stable ID:** `pure-harness-parsers`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`, `live-relay-read-path`
- **May run with:** `timestamp-cache-semantics` in an isolated worktree
- **Base state:** clean integrated persistent/live-reader target based on the
  recorded post-foundation commit; recapture actual HEAD/upstream/status and
  baseline each harness parser/test file immediately before dispatch
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
  shared entry points preserve output
- **Generated artifacts:** none
- **Change budget:** only the four parser families and adjacent tests/docs;
  expected net deletion; no golden fixture rewrite
- **Boundary:** no writer, relay, header, cache-version, or API change
- **Decision checkpoints:** stop if any production caller still sends a control to
  a native parser, or if removing model-info loses Pi replay semantics
- **Validation intent:** native fixtures parse unchanged through shared entry;
  direct native parsers reject/ignore caic records consistently; all control
  coverage lives in shared parser tests; regression/audit coverage proves the
  Phase 1 pending-action deduplication remains at the shared entry point; Codex
  native duration remains
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/... ./backend/internal/task/...`; `make lint-fix`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`
- **Review:** fresh harness-focused reviewer checks for remaining `caic_` parser
  branches and semantic regressions on integrated target; require `PASS`
- **Exit gate:** repository audit finds no caic routing in native parsers and all
  four golden suites pass without recording changes
- **Handoff:** provide audit command/result, per-harness tests, deletions/delta,
  validation, and unresolved risks

### Phase 6: Make timestamps and replay caches version-correct

- **Stable ID:** `timestamp-cache-semantics`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`, `live-relay-read-path`
- **May run with:** `pure-harness-parsers`
- **Base state:** clean integrated reader target based on the recorded
  post-foundation commit; recapture actual HEAD/upstream/status, cache version,
  and replay-fixture baselines immediately before dispatch
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; no generated
  replay recording without approval
- **Write scope:** timestamp domain semantics, `ToolTimingTracker`, replay filters,
  `backend/internal/eventreplay`, server replay tests, minimal fixtures
- **Data authority:** `agent.ts` is immutable producer time; conversion clock is
  derived; replay sidecars remain rebuildable caches
- **Purpose:** use exact v2 producer time without changing v1 files or exposing a
  timestamp event
- **Deliverables:** timestamp-transparent conversion and compaction; stable
  tool-duration replay; fallback for v1/no timestamp; timing-relevant gating;
  `CacheVersion` bump; pure-v1/v2 parity fixtures
- **Generated artifacts:** no committed generated sidecars; tests generate in
  temporary directories only
- **Change budget:** at most 8 production files and 7 tests; two minimal raw-log
  fixtures maximum; one intentional cache constant change
- **Boundary:** no API DTO field, v1 timestamp retrofit, relay change, or producer
  cutover
- **Decision checkpoints:** stop if timestamp must be persisted twice, becomes a
  user-visible event, or changes native harness duration precedence
- **Validation intent:** exact v2 tool spans; first-timestamp fallback; native
  duration precedence; v1 safe fallback; timestamps transparent to deltas,
  control searches, and exports; stale cache rejection and regeneration
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/server/api/v1conv/... ./backend/internal/eventreplay/...
  ./backend/internal/server/...`; `make lint-fix`; `git diff --check`;
  `git status --short`; `git diff --cached --name-only`
- **Review:** fresh timing/replay reviewer checks integrated semantics, cache bump,
  and fixture delta; require `PASS`
- **Exit gate:** v1/v2 fixture events are equivalent except exact v2 timestamps
  and durations; cache rebuild tests pass; no sidecar artifact is committed
- **Handoff:** report timing cases, old/new cache version, fixture hashes,
  filesystem delta, commands, and risks

### Phase 7: Introduce the enforcing versioned log sink

- **Stable ID:** `versioned-log-sink`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`
- **May run with:** a still-pending `relay-v2` in an isolated worktree; no parser
  or persistent-reader phase
- **Base state:** clean integrated `persistent-read-paths` target based on the
  recorded post-foundation commit; recapture actual HEAD/upstream/status, the
  accepted same-inode append constructor, direct `LogW.Write` inventory, and
  authoritative-scan outputs immediately before dispatch
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
  bump, relay selection, or parser cleanup
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
  mixed refusal before write; same-inode and concurrent append integrity; errors
  surfaced
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/... ./backend/internal/task/...`; `make lint-fix`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`
- **Review:** independent API/authority reviewer inspects the integrated sink and
  writer escape hatches; require `PASS`
- **Exit gate:** sink tests prove accepted same-inode behavior, v1 byte identity,
  typed-only v2 append/enforcement, continued untyped-v2 rejection, and restart-
  safe refusal of pending-action duplicates based on authoritative scan output,
  while all produced repository logs remain v1
- **Handoff:** include direct-write inventory, constructor/reopen API contract,
  authoritative identity-set evidence, v1/v2 reopen cases, byte fixtures,
  commands, exact delta, and remaining migration sites

### Phase 8: Migrate every direct log writer

- **Stable ID:** `agent-direct-write-migration`
- **Responsible:** one phase-executor subagent
- **Depends on:** `versioned-log-sink`, `live-relay-read-path`,
  `pure-harness-parsers`
- **May run with:** none; it crosses all agent backends and task writers
- **Base state:** clean integrated sink/readers/pure-parser target based on the
  recorded post-foundation commit; recapture actual HEAD/upstream/status, the
  complete physical reader/append inventory, direct-write inventory, and v1
  trace hashes immediately before dispatch
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
  reader or append bypass
- **Validation commands:** cwd `/home/user/src/caic`: `go test
  ./backend/internal/agent/... ./backend/internal/task/...`; `python3
  backend/internal/agent/relay/test_relay.py`; `make lint-fix`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`;
  audit direct `.Write` calls against an allowlist
- **Review:** fresh cross-backend reviewer receives inventory before/after,
  integrated SHA, v1 trace hashes, and gate; require `PASS`
- **Exit gate:** every task-log write, including both `Task.WriteToLog` branches,
  flows through the versioned sink or version-aware relay reader under an
  explicit persistence policy; a full production physical entry-point inventory
  has no authority/parser/append bypass; v1 trace hashes match; stdin and pending-
  action semantics occur exactly once; production still emits v1
- **Handoff:** report inventory closure, byte hashes, backend-specific tests,
  scoped exceptions, commands, and risks

### Phase 9: Cut new producers over to pure v2

- **Stable ID:** `v2-producer-cutover`
- **Responsible:** one phase-executor subagent
- **Depends on:** `pure-harness-parsers`, `timestamp-cache-semantics`,
  `agent-direct-write-migration`
- **May run with:** none
- **Base state:** clean integrated prerequisite target descended from the
  recorded post-foundation commit; recapture actual HEAD/upstream/status, full
  production physical entry-point inventory, and v1/v2 fixture statistics
  immediately before dispatch
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
  all line sizes fit
- **Validation commands:** cwd `/home/user/src/caic`: focused `go test
  ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/... ./backend/internal/server/...`; `python3
  backend/internal/agent/relay/test_relay.py`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; `make lint-fix`; `make check`;
  `git diff --check`; `git status --short`; `git diff --cached --name-only`
- **Review:** fresh full-diff reviewer receives pre-integration SHA, authority
  contract, producer inventory, exact fixtures/artifact delta, and all command
  results; require `PASS` before integration acceptance
- **Exit gate:** automated purity and entry-point audits find only `caic_meta`
  prefixed in v2, no bare native line, no `Task.WriteToLog` or other physical
  append bypass, and no reader using alternate authority; controlled existing v1
  stays entirely v1; full `make check` passes with no unexpected filesystem or
  staged delta
- **Handoff:** report new/existing cases, relay selection proof, purity audit,
  full validation, generated-state check, diffstat, and risks

### Phase 10: Prove real-runtime creation and adoption

- **Stable ID:** `runtime-adoption-smoke`
- **Responsible:** one phase-executor subagent
- **Depends on:** `v2-producer-cutover`
- **May run with:** none
- **Base state:** clean integrated cutover target descended from the recorded
  post-foundation commit; recapture actual HEAD/upstream/status plus md,
  container, caic binary, and controlled fixture identities immediately before
  dispatch
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
  missing-header and mixed-file cases refuse attach/append; no duplicate log line
- **Validation commands:** cwd `/home/user/src/caic`: `make smoke-wrapped-log`;
  `make lint-fix`; `make check`; rerun `make smoke-wrapped-log`; `git diff
  --check`; `git status --short`; `git diff --cached --name-only`; verify no
  runtime, temp, generated, staged, untracked, or `coverage.out` artifact remains
- **Review:** fresh runtime/reliability reviewer inspects the integrated smoke
  harness, command logs, physical samples, cleanup evidence, and exact test-only
  delta; require `PASS`
- **Exit gate:** controlled v1/v2 creation and adoption pass on real md through
  `make smoke-wrapped-log`; byte-level audits show no mixed file; full `make
  check` passes; cleanup leaves no container, log, temp, generated, staged,
  untracked, or `coverage.out` artifact
- **Handoff:** report environment, exact commands, sample hashes/vocabularies,
  restart results, cleanup proof, changed files, and residual host risks

## Completion condition

The rollout is complete only after every phase has been integrated in dependency
order, independently reviewed clean, validated on the integrated target, removed
from this active plan by the plan-maintenance subagent, and the real-runtime gate
has passed. Git history and durable tests/contracts are the changelog; completed
phase prose is deleted rather than accumulated here.
