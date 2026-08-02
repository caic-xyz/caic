# Wrapped task-log format (v2) — design and rollout status

## Current status

The physical authority foundation is shipped at the shared comparison role,
`origin/main`. The accepted local integration state is unshipped and adds:

- `LogVersion` as a closed type supporting exactly v1 and v2; unknown versions
  fail closed.
- First-header version/harness authority for every production plain and zstd
  reader, including matching later-segment enforcement and identity-validated,
  rebuildable `.taskmeta.json` derivation.
- Same-inode validation before raw append across `LogStore.Open`,
  `LogStore.Reopen`, and path-based `Task.WriteToLog`; missing, corrupt,
  mismatched, unknown-version, and valid-v2 raw append all fail closed.
- `LogRecordParser` with exact v1/v2 dispatch, shared control conversion, strict
  UTF-8 and envelope validation, ordered state, and pending-action deduplication.
- Explicit outer `ParsedMessage` metadata with `Message` and `ProducerTime`.
  Native callbacks remain `[]Message`, parsed output is `[]ParsedMessage`, and
  every consumer must explicitly unwrap `.Message`. This correction passed
  independent review and is accepted; the parser still has no production
  consumer.
- The accepted v1 adoption optimization: the exact combined live-adoption path
  fell from four to three complete passes plus a bounded tail. On the canonical
  1 GiB fixture, `rchar` divided by fixture size fell from 4.000 to 3.000.
  Corrected advisory medians were approximately 39.97 s to 36.33 s warm and
  40.53 s to 36.83 s cold. Trace evidence showed `encoding/json` dominates CPU.
  The accepted `load_benchmark_test.go`,
  `load_benchmark_cache_linux_test.go`, and
  `load_benchmark_cache_other_test.go` sources are immutable.

The locked unreleased-v2 decisions below supersede every local-only v2
`type`-discriminator or higher-than-millisecond timestamp assumption. Because v2
has not shipped, there is no alias or compatibility path to preserve: the current
fast-reader phase reshapes the shared parser/fixtures first, and the later
persistent-reader phase migrates task-layer bootstrap scanning.

For compatibility, breaking-change, and churn review, resolve `origin/main`
immediately before dispatch and freeze its exact commit as `SHIPPED_BASE` in the
ephemeral orchestration manifest. Resolve the accepted local foundation as
`LOCAL_BASE` in that same manifest. Accepted local work is an integration
prerequisite, not a published contract; implementation that exists only after
`SHIPPED_BASE` may be reshaped within an approved phase scope.

The remaining production behavior is still legacy v1:

- New physical task-log files start with `{"type":"caic_meta","version":1,...}`.
- Relay stdout is persisted as bare harness-native JSONL. Relay controls use
  `caic_diff_stat`, `caic_exit`, and `caic_stripped_env`.
- Backend controls, metadata, provisioning output, prompt/compact input, Pi
  startup traffic, and other direct writes still use the v1 vocabulary. Codex
  and OpenCode handshake traffic is not persisted in task logs.
- Production disk/live/export call sites do not yet use `LogRecordParser`, and
  harness parsers still recognize selected `caic_*` records. There is no v2
  relay or strict fast v2 record reader yet.
- `ToolTimingTracker` does not yet consume parsed producer-time metadata, and no
  production relay emits persisted per-message timestamps.
- `eventreplay.CacheVersion` is 4.

V1 performance is now compatibility-only. Every later v1 path must preserve
correctness, fail-closed behavior, and the accepted combined maximum of three
complete passes plus a bounded tail. There is no later v1 latency optimization
target; v1 wall time, throughput, allocations, and physical-read measurements
remain advisory. V2 owns the next hard adoption-performance gate.

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
- reserve `type` for v1 caic framing and use `t` on every v2 physical record,
  with no v2 alias or fallback while preserving nested harness-native keys;
- make canonical v2 agent records structurally fast to extract with one native
  semantic parse and no generic outer-envelope fallback;
- make each physical v2 encoder round its raw producer observation exactly once
  to milliseconds and require exact positive three-digit timestamps on read;
- keep v1 performance compatibility-only while hard-gating the one-pass v2
  adoption path;
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

The first non-empty record in a physical task-log file must have the semantic
token `caic_meta`. V1 bootstraps it with the exact discriminator key `type`; v2
bootstraps it with the exact discriminator key `t`. The bootstrap reader first
validates the closed `version` value and then requires the discriminator for that
version: exact v1 `type` for version 1, exact v2 `t` for version 2. A v2 `type`
alias, fallback, or compatibility path is forbidden. The header's `version` and
`harness` are authoritative for the entire file. Later `caic_meta` records are
legal segment markers only when their version-specific discriminator, `version`,
and `harness` match that first header. A missing first header, wrong or duplicate
discriminator, changed version, or changed harness is corruption.

`LogStore.Reopen`, path-based `Task.WriteToLog`, and every adoption/resume path
read this first-header authority from the same inode they would append. If the
local header is missing or unreadable, adoption fails closed: no relay attach,
relay restart, replacement header, or append is allowed. This is intentionally
stricter than guessing from runtime metadata or the deployed script. The current
fast-reader phase owns exact version-aware bootstrap discrimination at the shared
parser boundary; `persistent-read-paths` later owns migration of the task-layer
plain/zstd header and segment scanners to the same rule. Until that persistent-
reader migration, exact-`t` v2 is available only at the shared parser boundary,
not as a production task-layer adoption claim. Until the typed versioned sink is
integrated, raw append accepts only valid v1 authority; valid v2 is rejected
before append so an untyped writer cannot create a mixed file.

### v1 and v2 framing

A v1 file remains the frozen legacy format:

- `caic_meta` version 1;
- bare harness-native records;
- `caic_*` controls and metadata; and
- the existing v1 relay script and byte stream.

Do not add `caic_ts` to v1.

A v2 file contains only caic-defined top-level records:

- every physical record, including the version 2 `caic_meta` bootstrap and later
  segment markers, uses the discriminator key `t`; `type` is v1-only at the
  physical caic envelope layer;
- exact bootstrap examples are `{"type":"caic_meta","version":1,...}` for v1
  and `{"t":"caic_meta","version":2,...}` for v2;
- every harness-native stdin or stdout record uses the exact physical byte shape
  `{"t":"agent","ts":<timestamp>,"msg":<native JSON value>}\n`;
- the outer agent field order is exactly `t`, `ts`, `msg`; `t` is first and
  `msg` is final, with no extra envelope field, alternate ordering, insignificant
  outer whitespace, or generic slow fallback;
- `<timestamp>` is positive Unix epoch seconds in the single cross-language
  canonical grammar `(0|[1-9][0-9]*)\.[0-9]{3}`, with `0.000` rejected: exactly
  three fractional digits, no redundant integer leading zero, and no sign or
  exponent; integer overflow or a value outside the reader's finite Unix-time
  representation is corruption. Readers validate this exact form and never
  round input;
- each physical encoder rounds its raw positive observation exactly once to the
  nearest millisecond, with an exact half millisecond rounded upward, including
  carry into the next second. Given positive integer Unix nanoseconds `ns`,
  canonical total milliseconds are `(ns + 500_000) / 1_000_000` using integer
  division, then split into seconds and a zero-padded three-digit millisecond
  field. The encoder rejects nonpositive input, addition/formatting overflow, a
  rounded `0.000`, or a rounded result outside the reader's supported Unix-time
  range before emitting bytes. Thus `1_234_499_000 ns` becomes `1.234`,
  `1_234_500_000 ns` becomes `1.235`, and `1_999_500_000 ns` becomes `2.000`;
  backend direct-writer callers pass their raw `time.Time` observation unchanged
  to `VersionedLogSink`, which alone rounds and canonically formats every backend-
  written v2 agent record. Callers must not pre-round. The separate Python v2
  relay is its own physical encoder and rounds its raw clock observation exactly
  once by the same algorithm. Readers never round;
- caic controls and metadata use strict unprefixed semantic tokens under `t`;
  because they are small they may use ordinary decoding, but ordinary decoding
  must reject a missing or duplicate `t`, any top-level `type` (including both
  keys together), and unknown fields/tokens rather than admit an alias;
- an `agent` envelope with `msg:null` is corruption; and
- an unknown, duplicate, missing, or reordered top-level agent field or semantic
  token is corruption, not a harness fallback.

For a native logical JSON object, array, string, number, or boolean, the producer
strips only surrounding JSON whitespace (`SP`, `HT`, `LF`, and `CR`) and embeds
the remaining native value bytes unchanged. It must not semantically reserialize
or normalize those bytes. A native logical JSON null is not a valid v2 `msg`;
like a logical harness line that is not valid JSON or UTF-8, it is emitted as a
JSON string containing the existing bounded diagnostic representation rather
than as `msg:null`. Producers may use necessary output buffers, copies, repeated
writes, or equivalent emission mechanics. Before emitting any bytes, the v2
relay and backend sink must compute and enforce the final encoded physical-record
size, not merely the raw-input size, and every destination must receive the same
byte-identical canonical record. The complete encoded record, including its
terminating LF, must be strictly smaller than the shared 32 MiB scanner limit.
An oversized logical line produces one bounded diagnostic record and is never
silently split.

### v2 vocabulary

The table lists semantic token values, which do not change under this decision.
V1 caic records carry their token under `type`; every v2 physical record carries
its token under `t`. A native value nested under v2 `msg` retains its harness-
owned keys, including any native `type` key; those payload keys are unrelated to
the physical v2 discriminator and must be byte-preserved.

| Purpose | v1 token (`type`) | v2 token (`t`) | Producer |
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

There are two structural readers selected once from file authority. V1 retains
its accepted compatibility parser. V2 uses the strict fast record reader below;
it never routes an `agent` envelope through generic outer-object decoding. The
shared parser returns an outer stream value:

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
read and validate version from first caic_meta bootstrap
switch version exactly and require its discriminator:
  1 + exact `type:"caic_meta"` -> parseV1
  2 + exact `t:"caic_meta"`    -> parseV2
  default or wrong key          -> corruption/unsupported-version error

parseV1(record):
  recognized type=caic_* control -> shared control conversion
  otherwise                      -> native harness parser
  wrap every semantic result with zero ProducerTime

parseV2(record):
  exact canonical `t:"agent"` prefix:
    parse exact three-digit canonical ts from the fixed delimiters; never round
    slice bounded msg bytes directly from the scanner record
    messages = native harness parser(msg) exactly once
    wrap every semantic result with ProducerTime = envelope ts
  recognized strict v2 t=control:
    messages = shared control conversion using ordinary decoding
    wrap every semantic result with zero ProducerTime
  otherwise -> corruption error, with no alias or fallback
```

The package-private v2 fast reader accepts one scanner-owned record at a time.
For `agent`, it verifies the exact `{"t":"agent","ts":` prefix, delimiters,
field order, exact three-digit canonical timestamp grammar, final `msg` position,
closing brace, and record size before exposing a bounded zero-copy `msg` slice.
It rejects `0.000`, integer/no-fraction timestamps, one-, two-, or four-or-more-
digit fractions, signs, exponents, redundant integer leading zeros, overflow,
and values outside the reader's finite Unix-time representation; it does not
round input. The slice is valid only for the synchronous native-parser callback
and must not escape or survive scanner advance. It performs exactly one native
semantic parse, returns `[]ParsedMessage`, and never calls `json.Unmarshal` on
an outer `agent` envelope. Nested objects and arrays, strings, numbers, booleans,
escapes, valid UTF-8, and diagnostic strings are handled without weakening the
exact outer shape. An `agent` record whose `msg` is the JSON literal `null` is
corruption. Malformed, oversized, unknown, duplicate, or reordered records fail
closed; a v2 `type` discriminator and every generic agent-envelope fallback are
forbidden.

The shared log parser owns control conversion and parsing state. In particular,
it owns the v1 `caic_model_info` / v2 `model_info` context-window snapshot and
applies that snapshot to parsed Pi usage when the native event lacks the value.
The Pi native parser does not route caic records.

Every semantic message produced from one valid v2 `agent` envelope carries that
same immutable `ProducerTime`: the producer observation already rounded to the
nearest millisecond by the deterministic rule above. Readers preserve it exactly
and never re-round it. This applies to messages that are irrelevant to tool
timing. V1 records and controls without envelope producer time carry zero time.
Timing relevance is exclusively downstream `ToolTimingTracker` policy, not
parser framing policy. Producer timestamps are metadata only: they never become
semantic or user-visible events, and replay compaction and last-message/control-
boundary searches operate on the explicitly unwrapped message. Timing and replay
precision for v2 producer observations is milliseconds.

### Relay and direct-write contract

`relay.py` remains frozen v1. `relay_v2.py` is a separate, v2-only script; code
is copied rather than shared so a relay process cannot switch vocabularies. Its
encoder depends on the accepted strict fast-record contract and shares byte-
exact cross-language canonical envelope fixtures with the Go reader and sink.

The v2 relay emits the exact canonical `t`, `ts`, `msg` bytes for every persisted
stdout line and every stdin line that it is configured to log. As its own
physical encoder, it captures a raw positive Unix clock observation, rounds that
observation exactly once to the nearest millisecond with ties upward and second
carry, and formats exactly three fractional digits; it never emits zero or a
noncanonical timestamp. It preserves native value bytes without semantic
reserialization, computes and enforces the final encoded size before emission,
and may use necessary output buffers, copies, or repeated writes. It preserves
the v1 stream asymmetry:
`output.jsonl` contains framed stdout, controls, and (when `log_stdin` is
enabled) framed stdin, while the live connected client receives stdout and
controls without an stdin echo. Every record sent to the client is byte-for-byte
identical to its copy in `output.jsonl`, but `output.jsonl` is a superset. Attach
replay from a physical byte offset may therefore replay persisted stdin. No
direction field is added; native payloads retain their existing protocol `type`
keys. `ProducerTime` is the immutable rounded producer observation captured when
the logical record is observed; its precision is milliseconds.

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
`io.Writer`. Its authority is the physical file's `LogVersion`. Before v2 reopen
or append it consumes a matching in-memory validated snapshot and performs O(1)
physical identity verification; missing proof, identity/EOF change, or mismatch
fails closed or enters only an explicitly validated fallback. It provides
explicit operations for native records and caic controls, validates exact v1
`type` versus v2 `t` vocabulary, and refuses mixed writes or any v2 `type` alias.
For v2 native records `VersionedLogSink` accepts the raw `time.Time` producer
observation supplied unchanged by its backend caller. The sink is the physical
encoder and sole rounding/formatting authority: it rounds exactly once to nearest
millisecond with ties upward and second carry, then emits exactly three fractional
digits for every backend-written v2 agent record. A caller must not pre-round;
zero or out-of-range time fails before emission. The sink is the exclusive live
task-log persistence path for prompt, compact, and `SendRaw` input. Together with
the version-aware relay reader, it covers all current write paths, including:

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
  log metadata cache. `ValidatedLogSnapshot` is an in-memory, non-persisted
  derivation bound to device, inode, size, mtime, and validated EOF; it may carry
  parsed v2 authority, controls, session state, semantic messages, and pending-
  action identities only for that exact physical observation. A taskmeta cache
  may copy log version/harness only after raw-log identity validation; it is
  rebuildable and never an authority.
- **Immutable snapshot:** session, model context, PR, result, pending action,
  provisioning log, context-reset, and native protocol records already appended
  to a file. An agent record's `ProducerTime` is its already-rounded producer
  observation and is immutable at millisecond precision.
- **Provenance:** `agent` framing identifies native harness protocol content;
  top-level controls identify caic-owned content. Provenance is not identity.
- **Presentation:** rendered events, exported markdown, warnings, and bounded
  malformed-line diagnostics.
- **Redundant:** launch flags, runtime metadata, deployed filenames, and relay
  liveness are not alternate version authorities and must never be used as a
  fallback.

Forbidden additions and fallbacks:

- no second authoritative version field outside `caic_meta`; the sole approved
  persisted copy is the identity-bound, rebuildable `.taskmeta.json` cache, and
  a validated snapshot must remain in memory rather than persist a second
  authority;
- no version inference from discriminator/token prefixes, task age, binary
  version, runtime metadata, relay filename, or live process;
- no `version >= 2` compatibility shortcut;
- no per-line v1/v2 guessing;
- no v2 `type` alias, fallback, or backward-compatibility path: v1 caic framing
  alone uses `type`, and every v2 physical record uses `t`;
- no v2 fallback to a native harness parser for an unknown top-level token;
- no rewriting a missing header during adoption;
- no mixed-format recovery append; and
- no schema migration or public API field for log version.

## Preflight manifest

The living plan records symbolic prerequisites; exact volatile repository
execution-base and branch-tip Git identities are ephemeral evidence. This
restriction does not apply to authoritative dependency pseudo-versions or
content hashes: those are durable tool/content identities and may remain exactly
pinned in the plan for reproducibility. The base roles and complete manifest
must be freshly captured and recorded immediately before each phase dispatch,
integration, and review:

- **Shipped/shared comparison role:** resolve `origin/main` to an exact commit and
  record it as `SHIPPED_BASE` in the ephemeral orchestration manifest.
  Compatibility, breaking-change, and combined parser churn use that frozen
  value, never the later value of a moving `origin/main`.
- **Local dispatch/integration role:** confirm that the accepted unshipped
  physical-authority, parsed-message metadata, and v1 adoption-performance
  results are present, including the three immutable adoption benchmark sources,
  then record actual HEAD as `LOCAL_BASE` in the fresh ephemeral manifest. The
  parser has no production consumers and does not establish a published API
  contract.
- **Completed phase role:** after implementation and again after integration,
  freshly capture and record the exact resulting HEAD as `PHASE_FINAL` in
  ephemeral status and the phase handoff. Immediately before review, freshly
  capture and record all three exact values in the reviewer prompt.

Focused and full agent tests, the changed-package race test, vet, mandatory lint,
method placement, diff, and status checks passed for the accepted local parser.
The full `agent/...` race run still has the pre-existing Pi one-second timeout
reproduced at its clean comparison base; it is not evidence against the local
parser and remains outside this rollout. `ParsedMessage` replaced
`TimestampMessage` and passed independent review. The accepted v1 adoption gate
also passed fresh independent review: three complete passes plus bounded tail,
canonical 1 GiB `rchar`/fixture 4.000 to 3.000, corrected advisory
warm/cold medians recorded above, and `encoding/json` identified as the dominant
CPU cost. Those durable results and immutable benchmark sources are prerequisites
rather than active phases.

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
   requires fresh capture and recording before continuing. This volatile Git-SHA
   rule does not prohibit exact authoritative dependency pseudo-versions or
   content identities, which remain durable and reproducibly pinned.
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
8. Every affected phase performs an exhaustive stale-format audit over its full
   write scope, fixtures, examples, and generated physical samples. Search for v2
   physical `type` discriminator examples and for the former `[0-9]{6}` /
   `0.000000` / six-digit / microsecond timestamp grammar and language. Classify
   every match: only exact v1 `type`, unrelated harness-native payload keys,
   explicit corruption inputs, and the audit patterns themselves may remain.

## Dependency graph

```text
v2-fast-record-reader -> relay-v2
v2-fast-record-reader -> persistent-read-paths
relay-v2 -> live-relay-read-path
v2-fast-record-reader -> live-relay-read-path

persistent-read-paths -> pure-harness-parsers
live-relay-read-path -> pure-harness-parsers
persistent-read-paths -> timestamp-cache-semantics
live-relay-read-path -> timestamp-cache-semantics
persistent-read-paths -> versioned-log-sink

persistent-read-paths -> v2-adoption-performance
versioned-log-sink -> v2-adoption-performance
versioned-log-sink -> agent-direct-write-migration
live-relay-read-path -> agent-direct-write-migration
pure-harness-parsers -> agent-direct-write-migration
v2-adoption-performance -> agent-direct-write-migration

pure-harness-parsers -> v2-producer-cutover
timestamp-cache-semantics -> v2-producer-cutover
agent-direct-write-migration -> v2-producer-cutover
v2-adoption-performance -> v2-producer-cutover
v2-producer-cutover -> runtime-adoption-smoke
```

The accepted parsed-message metadata and v1 adoption result are prerequisites of
the active graph. The strict fast reader is integrated first so both relay and
persistent paths implement one canonical contract. After it, `relay-v2` and
`persistent-read-paths` may run in isolated worktrees because their write scopes
are disjoint. `live-relay-read-path` waits for the relay; `versioned-log-sink`
waits for the persistent snapshot contract and may not overlap that work.
`pure-harness-parsers` and `timestamp-cache-semantics` may run together only
after both read paths are integrated. `v2-adoption-performance` waits for the
persistent reader and sink, and both direct-writer migration and final cutover
wait for its hard deterministic gate. Integration remains one phase at a time.

## Active phased rollout

### Phase 1: Implement the strict v2 fast record reader

- **Stable ID:** `v2-fast-record-reader`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** none
- **Base state:** the accepted physical-authority parser, `ParsedMessage`
  metadata, and v1 adoption result are integrated locally but unshipped; no
  production consumer uses `LogRecordParser`; the worktree matches the declared
  clean/accepted-delta condition. Immediately before dispatch, resolve actual
  HEAD, `origin/main`, and any configured upstream into the ephemeral manifest;
  record parser/test statistics, current v2 decoding call graph, the accepted
  three-source benchmark hashes, and an absolute external artifact directory
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push. Before generated-
  index replay, the coordinator obtains a user-owned intent-to-add checkpoint for
  any approved new Go source; the executor verifies no staged content with `git
  diff --cached --exit-code`
- **Write scope:** package-private v2 record extraction, shared-parser dispatch,
  exact version-aware first-bootstrap discrimination, and a dedicated
  package-private v1 compatibility extraction at
  `backend/internal/agent/v1_record.go` under `backend/internal/agent`; adjacent
  focused tests/benchmark; one shared byte fixture contract at
  `backend/internal/agent/testdata/v2_agent_records.json`; and only generated
  first-line index changes in `backend/AGENTS.md`. The sole exception to the
  harness-native-parser exclusion consists exactly of
  `backend/internal/agent/pi/parse.go` and
  `backend/internal/agent/pi/parse_test.go`, only to make the unknown-event
  `RawMessage` path consume and validate the complete native JSON value, plus
  `backend/internal/agent/pi/pi.go`, only for the single stateful
  `piWireFormat.ParseMessage` call-site adjustment needed to remove the
  forwarding-only `decodeEventType` wrapper. No task-layer plain/zstd header
  scanner, server, relay, other harness-native parser, or producer file.
  Task-layer bootstrap migration remains owned by `persistent-read-paths`
- **Data authority:** file version/harness remain first-header authority; validated
  version selects exact v1 `type` or v2 `t` bootstrap discrimination; the scanner
  record and its bounded `msg` subslice are ephemeral derived views; canonical
  `agent.ts` is immutable producer metadata already rounded to milliseconds.
  Shared fixture raw observations are deterministic encoder inputs and fixture
  bytes describe transport behavior; neither is an alternate authority
- **Purpose:** accept canonical v2 agent records with one allocation-conscious
  structural extraction and exactly one native semantic parse before any relay
  or production reader integration
- **Deliverables:** a natural package-private reader that verifies the exact
  `{"t":"agent","ts":` prefix, canonical positive three-fraction timestamp,
  `,"msg":` delimiter, final message position, closing brace, and strict outer
  shape while the enclosing scanner accounts for required LF and size; obtains a
  zero-copy bounded `msg` slice from the LF-free scanner record; invokes the
  existing native `[]Message` callback exactly once; returns `[]ParsedMessage`
  with the exact millisecond timestamp on every result; validates exact v1
  `type:"caic_meta"` versus v2 `t:"caic_meta"` bootstrap according to the closed
  parser version; uses ordinary decoding only for strict small v2 `t` controls;
  rejects `type` on every v2 record with no alias/backward-compatibility path;
  never rounds reader input, calls `json.Unmarshal` on an outer agent envelope,
  or has a generic slow fallback; keeps v1 behavior isolated in the dedicated
  package-private `v1_record.go`; and includes focused microbenchmark and CPU/
  allocation trace/profile evidence. In the exact Pi exception, an unknown event
  type must consume and validate one complete native JSON value through its
  closing object and EOF (apart from trailing JSON whitespace) exactly once
  before returning an unchanged `RawMessage`; malformed remainder such as
  `{"type":"future","x":}` must fail. Known Pi event dispatch and behavior
  remain unchanged, and this targeted validation must not become a pre-scan for
  known types. One decoder-returning `decodeEventType` helper is used by both
  `parseMessage` and `piWireFormat.ParseMessage`; the latter ignores the
  decoder. There is no duplicate parsing implementation or broader `pi.go`
  behavior change. The shared fixture contract keeps reader cases and a separate
  encoder-vector collection. Every encoder vector contains
  `observed_unix_ns` as a base-10 decimal string for cross-language exactness,
  `expected_timestamp` in canonical grammar, `native_bytes`, and complete
  LF-terminated `record_bytes`. It includes deterministic below-half
  (`1234499000` -> `1.234`), exact-half (`1234500000` -> `1.235`), above-half
  (`1234501000` -> `1.235`), and second-carry (`1999500000` -> `2.000`) vectors.
  Python relay and Go sink tests reuse these byte-exact vectors read-only; the
  fixture is the sole source of rounding expectations
- **Generated artifacts:** `backend/AGENTS.md` may be regenerated only by `make
  lint-fix` from `/home/user/src/caic` after any required user-owned intent-to-
  add checkpoint. Benchmark output, CPU/memory profiles, traces, test binaries,
  and analysis remain in the coordinator-provided external artifact directory
  and are removed after review; the shared JSON fixture is reviewed source, not
  generated output
- **Change budget:** at most 5 production files and 4 test/benchmark files: the
  package-private v1/v2/shared-parser work plus exactly the Pi production and
  test exception named above; also the one shared fixture and generated
  `backend/AGENTS.md`. No dependency, API DTO, native callback signature, other
  harness-parser edit, or broad parser rewrite
- **Boundary:** preserve v1 `type` bytes, v1 parser correctness, fail-closed
  authority, `[]Message` native callbacks, `[]ParsedMessage` shared output,
  semantic token values, native payload keys, control semantics, ordered parser
  state, null corruption, and the accepted v1 maximum of three complete passes
  plus bounded tail; preserve all known Pi event behavior and limit the Pi change
  to complete validation on the unknown-type `RawMessage` branch, with no broader
  harness-parser cleanup or work pulled forward from `pure-harness-parsers`; do
  not migrate task-layer bootstrap scanners in this phase. In
  `parseV2Record`/`parseV2AgentRecord`, do not restore `json.Valid` or any other
  generic payload-wide pre-scan, and do not add an outer-envelope
  `json.Unmarshal`, payload-sized envelope-extraction copy, public abstraction,
  alternate field order, extra field, `type` alias, timestamp normalization, or
  fallback parser
- **Decision checkpoints:** stop if scanner-buffer lifetime cannot remain
  synchronous and explicit, if the native callback must retain input bytes, if
  exact extraction requires semantic reserialization, if ordinary control
  decoding could accept noncanonical agent data, or if any new authority/public
  API is needed
- **Validation intent:** exact success for `{"t":"agent","ts":1.000,"msg":...}`
  with nested objects/arrays, escaped strings, internal native whitespace,
  canonical payload bytes derived from logical values with surrounding JSON
  whitespace stripped, JSON string/number/boolean scalar kinds, valid UTF-8,
  diagnostic strings, maximum accepted size, and one-to-many/zero-message native
  parses. Validate the shared fixture schema, decimal-string raw nanoseconds,
  expected timestamp, native bytes, and LF-terminated record bytes; require the
  named below-half, exact-half, above-half, and second-carry encoder vectors in
  addition to reader cases. Exact v1 `type` and v2 `t` `caic_meta` bootstrap/
  segment success occurs under their validated versions. Exact corruption
  rejection covers
  `{"t":"agent","ts":1.000,"msg":null}`, v2 `type` agent/control/meta records,
  v1 `t` bootstrap, both discriminator keys, missing/duplicate discriminator,
  malformed UTF-8, malformed/oversized records, unstripped surrounding `msg`
  whitespace, `0.000`, integer/no fraction, one-, two-, and four-or-more-digit
  fractions, signs, exponents, redundant integer leading zeros, overflow and
  out-of-range times, unknown fields/tokens, duplicate/missing/reordered agent
  fields, extra outer whitespace, trailing data, and delimiter spoofing inside
  nested strings. Reader tests prove accepted timestamps are preserved exactly
  without rounding; instrumentation proves one native callback, no outer agent
  `encoding/json.Unmarshal`, no generic payload-wide fast-reader pre-scan, no
  payload-sized fast-reader envelope-extraction copy, and bounded allocation
  behavior. Focused Pi tests prove that a valid unknown event still returns the
  byte-identical `RawMessage`, malformed content after its `type` field (including
  `{"type":"future","x":}`) and trailing non-whitespace JSON data are
  rejected, the unknown path consumes the complete native JSON exactly once, and
  representative known Pi events retain their existing messages/errors
- **Validation commands:** cwd `/home/user/src/caic`: run focused fast-reader
  tests and microbenchmarks with `go test ./backend/internal/agent -run
  '^Test.*V2.*Record' -bench '^Benchmark.*V2.*Record' -benchmem`; capture a
  representative CPU profile/runtime trace externally and inspect with `go tool
  pprof`/`go tool trace`; run `go test ./backend/internal/agent/pi -run
  '^TestParseMessage$'` and `go test ./backend/internal/agent/...`; rerun the
  accepted focused v1 counting-reader compatibility tests without changing the
  immutable benchmark sources; audit for outer-agent `json.Unmarshal`, any
  `json.Valid` or other generic payload-wide pre-scan in
  `parseV2Record`/`parseV2AgentRecord`, fallback paths, fast-reader envelope-
  extraction copies, callback count, complete exactly-once validation before the
  Pi unknown-event `RawMessage` return, changes to known Pi behavior or any other
  harness parser, any v2 physical `type` discriminator/example, and any stale
  timestamp grammar/precision contract, applying global rule 8's exclusions for
  v1 framing and native payload keys; run `make lint-fix`, `make lint-docs`, `git
  diff --check`, `git diff --cached --exit-code`, `git status --short`, and `git
  ls-files --others --exclude-standard`
- **Review:** a fresh Go parsing/performance reviewer receives the integrated
  target and exact pre-integration base from fresh ephemeral state, fixed byte
  contract, fixture schema with reader cases plus all four raw-nanosecond encoder
  vectors, allocation/callback/pre-scan instrumentation, profile/trace summary,
  v1 compatibility evidence, focused Pi unknown-event complete-validation and
  known-event-preservation evidence, an audit proving the exception touched only
  the three named Pi files and introduced no generic fast-reader pre-scan or
  broader harness cleanup, exact artifact delta, and gate; require `PASS`
- **Exit gate:** all canonical and corruption cases pass; the shared fixture
  schema contains reader cases plus deterministic below-half, exact-half, above-
  half, and second-carry encoder vectors, each with a decimal-string raw Unix-
  nanosecond observation, expected canonical timestamp, native bytes, and complete
  LF-terminated record bytes. Those bytes contain only v2 physical `t`
  discriminators plus canonical three-digit positive timestamps, and the fixture
  is the sole cross-language rounding expectation; version-aware shared-parser
  bootstrap tests require exact v1 `type` and exact v2 `t` while task-layer
  migration remains deferred; agent extraction performs one native semantic
  callback and no outer-envelope `encoding/json.Unmarshal`, generic payload-wide
  pre-scan (including `json.Valid`), generic fallback, input rounding, or payload-
  sized envelope-extraction copy. The dedicated package-private `v1_record.go`
  preserves v1 behavior. Every valid unknown Pi event is returned as an unchanged
  `RawMessage` only after exactly one complete native-JSON validation, malformed
  remainder and trailing data fail, known Pi events remain unchanged, and no
  harness parser beyond the three-file exception changes; stale-format audits
  are clean; microbenchmark/profile evidence is recorded; v1 correctness and
  three-pass-plus-tail compatibility do not regress; only the allowed
  repository and external artifact deltas exist
- **Handoff:** report API/signature boundaries, shared-parser bootstrap split and
  deferred task-layer migration, dedicated v1 compatibility boundary, scanner-
  slice lifetime proof, exact three-digit timestamp parser/rejection rules,
  callback/copy/unmarshal/generic-pre-scan evidence, Pi unknown-event complete-
  validation tests and known-event preservation, the three-file exception audit,
  fixture schema/vector inventory and hash, stale-format audit commands/results,
  benchmark/profile/trace commands and results, v1 compatibility, generated-
  index/intent state, exact files, cleanup, and residual risks

### Phase 2: Build the v2-only relay

- **Stable ID:** `relay-v2`
- **Responsible:** one phase-executor subagent
- **Depends on:** `v2-fast-record-reader`
- **May run with:** `persistent-read-paths` in an isolated worktree
- **Base state:** accepted stable phase `v2-fast-record-reader` is integrated,
  the v2 relay is not yet implemented, and all rollout changes remain unshipped.
  The worktree matches the declared clean/accepted-delta condition.
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
  comment, read-only consumption of the accepted shared canonical fixture, and
  generated `backend/AGENTS.md` file-index output only
- **Data authority:** script version is derived from backend selection; the relay
  captures a raw clock observation and, as its own physical encoder, rounds it
  exactly once. The resulting millisecond `ProducerTime` is immutable metadata,
  not version authority
- **Purpose:** create a single-purpose v2 physical stream without activating it
- **Deliverables:** copied v1 infrastructure with byte-exact canonical v2
  stdout/logged-stdin framing in exact `t`, `ts`, `msg` order; every v2 physical
  control also discriminated by `t`; positive Unix seconds with exactly three
  fractional digits and no sign/exponent/leading zero; deterministic producer
  rounding from positive Unix nanoseconds via `(ns + 500_000) / 1_000_000`, with
  half milliseconds upward and carry into the next second; stripping only
  surrounding native JSON whitespace while preserving the remaining native value
  bytes without semantic reserialization; computing and enforcing the final
  encoded-record size before emission; necessary output buffers, copies, and
  repeated writes permitted; `output.jsonl` as a client-stream superset; byte
  identity for every canonical record sent to both destinations; no live stdin
  echo; strict unprefixed control tokens under `t`; newline carry and EOF flush;
  native logical JSON null, invalid JSON/UTF-8, and oversized diagnostics using
  the existing bounded representation rather than `msg:null`; read-only
  consumption of the shared fixture's raw-observation encoder vectors for byte-
  exact Go reader/relay/sink parity, with no relay-local copy of rounding expected
  values; no v1-emitting branch or v2 `type` alias; update `embed.go`'s first-line
  purpose comment from one-script wording to describe the two version-specific
  relay
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
  no mutation of the accepted canonical fixture or fast-reader contract, no
  zero-copy producer requirement, no semantic token change, no v2 `type`
  compatibility, and no edit to frozen v1 behavior
- **Decision checkpoints:** stop if the final-record limit cannot fit existing
  scanner bounds, if a record sent to both client and output must differ between
  destinations, or if sharing v1 code is proposed
- **Validation intent:** chunk boundaries, blank lines, partial EOF, logged and
  unlogged stdin, no live stdin echo, attach replay from a physical offset,
  stdout, concurrent `t` controls, nested/escaped JSON plus string/number/boolean
  scalars, surrounding native whitespace, and read-only execution of every shared
  raw-observation encoder vector, including its below-half, exact-half, above-half,
  and second-carry cases. The shared fixture supplies all expected timestamps and
  bytes; tests do not duplicate them. Every output has a positive in-range
  timestamp with exactly three fractional digits; zero, negative, overflow, or
  out-of-range producer observations fail before emission.
  Native logical JSON null converts to the existing bounded diagnostic string
  with no `msg:null`; also cover invalid UTF-8, oversized input, final-size
  enforcement before emission, byte-exact cross-language fixture parity, per-
  record client/file identity, output-superset framing, and absence of extra/
  reordered fields, v2 `type`, or v1 emissions
- **Validation commands:** cwd `/home/user/src/caic`: after the coordinator
  records the user-owned `git add -N` checkpoint for the two approved new files,
  run `git status --short`; `git diff --cached --exit-code`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; `python3
  backend/internal/agent/relay/test_relay.py`; `ruff check
  backend/internal/agent/relay`; `make lint-fix`; `git diff --check`; `git
  diff --cached --exit-code`; `git status --short`; `git ls-files --others
  --exclude-standard`; compare v1 file hashes, verify the generated index, and
  run the global stale-format audit over relay output/tests, excluding v1 and
  nested native payload keys exactly as rule 8 permits
- **Review:** fresh Python/concurrency reviewer inspects the two new scripts,
  latent embed wiring, `embed.go`'s two-version first-line purpose comment, all
  three purpose inputs reflected in generated `backend/AGENTS.md`, accepted
  cross-language fixture parity from read-only execution of the shared raw-
  observation vectors, exact `t` framing, single-round millisecond/carry and
  native-byte behavior with no duplicated expected values, intent-to-add status,
  empty cached-content diff, v1 hashes, final-size math, and integrated gate;
  require `PASS`
- **Exit gate:** direct tests cover all named failure paths and prove the
  output-superset/client-subset contract; exact delta is the two new Python files,
  latent `embed.go` edit including its two-version purpose comment, and generated
  `backend/AGENTS.md` with both new entries and the refreshed embed entry;
  emitted envelopes match every read-only shared raw-observation vector byte-for-
  byte in exact `t`, `ts`, `msg` order, with exactly three digits after the relay
  encoder rounds its raw clock observation once; no relay-local rounding
  expectation is duplicated; every v2 control uses `t`, and no v2 `type` alias
  exists; logical native null uses the existing bounded diagnostic and never emits
  `msg:null`; final encoded size is enforced before emission without a zero-copy
  producer requirement; intent-to-add entries are
  explicitly accounted for with no staged content; v1 hashes match the baseline;
  and no production path references the v2 embed
- **Handoff:** report test cases, exact discriminator audit, shared fixture hash
  and read-only raw-observation vector results, single-round/carry evidence with no
  duplicated expectations, native-byte/null-diagnostic behavior, final-size-bound
  proof, producer emission mechanics, v1 hashes, exact four-file delta/diffstat,
  the
  three purpose-input changes and generated index entries, generated-index replay
  command/result, intent-to-add entries, `git diff --cached --exit-code` result,
  user-owned index cleanup status, commands, and remaining relay risks

### Phase 3: Build one-pass persistent v2 snapshots

- **Stable ID:** `persistent-read-paths`
- **Responsible:** one phase-executor subagent
- **Depends on:** `v2-fast-record-reader`
- **May run with:** `relay-v2` in an isolated worktree
- **Base state:** accepted stable phase `v2-fast-record-reader` and the accepted
  parsed-message/v1 adoption prerequisites are integrated; all rollout changes
  remain unshipped and the worktree is clean. Immediately before dispatch,
  resolve actual HEAD, `origin/main`, and any configured upstream into the
  ephemeral manifest; capture the loader/export/replay entry-point inventory,
  accepted v1 pass-count evidence and immutable source hashes, current v2 parse
  call graph, and physical identity fields available on every plain/zstd path
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; no fixture
  recording or regeneration without coordinator approval
- **Write scope:** `backend/internal/task` plain/zstd full, tail, session, message,
  inventory, and reopen loaders; `backend/internal/agent/export.go`; replay input
  adapters; natural package-private snapshot plumbing; and adjacent tests
- **Data authority:** first-header version/harness and the physical file remain
  authoritative. `ValidatedLogSnapshot` (or an equally clear package-private
  name) is an in-memory derived value bound to device, inode, size, mtime, and a
  validated EOF for the exact observed file; it may carry parsed authority,
  controls, session state, semantic messages, and pending-action identities but
  is never persisted or accepted for a different physical identity
- **Purpose:** make v2 persistent adoption parse authority, state, and messages in
  one semantic file pass while preserving the accepted v1 compatibility path
- **Deliverables:** migrate every task-layer plain/zstd first-header and later-
  segment scanner to validate the closed version and then discriminate exact v1
  `type:"caic_meta"` from exact v2 `t:"caic_meta"`, with no v2 `type` alias.
  V2 plain/full/adoption reads use the strict fast reader to validate authority
  and later headers, apply `t` controls/session/model state, parse every native
  record exactly once, collect semantic messages, validate EOF, and return one
  matching snapshot from the same pass; reopen consumers can receive that
  snapshot without rescanning. Compressed, tail, export, and replay paths dispatch
  exactly by authoritative version and cannot invent authority from a fragment.
  Parsed consumers explicitly unwrap `.Message` and retain the exact already-
  rounded millisecond `ProducerTime` to the conversion boundary without reader
  rounding. V1 retains its accepted compatibility implementation and maximum of
  three complete passes plus bounded tail
- **Generated artifacts:** no committed artifacts; taskmeta/replay/cache tests use
  temporary directories, existing golden recordings are read-only, and no
  benchmark/profile artifact enters the repository
- **Change budget:** at most 8 production files and 6 tests; fixture additions are
  limited to one minimal exact-`type` v1 and one canonical exact-`t`, three-digit-
  timestamp v2 file; no public API or duplicate scanner framework
- **Boundary:** live relay and writers stay unchanged; `.taskmeta.json` remains a
  rebuildable cache; the validated snapshot is not serialized; harness parser
  caic cases remain until cleanup; do not weaken same-inode, mixed/later-header,
  truncation, scanner-size, UTF-8, or fail-closed behavior, and do not regress the
  accepted v1 three-complete-pass-plus-bounded-tail maximum
- **Decision checkpoints:** stop for any persisted snapshot/second authority,
  inability to bind proof to device/inode/size/mtime/EOF, skipped corruption,
  fixture rewrite, duplicate authority scanner, extra v2 semantic parse/pass, or
  need to alter the accepted immutable v1 benchmark sources
- **Validation intent:** equivalent v1/v2 semantic full/tail/export/replay output;
  zero v1 and exact millisecond v2 producer time; exact v1 `type` and v2 `t`
  bootstrap/later-segment authority; v2 `t` controls, session/model state, native
  messages, pending-action identities, and EOF validated in one semantic pass;
  one native parse per agent; compressed parity; taskmeta hit/miss/stale/corrupt
  behavior. Exact failure covers identity mismatch/replacement, changed segment,
  truncation, missing EOF proof, unknown version/token, v1 `t`, v2 `type` on meta/
  control/agent, duplicate/both discriminators, bare-v2, noncanonical envelope,
  `0.000`, integer/no fraction, one/two/four-or-more fraction digits, signs,
  exponents, leading zeros, overflow/out-of-range time, corrupt UTF-8, and tail
  without authority; readers never round timestamps. V1 counting-reader tests
  enforce compatibility-only non-regression; v1 timing remains advisory
- **Validation commands:** cwd `/home/user/src/caic`: verify accepted immutable v1
  benchmark hashes; run focused snapshot/identity/pass-count/corruption tests and
  `go test ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/...`; audit every persistent entry point and
  snapshot field/lifetime; rerun focused v1 counting-reader tests for the accepted
  three-pass-plus-tail ceiling without modifying benchmark sources; audit task-
  layer readers/tests/fixtures with the global stale-format audit, preserving v1
  `type` and nested native payload keys exactly as rule 8 permits;
  run `make lint-fix`, `make lint-docs`, `git diff --check`, `git diff --cached
  --exit-code`, `git status --short`, and `git ls-files --others
  --exclude-standard`
- **Review:** a fresh authority/performance reviewer receives the integrated
  target and exact pre-integration base from fresh ephemeral state, entry-point
  inventory, one-pass v2 instrumentation, snapshot identity/lifetime proof,
  corruption matrix, v1 compatibility evidence, exact fixture delta, and gate;
  require `PASS`
- **Exit gate:** every persistent path dispatches by exact file authority; exact
  v2 app/taskmgr adoption through validated EOF produces a matching in-memory
  snapshot in one semantic file pass with one native parse per agent record and
  no persisted second authority; every bootstrap/discriminator/timestamp mismatch
  or corruption case fails closed; v1 correctness and the accepted three-
  complete-pass-plus-bounded-tail maximum do not regress; only the allowed
  fixture/filesystem delta exists
- **Handoff:** list migrated entry points, task-layer bootstrap split, snapshot
  type/fields/lifetime and consumers, exact-millisecond v2 pass/native-parse
  evidence, pending-action derivation, fixture hashes, stale-format audits, v1
  compatibility result, corruption/error matrix, exact files, validation,
  cleanup, and risks

### Phase 4: Route live and startup relay reads

- **Stable ID:** `live-relay-read-path`
- **Responsible:** one phase-executor subagent
- **Depends on:** `relay-v2`, `v2-fast-record-reader`
- **May run with:** none due to overlap across agent launch/read paths
- **Base state:** accepted stable phases `v2-fast-record-reader` and `relay-v2`
  plus accepted parsed-message metadata are integrated, all rollout changes
  remain unshipped, and the worktree is
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
  normal-live log-once behavior; v2 agent records use the accepted strict `t`
  fast reader with no alternate envelope decoding or `type` alias; semantic
  consumers receive parsed values and explicitly unwrap `.Message`; every v2
  envelope result retains its exact immutable millisecond `ProducerTime` without
  reader rounding; v2 native-payload unwrap for handshakes; Pi startup
  persistence; unpersisted Codex/OpenCode handshakes; `t` control routing;
  physical offsets; attach-overlap deduplication; common script deployment/
  selection seam used by custom launchers; v1 byte preservation
- **Generated artifacts:** none
- **Change budget:** at most 10 production files and 8 tests; no protocol DTO or
  handshake sequence changes
- **Boundary:** selected version remains v1 in production; parsed metadata must
  not implement or be inserted as `Message`; no alternate v2 envelope parser,
  header bump, v2 deployment, sink migration, or harness-parser cleanup; v1
  bytes and accepted three-pass-plus-tail adoption behavior do not regress
- **Decision checkpoints:** stop if a handshake must receive a caic control as
  native data, if a consumer cannot state its persistence policy, if logging
  twice appears necessary, or if offset semantics would change for v1
- **Validation intent:** split reads, interleaved controls, handshake buffering,
  non-zero exits, attach physical offsets, relay-stdin overlap deduplication, no
  live stdin double write, v1 exact logged bytes and zero producer time, v2
  physical-byte counts, exact canonical three-digit encoded `ts`, and decoded
  `ProducerTime` exactly equal to that timestamp with Unix nanoseconds divisible
  by `1_000_000`, without reader rounding or a semantic timestamp event. Require
  strict rejection of v2 `type` records and every noncanonical timestamp form, Pi
  startup persistence, Codex/
  OpenCode handshake non-persistence, and all three startup paths succeeding with
  synthetic streams.
  Verify the immutable accepted v1 benchmark sources and run focused v1 counting-
  reader compatibility tests proving the accepted maximum of three complete
  passes plus bounded tail; v1 timing remains advisory
- **Validation commands:** cwd `/home/user/src/caic`: verify the immutable accepted
  v1 benchmark-source hashes; run focused v1 counting-reader compatibility tests,
  `go test ./backend/internal/agent/... ./backend/internal/task/...`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; `make lint-fix`; `make
  lint-docs`; `git diff --check`; `git diff --cached --exit-code`; `git status
  --short`; `git ls-files --others --exclude-standard`; audit live-reader tests
  with the global stale-format audit while preserving v1 and native payload usage
  exactly as rule 8 permits; any unexpected status or untracked
  output fails the gate
- **Review:** independent concurrency/protocol reviewer inspects the integrated
  reader, all custom launch paths, offsets, log-once evidence, exact immutable
  millisecond `ProducerTime` preservation, proof that the reader never rounds,
  strict `t`-only v2 handling and rejection of every noncanonical timestamp,
  immutable v1 benchmark-source verification, and focused counting-reader
  evidence for the accepted three-complete-pass-plus-bounded-tail ceiling;
  require `PASS`
- **Exit gate:** entry-point tests prove v1 byte identity; strict `t`-only latent
  v2 decoding; exact preservation of the encoded millisecond as immutable
  `ProducerTime`; no reader rounding; rejection of v2 `type` and every
  noncanonical timestamp; current per-harness persistence policy; and no duplicate
  stdin across live and attach paths. Immutable v1 benchmark-source hashes match
  and focused counting-reader compatibility evidence proves the accepted three-
  complete-pass-plus-bounded-tail ceiling does not regress; no production task
  selects v2
- **Handoff:** report migrated call graph, strict `t`-only and noncanonical-
  timestamp rejection results, exact immutable millisecond `ProducerTime` and no-
  reader-rounding proof, physical-offset/log-once evidence, protocol and stale-
  format audit results, immutable v1 benchmark-source verification, focused
  counting-reader evidence for the accepted three-complete-pass-plus-bounded-tail
  ceiling, files, commands, and residual risks

### Phase 5: Make harness parsers native-only

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
  point; Codex native duration remains. Focused v1 counting-reader tests enforce
  compatibility-only correctness and the accepted maximum of three complete
  passes plus bounded tail; timing remains advisory.
- **Validation commands:** cwd `/home/user/src/caic`: verify the immutable accepted
  v1 benchmark-source hashes; run focused v1 compatibility counting tests, `go
  test ./backend/internal/agent/... ./backend/internal/task/...`; `make
  lint-fix`; `make lint-docs`; `git diff --check`; `git diff --cached
  --exit-code`; `git status --short`; `git ls-files --others
  --exclude-standard`
- **Review:** fresh harness-focused reviewer checks for remaining `caic_` parser
  branches and semantic regressions on integrated target; require `PASS`
- **Exit gate:** repository audit finds no caic routing in native parsers, all
  four golden suites pass without recording changes, and focused counting-reader
  evidence proves v1 correctness and the accepted three-complete-pass-plus-
  bounded-tail maximum do not regress
- **Handoff:** provide audit command/result, per-harness tests, deletions/delta,
  validation, and unresolved risks

### Phase 6: Make producer-time metadata and replay caches version-correct

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
- **Data authority:** `ParsedMessage.ProducerTime` copied from `agent.ts` is the
  immutable producer observation already rounded to the nearest millisecond;
  readers and consumers never re-round it; conversion clock is derived; replay
  sidecars remain rebuildable caches
- **Purpose:** consume exact millisecond-precision v2 producer-time metadata
  without changing v1 files or exposing a timestamp event
- **Deliverables:** explicit `.Message` unwrapping; metadata-transparent
  conversion and compaction; stable millisecond-precision tool-duration replay;
  fallback for zero producer time; timing-relevance gating owned only by
  `ToolTimingTracker`; `CacheVersion` bump; pure-v1/v2 parity fixtures with exact
  `type`/`t` bootstrap and canonical three-digit timestamps
- **Generated artifacts:** no committed generated sidecars; tests generate in
  temporary directories only
- **Change budget:** at most 8 production files and 7 tests; two minimal raw-log
  fixtures maximum; one intentional cache constant change
- **Boundary:** no parser-side timing relevance, metadata-to-message adapter, API
  DTO field, v1 timestamp retrofit, relay change, or producer cutover
- **Decision checkpoints:** stop if producer time must be persisted twice, becomes
  a semantic/user-visible event, is lost before `ToolTimingTracker`, or changes
  native harness duration precedence
- **Validation intent:** exact millisecond v2 tool spans; first-producer-time
  fallback; native duration precedence; zero-time v1 safe fallback; tracker
  ignores metadata on timing-irrelevant messages while semantic consumers still
  receive those messages; accepted `ProducerTime` values remain byte-derived and
  unchanged rather than normalized or re-rounded; producer metadata is
  transparent to deltas, control searches, exports, and the measured full-message
  restore path; stale cache rejection and regeneration. Fixtures and replay tests
  reject v2 `type` and noncanonical timestamp forms and retain exact `t` plus
  three-digit form. Focused v1 counting-reader tests enforce compatibility-only
  correctness and the accepted maximum of three complete passes plus bounded
  tail; timing remains advisory.
- **Validation commands:** cwd `/home/user/src/caic`: verify immutable accepted v1
  benchmark-source hashes; run focused v1 compatibility counting tests, `go test
  ./backend/internal/server/api/v1conv/... ./backend/internal/eventreplay/...
  ./backend/internal/server/... ./backend/internal/task/...`; audit replay/timing
  fixtures and tests with the global stale-format audit while preserving v1 and
  native payload usage exactly as rule 8 permits; `make lint-fix`; `make lint-docs`;
  `git diff --check`; `git diff --cached --exit-code`; `git status --short`;
  `git ls-files --others --exclude-standard`
- **Review:** fresh timing/replay reviewer checks integrated semantics, cache bump,
  and fixture delta; require `PASS`
- **Exit gate:** v1/v2 fixture semantic events are equivalent; v1 parsed values
  carry zero time, v2 parsed values carry the exact immutable rounded millisecond
  producer observation, and only millisecond-precision downstream durations may
  differ; no reader/consumer re-rounding occurs; cache rebuild tests and stale-
  format audits pass; v1 correctness and its accepted three-complete-pass-plus-
  bounded-tail maximum do not regress; no sidecar or benchmark artifact is
  committed
- **Handoff:** report millisecond timing cases and precision proof, old/new cache
  version, fixture hashes/discriminators, stale-format audits, filesystem delta,
  commands, and risks

### Phase 7: Introduce the enforcing versioned log sink

- **Stable ID:** `versioned-log-sink`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`
- **May run with:** none
- **Base state:** accepted stable phase `persistent-read-paths` is integrated,
  all rollout changes remain unshipped, and the worktree is clean. Immediately
  before dispatch, resolve actual HEAD, `origin/main`, and any configured
  upstream into the ephemeral manifest; capture the same-inode append
  constructor, direct `LogW.Write` inventory, validated-snapshot API/identity
  fields, accepted shared byte fixture hash, and v1 compatibility evidence
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push
- **Write scope:** versioned sink API/implementation in `backend/internal/agent`
  and `backend/internal/task`, minimum reopen constructor plumbing, adjacent
  focused tests, and read-only use of the accepted canonical byte fixture;
  callers otherwise remain v1-compatible
- **Data authority:** extend rather than replace the accepted same-inode append
  constructor. A v2 reopen sink consumes a matching non-persisted
  `ValidatedLogSnapshot` and verifies device/inode/size/mtime plus validated EOF
  against the opened append target in O(1); the physical first header remains the
  authority. For native writes the caller supplies its raw `time.Time` producer
  observation unchanged; `VersionedLogSink` is the sole backend physical encoder
  and rounding/formatting authority. The sink owns no persisted version, snapshot,
  or duplicate identity index
- **Purpose:** create one enforcing append policy, prove constant-time v2 reopen
  identity, and lift valid-v2 rejection only for typed writes with matching proof
- **Deliverables:** typed native/control operations; byte-identical v1 encoding;
  byte-exact canonical v2 `t`, `ts`, `msg` encoding matching the shared Go/Python
  fixture; every v2 physical control uses `t`, with no `type` alias; accept the
  raw `time.Time` producer observation and round it exactly once inside the sink
  to nearest millisecond via `(ns + 500_000) / 1_000_000`, ties upward with second
  carry, then canonically emit exactly three fractional digits for every backend-
  written v2 agent record. The sink API does not accept an already-rounded
  timestamp, and backend callers must not pre-round. Sink tests consume the shared
  raw-observation encoder vectors read-only as their sole expected timestamps and
  bytes; native object/array/string/number/boolean bytes are preserved without
  semantic reserialization; native logical JSON null is converted
  to the existing bounded diagnostic string rather than `msg:null`; final encoded
  size computed and enforced before emission, with necessary output buffers,
  copies, or repeated writes permitted; strict v2 controls; mixed-token refusal;
  valid-v2 append only through typed operations; O(1) reopen proof verification
  before append; missing proof, changed identity/size/mtime/EOF, replacement,
  truncation, or mismatch fails closed or invokes only a separately named
  explicitly validated fallback; pending-action guard uses identities derived in
  the supplied matching snapshot; serialized writes and explicit close/error
  behavior
- **Generated artifacts:** none; tests use temporary files and accepted fixtures,
  and no profile, benchmark, or log artifact enters the repository
- **Change budget:** at most 7 production files and 6 tests; no direct-writer
  migration beyond minimum constructor/reopen plumbing and no fixture mutation
- **Boundary:** all production callers still select v1; untyped raw append keeps
  rejecting valid v2; backend callers pass raw observations and may not pre-round.
  Do not weaken same-inode authority, persist snapshot proof, rescan v2 on the
  matching fast path, accept stale/missing proof, remove raw writer paths, bump
  headers, select relays, change semantic token values, add v2 `type`
  compatibility, duplicate shared fixture rounding expectations, or change frozen
  v1 bytes; preserve the v1 three-complete-pass-plus-bounded-tail maximum
- **Decision checkpoints:** stop if same-inode authority would be bypassed, if O(1)
  identity cannot distinguish changed EOF, if fallback validation is implicit,
  if v2 append becomes possible through an untyped writer, if snapshot/version/
  pending identities would be persisted independently, if the sink cannot accept
  raw `time.Time` observations and own the only backend rounding step, or if v1
  bytes differ
- **Validation intent:** byte-for-byte v1 native/control writes; strict `t`
  controls; read-only execution of every shared raw-observation encoder vector,
  including below-half, exact-half, above-half, and next-second carry, with no
  duplicated expected timestamp or byte literal. Prove the sink receives raw
  `time.Time`, rounds exactly once, and emits the fixture's canonical three-digit
  record; zero, negative, overflow, and out-of-range producer observations are
  rejected before emission; no caller pre-rounding or v2 `type` path exists.
  Preserve native object/array/string/number/boolean bytes; native logical JSON
  null emits the existing bounded diagnostic
  and never `msg:null`; final-size rejection occurs before emission; matching
  snapshot permits reopen/append without a full scan; missing proof and every
  device/inode/size/mtime/EOF mismatch or corruption fails before write; any
  explicit fallback fully revalidates authority and EOF; native source predating
  reopen suppresses an equivalent pending-action snapshot; untyped v2/mixed
  writes fail; same-inode, concurrent append, close/error, and v1 compatibility
  tests stay green
- **Validation commands:** cwd `/home/user/src/caic`: run focused snapshot identity,
  reopen, fallback, byte-fixture, mixed-write, concurrency, and corruption tests;
  run `go test ./backend/internal/agent/... ./backend/internal/task/...`; audit
  constructor call sites and prove matching-v2 reopen has no complete scan;
  rerun focused accepted v1 counting-reader compatibility tests without changing
  immutable benchmark sources; run the global stale-format audit over sink tests/
  fixtures while preserving v1 and native payload keys exactly as rule 8 permits;
  run `make lint-fix`, `make lint-docs`, `git diff --check`, `git diff --cached
  --exit-code`, `git status --short`, and `git ls-files --others
  --exclude-standard`
- **Review:** an independent API/authority reviewer receives the integrated target
  and pre-integration base from fresh ephemeral state, snapshot/sink contract,
  O(1) call-count proof, exact mismatch matrix, explicit fallback if any, read-
  only shared raw-observation fixture parity, raw-`time.Time` sink ownership and
  exactly-once rounding proof, absence of caller pre-rounding or duplicated
  expectations, writer inventory, v1 evidence, artifact delta, and gate; require
  `PASS`
- **Exit gate:** a matching validated snapshot enables O(1) physical identity and
  EOF verification before typed v2 reopen/append; missing/stale/mismatched proof
  fails closed or follows only the fully tested explicit validation fallback;
  same-inode authority is unchanged; sink tests consume all shared raw-observation
  vectors read-only and prove v1 byte identity, raw-`time.Time` input, sink-owned
  exactly-once rounding and canonical three-digit typed-only v2 `t` encoding with
  no caller pre-rounding or duplicated expectations, preserved native bytes, null
  diagnostics instead of `msg:null`, final-size enforcement before emission,
  untyped-v2 and v2-`type` rejection, and restart-safe pending-action behavior; v1
  correctness and three-pass-plus-tail compatibility do not regress
- **Handoff:** report constructor/reopen signature, snapshot match fields and
  lifetime, syscall/read-count proof, mismatch/fallback results, direct-write
  inventory, shared fixture hash and read-only vector results, v1/v2 discriminator/
  native/null/size behavior, raw-`time.Time` API contract, sink-owned exactly-once
  rounding/canonical formatting proof, caller no-pre-round audit, stale-format
  audit, exact files, commands, cleanup, and remaining migration sites

### Phase 8: Gate v2-primary adoption performance

- **Stable ID:** `v2-adoption-performance`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-read-paths`, `versioned-log-sink`
- **May run with:** none
- **Base state:** accepted stable phases `persistent-read-paths` and
  `versioned-log-sink` are integrated; the fast reader and matching snapshot/sink
  contracts are fixed, all rollout changes remain unshipped, and the worktree is
  clean. Immediately before dispatch, resolve actual HEAD, `origin/main`, and any
  configured upstream into the ephemeral manifest; capture exact app/taskmgr v2
  adoption/reopen call graphs, accepted fixture and v1 benchmark hashes, current
  pass/native-callback/reader-extraction-copy/unmarshal behavior, sensitive-file
  statistics, and an absolute external artifact directory outside the repository
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push. The coordinator
  must obtain a user-owned intent-to-add checkpoint for exactly
  `backend/internal/task/load_v2_benchmark_test.go`,
  `backend/internal/task/load_v2_benchmark_cache_linux_test.go`, and
  `backend/internal/task/load_v2_benchmark_cache_other_test.go` before generated-
  index replay; no staged content is permitted
- **Write scope:** the exact three new build-tagged benchmark/helper sources;
  focused natural-boundary counting/callback tests under
  `backend/internal/task`; exact v2 app/taskmgr adoption and validated-reopen fast
  paths under `backend/internal/task` and `backend/internal/task/taskmgr`; trace/
  profile analysis only outside the repository; and generated `backend/AGENTS.md`
- **Data authority:** first-header version/harness and physical file remain the
  sole authority; `ValidatedLogSnapshot` is a non-persisted identity-bound
  derivation; benchmark counters and profiles are evidence only and cannot alter
  production behavior
- **Purpose:** prove the exact combined v2-primary live-adoption path is a single
  semantic pass through validated reopen before direct writers or cutover can use
  it
- **Deliverables:** a new realistic canonical 1 GiB v2-primary benchmark whose
  generated physical records use only `t` and canonical positive three-digit
  millisecond timestamps, for the exact app/taskmgr sequence including authority/
  state/message restoration and matching reopen; warm and supported Linux-cold
  modes; deterministic natural-boundary counters for complete passes, logical
  bytes, native callbacks, fast-reader/adoption envelope-extraction copies, and
  outer unmarshal calls; production fast-path corrections
  within scope; CPU profile/runtime-trace analysis; unchanged accepted v1
  benchmark sources and compatibility behavior
- **Measurement boundary:** one combined v2 iteration must have exactly one
  reader that consumes the complete semantic file through validated EOF,
  including all authority, controls, session state, and native messages; matching
  reopen consumes zero task-log payload bytes and uses O(1) identity checks.
  Total logical bytes are at most fixture length plus an exact aggregate bounded-
  read allowance encoded and explained in the frozen benchmark before baseline;
  each allowance is independent of fixture/payload size and no undeclared reader
  is permitted. There is exactly one native semantic callback per `agent` record,
  no payload-sized outer-envelope copy during fast-reader/adoption extraction,
  and no outer-agent `encoding/json.Unmarshal`. These structural metrics are hard
  gates only for the reader/adoption path. Producer emission is outside this copy
  gate and may use necessary output buffers, copies, or repeated writes while
  preserving native bytes and enforcing final encoded size before emission.
  Timing, throughput, allocations, `rchar`, `read_bytes`, and physical-read
  metrics are advisory
- **Benchmark checkpoint:** begin with only the three new benchmark/helper files
  and generated index delta. After user-owned intent-to-add, run `make lint-fix`,
  prove zero production-file diff, review the declared bounded-read inventory,
  and record a SHA-256 manifest over all three sources externally. Freeze those
  bytes before baseline; after output is invalid unless the manifest still
  matches. Only after the baseline may production optimization start. The three
  accepted v1 benchmark sources are read-only throughout
- **Generated artifacts:** the main benchmark uses `//go:build
  v2_adoption_benchmark`; Linux/non-Linux helpers use `//go:build
  v2_adoption_benchmark && linux` and `//go:build v2_adoption_benchmark &&
  !linux`. Each has the repository-required first-line purpose comment/build-tag
  layout. After intent-to-add, `make lint-fix` from `/home/user/src/caic`
  regenerates only their three entries in `backend/AGENTS.md`. Fixtures live in
  `b.TempDir`. Results, profiles, traces, test binaries, host/resource metadata,
  `/proc/self/io`, hash manifests, and benchstat output stay in the external
  artifact directory; transient copies are removed after review and all retained
  performance evidence is removed at rollout completion
- **Change budget:** exactly three new benchmark/helper sources, generated three-
  entry `backend/AGENTS.md` delta, at most 4 production files and 4 existing/new
  focused tests in the declared task/taskmgr scope; no relay, parser, sink API,
  accepted fixture, v1 benchmark, API DTO, dependency, or wire-format change
- **Boundary:** preserve v1 correctness/fail-closed behavior and the accepted
  three-complete-pass-plus-bounded-tail maximum; do not change any accepted v1
  benchmark source, weaken same-inode/snapshot/EOF authority, persist a second
  authority, introduce a test-only production hook, broaden the fast reader, or
  treat advisory timing as a hard target
- **Resource and comparison protocol:** `CAIC_V2_ADOPTION_BENCH_BYTES` has a hard
  4 GiB maximum and the canonical gate is exactly 1 GiB. Require at least 3 GiB
  free fixture storage and 6 GiB available RAM; failure blocks rather than
  shrinking the gate. Reuse one fixture and never create concurrent copies.
  Before comparison prove a warmed module cache or network access, enabled Go
  checksum database, and checksum-bearing metadata for
  `golang.org/x/perf@v0.0.0-20260709024250-82a0b07e230d`; only that pinned
  benchstat version is allowed. This exact dependency pseudo-version is a durable
  tool identity, not a recorded repository execution-base SHA, and remains pinned
  for reproducibility. Baseline/current use the same host, Go runtime,
  power/load controls, and frozen sources. Linux cold mode uses per-fixture
  `FADV_DONTNEED`; refusal is unsupported/advisory, non-Linux reports unsupported,
  and neither uses root or global `drop_caches`
- **Decision checkpoints:** stop for resource failure, changed source hash,
  unsupported hard instrumentation, a second complete semantic reader, any
  payload-scaled auxiliary read or fast-reader/adoption envelope-extraction copy,
  inability to prove native callback or outer-unmarshal counts, a snapshot
  mismatch that does not fail closed, or an optimization requiring a new
  authority/public seam; producer output buffering/copying is not grounds to stop;
  benchmark-only work cannot pass
- **Validation intent:** realistic canonical v2 mix includes small deltas, normal
  events, nested/escaped/native-whitespace values, `t` controls/session/model
  state, large tool output, diagnostics, repeated exact-`t` matching headers, and
  varied exact three-digit millisecond timestamps; exact combined app/taskmgr
  adoption including reopen has one complete semantic pass, only the frozen
  bounded reads, one native callback per agent, no payload-sized fast-reader/
  adoption envelope-extraction copy, and no outer agent unmarshal. Every identity
  mismatch, replacement, truncation, EOF failure, corrupt envelope/control, v2
  `type`, unknown/duplicate/reordered field/token, `0.000`, integer/no fraction,
  one/two/four-or-more fraction digits, sign, exponent, leading zero, overflow/
  out-of-range time, UTF-8 failure, and oversize record fails closed without
  reader rounding; plain/compressed behavior remains bounded and v1 compatibility
  tests remain green
- **Validation commands:** cwd `/home/user/src/caic`: preflight external path,
  disk/RAM, Go checksum database, and pinned module with `go mod download -json`;
  after intent-to-add run `make lint-fix`, verify the exact three-entry generated
  delta and empty cached diff, and freeze/verify source hashes. With
  `CAIC_V2_ADOPTION_BENCH_BYTES=$((1024*1024*1024))`, run `go test -tags
  v2_adoption_benchmark ./backend/internal/task -run '^$' -bench
  '^BenchmarkV2TaskAdoption' -benchmem -benchtime=1x -count=3` before and after;
  compare with pinned benchstat; capture/inspect CPU profile and runtime trace;
  cross-compile tagged test binaries for current-architecture Linux and Darwin
  into the external directory, then remove them; run focused deterministic and
  corruption tests, `go test ./backend/internal/agent/...
  ./backend/internal/task/...`, accepted v1 compatibility counting tests; audit
  benchmark generators, fixtures, tests, and output samples with the global stale-
  format audit while preserving v1 and nested native payload keys exactly as rule
  8 permits; run `make lint-fix`, replay the v2 benchmark, `make lint-docs`,
  `git diff --check`, `git diff --cached --exit-code`, `git status --short`, and
  `git ls-files --others --exclude-standard`
- **Review:** a fresh performance/authority reviewer receives the integrated
  target and exact pre-integration base from fresh ephemeral state, exact call
  graph, fixture mix, frozen source/bounded-read manifest, hashed 1 GiB warm/cold
  before/after evidence, deterministic pass/byte/callback/reader-extraction-copy/
  unmarshal counts, profile/trace analysis, mismatch/corruption matrix, cross-
  platform evidence,
  v1 compatibility, artifact delta, cleanup, and gate; require `PASS`
- **Exit gate:** the exact combined v2 app/taskmgr live-adoption path including
  matching reopen completes one semantic file pass, reads approximately 1x
  logical bytes plus only the frozen declared bounded allowance, invokes one
  native parse per agent record, makes no payload-sized fast-reader/adoption
  envelope-extraction copy, and makes no outer-agent `encoding/json.Unmarshal`;
  every identity mismatch/corruption
  case fails closed; v1 compatibility does not regress; timing/allocation/
  physical metrics are reported only as advisory; hashes, generated delta,
  intent-to-add state, cross-platform checks, and external cleanup all match the
  contract
- **Handoff:** report exact operation/caller sequence, fixture composition/size,
  source and evidence hashes, bounded-read inventory, resource/host controls,
  before/after/profile/trace commands, deterministic and advisory metrics,
  mismatch matrix, old/new call graph, v1 evidence, exact files/generated/index
  state, cleanup, residual host noise, and risks

**Carry-forward v2 performance contract:** every later phase that touches v2
read, reopen, append, direct-write, cutover, or runtime adoption reruns the
focused deterministic gate: one complete semantic pass through matching reopen,
fixture length plus only the frozen bounded-read allowance, one native parse per
agent record, no payload-sized fast-reader/adoption envelope-extraction copy, and
no outer-agent `encoding/json.Unmarshal`. Its fixtures and samples use exact v2
`t` discrimination and canonical positive three-digit millisecond timestamps;
v2 `type` and every noncanonical timestamp fail closed without reader rounding.
This copy gate applies only to reader/adoption extraction, never producer
emission. Identity mismatch and corruption always fail closed. The three v2
benchmark sources and three accepted v1 benchmark sources remain
immutable. Timing, allocation, and physical-read evidence stays advisory. Each
phase records source/evidence hashes and an absolute external artifact directory,
uses `-tags v2_adoption_benchmark` for any v2 benchmark invocation, observes the
same resource/cross-platform/artifact-cleanup rules, and never commits a
performance artifact. V1 carry-forward is compatibility-only: correctness,
fail-closed behavior, and at most three complete passes plus bounded tail.

### Phase 9: Migrate every direct log writer

- **Stable ID:** `agent-direct-write-migration`
- **Responsible:** one phase-executor subagent
- **Depends on:** `versioned-log-sink`, `live-relay-read-path`,
  `pure-harness-parsers`, `v2-adoption-performance`
- **May run with:** none; it crosses all agent backends and task writers
- **Base state:** accepted stable phases `versioned-log-sink`,
  `live-relay-read-path`, `pure-harness-parsers`, and
  `v2-adoption-performance` are integrated, all rollout changes remain unshipped,
  and the worktree is clean. Immediately before
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
  reopen, a matching non-persisted validated snapshot carrying derived pending-
  action identities. A direct-writer caller captures its raw `time.Time` producer
  observation and passes it unchanged to the sink; only the sink rounds and
  canonically formats backend-written v2 records. No caller chooses vocabulary,
  pre-rounds time, manually wraps native data, persists snapshot proof, or rebuilds
  that set from process-local memory
- **Purpose:** eliminate bypasses before v2 can be enabled
- **Deliverables:** migrate the full inventory, including the accepted
  `Task.WriteToLog` path-based fallback and every production physical entry
  point; v1 `type` bytes stay identical; latent v2 direct native records use
  exact `t:"agent"` envelopes and every v2 control uses only `t` with the
  approved unchanged semantic token. Direct-writer callers capture raw
  `time.Time` producer observations and pass them unchanged to `VersionedLogSink`;
  callers never pre-round, and the sink owns the sole rounding plus canonical
  three-digit formatting step. Context reset is top-level
  `t:"context_cleared"`; live prompt/compact/`SendRaw` persistence
  is sink-exclusive; Pi startup persistence is retained while Codex/OpenCode
  handshakes remain unpersisted; pending-action snapshots obey the exactly-once
  rule; no raw task-log writer escapes
- **Generated artifacts:** none; existing traces remain unchanged
- **Change budget:** cross-cutting but limited to inventoried write sites and
  tests; no unrelated backend refactor; every added file must map to an inventory
  row
- **Boundary:** new headers and selected relays remain v1; no cutover, v2 `type`
  compatibility, caller-side timestamp rounding, semantic token change, or API
  change; relay v1 files remain frozen
- **Decision checkpoints:** stop for any writer without clear native/control
  provenance, any caller that cannot pass its raw `time.Time` observation
  unchanged to the sink, any v1 byte change, or any need to expose sink internals
  publicly
- **Validation intent:** per-writer v1 golden bytes and exact `t`-only v2 latent
  encoding with canonical three-digit timestamps. Call-contract tests prove each
  direct writer passes its unmodified raw `time.Time` observation to the sink and
  never pre-rounds; read-only shared rounding-vector evidence from the sink proves
  below-half, exact-half, above-half, and second-carry encoding without duplicated
  expected values. Zero/negative/overflow/out-of-range observations fail before
  emission, and no v2 `type` route exists. `Task.WriteToLog` with and without a
  live handle uses the sink and retains same-inode authority; persisted Pi startup
  traffic logs once; Codex/OpenCode
  handshake requests and responses do not enter task logs; prompt/compact/
  `SendRaw` framing has no relay duplicate; native/top-level pending actions
  deduplicate by semantic identity; model/session state and context/trailer/
  provisioning behavior remain correct; an inventory audit finds no physical
  reader or append bypass. The carry-forward v2 gate proves one complete semantic
  pass through matching reopen, only frozen bounded reads, one native parse per
  agent, no payload-sized fast-reader/adoption envelope-extraction copy, and no
  outer agent unmarshal; it imposes no zero-copy requirement on sink emission.
  Identity mismatch/corruption fails closed. V1 stays compatibility-only.
- **Validation commands:** cwd `/home/user/src/caic`: verify immutable v1/v2
  benchmark-source and evidence hashes plus the external artifact path; run the
  focused carry-forward v2 deterministic/corruption/reopen tests and resource-
  qualified canonical 1 GiB v2 benchmark with the frozen tag/benchstat protocol;
  run focused v1 compatibility counting tests, `go test
  ./backend/internal/agent/... ./backend/internal/task/...`, and `python3
  backend/internal/agent/relay/test_relay.py`; hash external results and clean
  transient artifacts; run `make lint-fix`, `make lint-docs`, `git diff
  --check`, `git diff --cached --exit-code`, `git status --short`, `git ls-files
  --others --exclude-standard`; audit direct `.Write` calls against an allowlist
  and run the global stale-format audit over all migrated writers/tests while
  preserving v1 and native payload keys exactly as rule 8 permits
- **Review:** fresh cross-backend reviewer receives inventory before/after,
  integrated SHA, v1 trace hashes, per-writer exact `t`-only latent byte evidence,
  raw-observation-to-sink call-contract and no-caller-pre-round audit, canonical
  three-digit output, read-only shared rounding-vector results, and gate; require
  `PASS`
- **Exit gate:** every task-log write, including both `Task.WriteToLog` branches,
  flows through the versioned sink or version-aware relay reader under an
  explicit persistence policy; a full production physical entry-point inventory
  has no authority/parser/append bypass. Every migrated direct writer has exact
  `t`-only latent v2 byte evidence with a canonical three-digit timestamp, passes
  its raw `time.Time` observation unchanged to the sink, and never pre-rounds;
  sink-owned rounding/carry is evidenced by read-only consumption of the shared
  vectors with no duplicated expectations. V1 trace hashes match; stdin and
  pending-action semantics occur exactly once; the full carry-forward v2
  deterministic gate passes and identity/corruption failures remain closed; v1
  correctness and three-pass-plus-tail compatibility do not regress; advisory
  evidence follows the external artifact protocol; production still emits v1
- **Handoff:** report inventory closure, byte hashes and exact `t`-only latent
  encodings, canonical three-digit output, raw-observation-to-sink call-contract
  evidence and caller no-pre-round audit, shared rounding-vector hash/read-only
  results, stale-format audits, backend-specific tests, scoped exceptions,
  commands, and risks

### Phase 10: Cut new producers over to pure v2

- **Stable ID:** `v2-producer-cutover`
- **Responsible:** one phase-executor subagent
- **Depends on:** `pure-harness-parsers`, `timestamp-cache-semantics`,
  `agent-direct-write-migration`, `v2-adoption-performance`
- **May run with:** none
- **Base state:** accepted stable phases `pure-harness-parsers`,
  `timestamp-cache-semantics`, `agent-direct-write-migration`, and
  `v2-adoption-performance` are integrated; the worktree is clean and producer
  cutover remains unshipped. Immediately
  before dispatch, resolve actual HEAD, `origin/main`, and any configured
  upstream into the ephemeral manifest and capture the full production physical
  entry-point inventory, v1/v2 fixture statistics, and accepted 1 GiB adoption
  benchmark/pass-count baseline
- **VCS authority:** follow the global serial/isolated integration rule exactly;
  no subagent may stage, commit, merge, reset, rebase, or push; any unexpected
  generated delta fails the phase
- **Write scope:** new-file header version, single relay selection site, task
  creation/reopen/resume/adoption plumbing, producer vocabulary, and guard tests
- **Data authority:** a new physical file's exact `t:"caic_meta"`, version 2
  header is authoritative; an existing exact-`type` v1 or exact-`t` v2 header
  remains authoritative; selected relay and sink derive from it
- **Purpose:** enable v2 only after every reader and writer is ready
- **Deliverables:** new files write exact `t:"caic_meta"` version 2 headers;
  relay script selection is exact; Codex/OpenCode custom paths share it; existing
  exact-`type` v1 resume starts v1; alive adoption never restarts relay; all v2
  backend controls use `t` and unchanged semantic tokens; the relay and sink
  physical encoders each round their own raw observation exactly once and emit
  exact three-digit timestamps, while backend callers never pre-round; purity
  guard audits every physical line with no v2 `type` alias
- **Generated artifacts:** none expected; `make check` may verify generated files
  but an actual delta must be explained and separately approved
- **Change budget:** at most 8 production files and 8 tests; header change occurs
  in exactly one constructor/default; no v1 relay edit
- **Boundary:** no migration of existing logs, per-line conversion, public API
  change, or automatic repair; missing-header adoption still fails closed
- **Decision checkpoints:** stop if more than one new-version default appears, if
  existing files need rewriting, or if any producer cannot derive from header
- **Validation intent:** pure new v2 across header, relay, direct, provisioning,
  trailer, and both `Task.WriteToLog` branches: every physical record uses `t`,
  every agent timestamp is positive/in-range with exactly three fractional digits,
  and the shared read-only encoder vectors prove below/at/above-half plus second
  carry without caller pre-rounding or duplicated expectations. Resumed v1
  remains byte-compatible with `type`; resumed v2 remains exact `t`; wrong script/
  discriminator selection, v2 `type` meta/control/agent, unknown/mixed files, and
  zero/negative/overflow/out-of-range producer observations fail before append;
  every production physical entry point derives authority/parser/sink behavior
  from the header; prompt and pending-action identities occur exactly once across
  live/attach/replay; taskmeta cannot override the header; all line sizes fit.
  The carry-forward v2 gate proves one complete semantic pass through matching
  reopen, only frozen bounded reads, one native parse per agent, no payload-sized
  fast-reader/adoption envelope-extraction copy, and no outer agent unmarshal; it
  imposes no zero-copy requirement on producer emission. Mismatch and corruption
  fail closed. V1 retains compatibility correctness and its accepted three-pass-
  plus-tail ceiling.
- **Validation commands:** cwd `/home/user/src/caic`: verify immutable v1/v2
  source/evidence hashes and external artifact path; run focused carry-forward v2
  deterministic/corruption/reopen tests and the resource-qualified canonical
  1 GiB v2 benchmark under the frozen tag/benchstat protocol; run focused v1
  compatibility counting tests, `go test ./backend/internal/agent/...
  ./backend/internal/task/... ./backend/internal/eventreplay/...
  ./backend/internal/server/...`, `python3
  backend/internal/agent/relay/test_relay.py`, and `python3
  backend/internal/agent/relay/test_relay_v2.py`; hash evidence and clean
  transient external artifacts; run the global stale-format audit over all
  physical fixtures/examples/writers while preserving v1 `type` and nested native
  payload keys exactly as rule 8 permits; run `make lint-fix`; `make check`;
  `make lint-docs`; `git diff --check`; `git diff --cached --exit-code`; `git
  status --short`;
  `git ls-files --others --exclude-standard`
- **Review:** fresh full-diff reviewer receives pre-integration SHA, authority
  contract, producer inventory, exact fixtures/artifact delta, and all command
  results; require `PASS` before integration acceptance
- **Exit gate:** automated purity and entry-point audits find `t` as the physical
  discriminator on every v2 line, `caic_meta` only as the unchanged header token,
  exact three-digit rounded timestamps on every v2 agent record, no v2 `type`, no
  bare native line, no `Task.WriteToLog` or other physical append bypass, and no
  reader using alternate authority; controlled existing v1 stays entirely v1;
  the complete carry-forward v2 structural gate and every discriminator/
  timestamp/mismatch corruption test pass; v1 correctness and three-pass-plus-
  tail compatibility do not regress; advisory evidence follows the external
  artifact protocol; full `make check` passes with no unexpected filesystem or
  staged delta
- **Handoff:** report new/existing discriminator and rounding cases, relay
  selection proof, purity/stale-format audits, full validation, generated-state
  check, diffstat, and risks

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
  and script files are evidence only. The runtime smoke has no deterministic clock
  seam and cannot establish rounding-boundary or second-carry behavior
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
  not add a deterministic clock seam or claim the live smoke proves rounding or
  carry; do not depend on external LLM credentials for format assertions beyond
  the real harness/runtime path's guarded prerequisites
- **Decision checkpoints:** stop if a preserved environment may be modified, if a
  v1 fixture cannot be created reproducibly, or if cleanup cannot be guaranteed
- **Validation intent:** new v2 round-trip has exact `t` on every physical line
  and a canonical `ts` with exactly three fractional digits on every live agent
  record; decoding preserves the timestamp exactly as `ProducerTime`, whose Unix
  nanoseconds are millisecond-aligned. Because the smoke has no deterministic
  clock seam, it does not prove rounding boundaries or second carry; the earlier
  relay and sink tests against the shared raw-observation vectors provide that
  deterministic evidence. Alive exact-`type` v1 adoption remains v1; alive exact-
  `t` v2 adoption remains v2; restart selects the matching script for existing
  files; v2 `type`, every noncanonical timestamp form, missing-header, and mixed-
  file cases refuse attach/append; no duplicate log line. On a resource-qualified
  smoke host, the carry-forward v2 gate proves one complete semantic pass through
  matching reopen, only
  frozen bounded reads, one native parse per agent, no payload-sized fast-reader/
  adoption envelope-extraction copy, and no outer agent unmarshal; producer
  emission is outside the copy gate. Every mismatch/corruption case fails closed.
  V1 remains compatibility-only and timing remains advisory.
- **Validation commands:** cwd `/home/user/src/caic`: verify immutable v1/v2
  source/evidence hashes and external artifact path; where the smoke host meets
  the resource gate, run focused carry-forward v2 deterministic/corruption/
  reopen tests and the canonical 1 GiB v2 benchmark under the frozen tag/benchstat
  protocol plus focused v1 compatibility tests; otherwise block/escalate rather
  than shrinking the gate. Hash evidence and clean transient external artifacts;
  run `make smoke-wrapped-log`; `make lint-fix`; `make check`; `make lint-docs`;
  rerun `make smoke-wrapped-log`; run the global stale-format audit over captured
  physical samples while preserving v1/native payload uses exactly as rule 8
  permits; `git diff --check`; `git diff --cached --exit-code`; `git status
  --short`; `git ls-files --others --exclude-standard`; verify no runtime, temp,
  generated,
  staged, untracked, `coverage.out`, or repository performance artifact remains
- **Review:** fresh runtime/reliability reviewer inspects the integrated smoke
  harness, command logs, physical samples showing canonical `t`/`ts`, exact three-
  digit timestamps, and millisecond-aligned decoded `ProducerTime`, plus the
  separate earlier deterministic relay/sink vector evidence. The review must not
  attribute rounding-boundary or carry proof to the clock-seam-free smoke; it also
  checks cleanup evidence and the exact test-only delta; require `PASS`
- **Exit gate:** controlled v1/v2 creation and adoption pass on real md through
  `make smoke-wrapped-log`; byte-level audits show no mixed file, no v2 `type`,
  canonical `t`/`ts` with exactly three timestamp digits, exact decoded
  `ProducerTime`, and millisecond alignment. Rounding-boundary and carry evidence
  comes only from the earlier deterministic relay/sink shared-vector tests, not
  from the smoke. The full carry-forward v2 structural/discriminator/timestamp/
  mismatch gate passes on a resource-qualified smoke host, v1 compatibility does
  not regress, advisory evidence follows the external artifact protocol, and real
  restart timing is recorded; full `make check` passes; cleanup leaves no
  container, log, temp, generated, staged, untracked, `coverage.out`, or
  performance artifact
- **Handoff:** report environment, exact commands, sample hashes/discriminators,
  canonical three-digit `ts`, exact millisecond-aligned decoded `ProducerTime`,
  and explicitly state that the smoke has no deterministic clock seam and does
  not prove rounding/carry. Reference the earlier deterministic relay/sink shared-
  vector evidence; report restart results, stale-format audit, cleanup proof,
  changed files, and residual host risks

## Completion condition

The rollout is complete only after every phase has been integrated in dependency
order, independently reviewed clean, validated on the integrated target, removed
from this active plan by the plan-maintenance subagent, and the real-runtime gate
has passed. Git history and durable tests/contracts are the changelog; completed
phase prose is deleted rather than accumulated here.
