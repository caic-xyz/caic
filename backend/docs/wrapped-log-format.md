# Wrapped task-log format (v2) — design and rollout status

## Current status

The physical authority foundation is shipped at the shared comparison role,
`origin/main`. The accepted local integration state is unshipped and adds:

- Fork lineage is now a shipped `caic_meta` field: when present,
  `forked_from_task_id` identifies the parent task and flows into loaded-task
  and taskmeta summaries. The v2 migration must preserve the same optional
  field and semantics; strict v2 metadata validation must not reject it.
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
- The strict canonical v2 reader in `backend/internal/agent/v2_record.go` enforces
  exact `t`, `ts`, `msg` framing, canonical three-digit timestamps, one
  synchronous native callback, a bounded zero-copy `msg` view, and no outer
  unmarshal, payload-wide pre-scan, or fallback. Its durable contract lives in
  `backend/internal/agent/v2_record_test.go`,
  `backend/internal/agent/v2_record_benchmark_test.go`, and the shared
  `backend/internal/agent/testdata/v2_agent_records.json` fixture.
- The separate v2-only relay is embedded as `relay.ScriptV2` without a production
  consumer. `backend/internal/agent/relay/relay_v2.py` and
  `backend/internal/agent/relay/test_relay_v2.py` preserve native JSON value bytes,
  use the shared canonical fixture for exact three-digit timestamp framing, keep
  the v1 stream asymmetry, and leave the v1 relay sources frozen.
- Dedicated `backend/internal/agent/v1_record.go` extraction preserves ordinary
  v1 native fallback. `backend/internal/agent/pi/parse.go` and its focused tests
  validate complete unknown-event values with the same decoder before unchanged
  `RawMessage` passthrough. `backend/AGENTS.md` indexes the accepted sources.
- Explicit outer `ParsedMessage` metadata with `Message` and `ProducerTime`.
  Native callbacks remain `[]Message`, parsed output is `[]ParsedMessage`, and
  every consumer must explicitly unwrap `.Message`. This correction passed
  independent review and is accepted; the parser still has no production
  consumer.
- The accepted v1 adoption optimization established the favorable-path baseline
  governed by the **V1 compatibility-performance invariant** below: that combined
  live-adoption path removed one full pass, and canonical 1 GiB `rchar`/fixture
  fell from 4.000 to 3.000. Corrected advisory medians were approximately 39.97 s
  to 36.33 s warm and 40.53 s to 36.83 s cold; `encoding/json` dominates CPU.
  This result did not yet prove the invariant's universal ceiling. The named
  invariant governs the three accepted benchmark sources.

The locked unreleased-v2 decisions below supersede every local-only v2
`type`-discriminator or higher-than-millisecond timestamp assumption. Because v2
has not shipped, there is no alias or compatibility path to preserve. The strict
reader and shared fixture now establish the canonical parser contract; the
remaining persistent-reader work migrates task-layer bootstrap and segment
scanning without transferring that authority into `LogRecordParser`.

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
  harness parsers still recognize selected `caic_*` records. The accepted strict
  v2 reader and embedded v2 relay remain latent and unselected; there is no
  production task-layer v2 read path yet.
- `ToolTimingTracker` does not yet consume parsed producer-time metadata, and no
  production relay emits persisted per-message timestamps.
- `eventreplay.CacheVersion` is 4.

Every later v1 path is governed by the **V1 compatibility-performance
invariant** below. V2 owns the next hard adoption-performance gate.

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
wire protocols, or the cached `EventMessage` JSONL body encoding. Derived cache
headers gain only the physical-identity and validated-authority proof fields
required below. The replay cache version bump owns semantic invalidation for the
conversion change; it does not change event encoding.

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
`harness` are authoritative for the entire file. The raw-key validator rejects
duplicate discriminator (`type` or `t`), `version`, or `harness` keys on the
first header and on every later meta record, whether duplicate values match or
conflict; it also rejects both discriminator keys together. Later `caic_meta`
records are legal segment markers only when their version-specific
discriminator, `version`, and `harness` match that first header. A missing first
header, any duplicate/both/wrong authority key, changed version, or changed
harness is corruption.

`LogStore.Reopen`, path-based `Task.WriteToLog`, and every adoption/resume path
read this first-header authority from the same inode they would append. If the
local header is missing or unreadable, adoption fails closed: no relay attach,
relay restart, replacement header, or append is allowed. Runtime metadata may
only corroborate the header harness: a nonempty runtime/header mismatch fails
adoption closed, an empty runtime value supplies no fallback, and runtime
metadata never overwrites the header-derived harness. This is intentionally
stricter than guessing from runtime metadata or the deployed script.
`persistent-read-paths` owns the combined exact version-aware authority, physical
proof, fresh-parser construction, and persistent-consumer migration.
`persistent-consumer-migration` is its dependent no-new-implementation
verification/acceptance slice. After validating the first header
inside a semantic scan, the task layer resolves a fresh native parser for that
header harness, constructs `NewLogRecordParser` with the already-validated
`LogVersion`, and passes it every physical record. The record parser neither
establishes header authority nor tracks which record is first. Until that
consumer migration, exact-`t` v2 is not a production task-layer adoption claim.
Until the typed versioned sink is integrated, raw append accepts only valid v1
authority; valid v2 is rejected before append so an untyped writer cannot create
a mixed file.

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
- v2 `caic_meta` retains every supported task-header field with its existing JSON
  name and meaning, including optional `forked_from_task_id`; that value remains
  task lineage metadata, not header authority, and is projected into loaded-task
  and taskmeta summaries like its v1 counterpart;
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

There are two structural record decoders selected once from task-layer file
authority. The future persistent-reader migration will resolve a fresh native
parser only after header validation, calling `Backend.NewWire()` for every
request and never returning a stored callback. Empty or unknown harnesses fail
rather than defaulting. A
summary-only inventory read may omit native-parser construction, but every
semantic physical scan reads and validates its first header on that same scanner,
then invokes the resolver exactly once and constructs a fresh
`NewLogRecordParser` from the validated harness/version. There is no preliminary
authority scan and no parser reuse across files, scans, goroutines, session/tail
loads, export, or replay.

The task layer passes every physical record, including authorized bootstrap and
segment records, to that scan-local parser. The parser does not discover header
authority or track first-record position. V1 retains its accepted compatibility
parser and ordinary native fallback. V2 uses the strict fast record reader below;
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
task-layer persistent reader:
  validate first nonempty record as the exact caic_meta bootstrap
  parser = NewLogRecordParser(already-validated LogVersion, native parser)
  parser.ParseRecord(bootstrap)
  for each later physical record in the same semantic pass:
    if record is caic_meta:
      validate its exact discriminator/version/harness against bootstrap
    parser.ParseRecord(record)

parseV1(record):
  recognized type=caic_* control -> shared control conversion
  otherwise, including a missing or unrecognized type -> native harness parser
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

Under the same publication lock used by attach replay, the relay persists each
complete record before client publication: it loops through partial file writes,
rejects zero, missing, or invalid write counts and write errors, flushes the
output, and only then sends the same record bytes to the client. A persistence
failure sends no bytes for that record to the client. This preserves publication
order and keeps attach offsets aligned to the physical file.

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
  `caic_meta.harness` in each physical task-log file. A nonempty runtime harness
  must equal that value or adoption fails closed; it never replaces the header.
- **Derived:** the selected fresh parser, selected relay script, control
  vocabulary, sink behavior, cached in-memory `LogVersion`, and both taskmeta and
  replay sidecars. `ValidatedLogSnapshot` is an in-memory, non-persisted
  derivation bound to the open file's device/inode or `os.SameFile`-equivalent
  identity, size, nanosecond mtime, strict validated header authority, and
  scanner/decompressor EOF followed by unchanged-path verification. It may carry
  parsed authority, controls, session state, `[]agent.ParsedMessage`, pending-
  action identities, and a matching cache key only for that exact observation.
- **Immutable snapshot:** session, model context, PR, result, pending action,
  provisioning log, context-reset, and native protocol records already appended
  to a file. An agent record's `ProducerTime` is its already-rounded producer
  observation and is immutable at millisecond precision.
- **Provenance:** `agent` framing identifies native harness protocol content;
  top-level controls identify caic-owned content. Provenance is not identity.
- **Presentation:** rendered events, exported markdown, warnings, and bounded
  malformed-line diagnostics.
- **Redundant:** launch flags, runtime metadata, deployed filenames, cached
  version/harness, and relay liveness are not alternate authorities and must
  never be used as a fallback.

### Persistent observation, carrier, cache, publication, and compatibility invariants

The task package owns the sole physical plain/zstd opener, strict header/segment
scanner, and physical export read path. `agent` owns only pure semantic rendering;
it receives already validated metadata and explicitly unwrapped messages, never
an `io.Reader` or task-log pathname. This direction avoids an `agent` to `task`
import and forbids a second export scanner. The shared `agenttest` golden-export
helper accepts an injected export closure; only the Claude, Codex, Pi, and
OpenCode external harness `_test` packages import `task` and call its physical
export reader, so `agenttest` itself never imports `task` and cannot create the
`task` test-binary import cycle.

Every semantic snapshot or iterator carries `agent.ParsedMessage` through the
task boundary and replay compaction/conversion boundary. Reducers and consumers
that do not use time—inventory/session projections, `LoadedTask.Msgs`, tail UI
restore, and pure export rendering—explicitly unwrap `.Message` at their own
boundary. Both `eventreplay.RegenerateReplay` and live
`eventreplay.MessageWriter.WriteMessage` accept `agent.ParsedMessage`; their
filtering/compaction pipelines and replay-facing `ToolTimingTracker` entry retain
the wrapper and inspect `.Message`. `ToolTimingTracker` receives the harness from
the validated snapshot/header, never a runtime value or caller-supplied label.

**Path-specific producer-time conversion invariant.** Every conversion site uses
a nonzero `ProducerTime` unchanged as the event and timing observation. For a
zero value, each site preserves its current observation policy:
`RegenerateReplay` uses one fixed observation per regeneration;
`MessageWriter` uses per-message wall clock; `task_handlers` in-memory history
uses one fixed observation per replay; and live SSE uses per-message wall clock.
A writer lifetime never freezes a live per-message fallback. A nonzero harness-
native duration remains authoritative over a producer-time delta.
`persistent-read-paths` implements this explicit path-specific final policy for
both writers before either writes cache version 5; version 5 owns that final
policy and its invalidation. `persistent-consumer-migration` audits that evidence
without changing the implementation. Cached `EventMessage` body encoding does not
change. `live-relay-read-path` only supplies the original live `ProducerTime`
through the final writer API. `timestamp-cache-semantics` preserves every
remaining conversion site's existing zero-time policy and remains persistent-
replay-output-neutral; it may not homogenize fixed-observation in-memory history
and per-message live behavior. Later phases may not change either writer's
conversion or event bytes. Equality among sites' zero-time byte streams is not an
invariant.

**V1 compatibility-performance invariant.** The accepted favorable combined v1
live-adoption path preserves correctness and fail-closed behavior and has the
accepted result of three complete raw-log passes plus one bounded tail.
`persistent-read-paths` owns establishing and proving that every complete combined
v1 live-adoption path preserves correctness and fail-closed behavior and performs
at most three complete raw-log passes plus one bounded tail. Its proof includes
both the favorable adoption benchmark and new native-session/no-top-level-session
plain/zstd counting coverage exercising the real path. Every later phase preserves
that universal result. Timing, throughput, allocations, and physical reads remain
advisory. `persistent-read-paths` also migrates every `SetParser` caller to the
fresh factory, including `backend/internal/task/load_benchmark_test.go` and
`backend/internal/server/router_test.go`, then deletes `SetParser` and its stored
callback with no compatibility adapter. Only this combined phase may mechanically
adapt `backend/internal/task/load_benchmark_test.go`; the Linux and other-platform
cache helpers remain read-only. Fixture generation, measured path, pass-count
assertions, cache-control logic, and benchmark meaning are frozen across all three
benchmark sources. The phase records their pre-migration hashes, the narrow
main-source diff, and post-migration hashes; after independent review accepts the
mechanical adaptation, all three sources and resulting hashes are frozen. The
later `persistent-consumer-migration` acceptance slice audits this evidence and
may recognize retirement of the legacy seam only when the combined Phase 1 gate
supplies it; it introduces no implementation.

Both sidecars are derived and retain their existing temporary-file completion and
atomic-rename behavior. Their versioned cache headers carry the required
identity/authority proof; replay `EventMessage` body bytes keep their existing
encoding. `logSummaryVersion` advances from 2 to 3 when taskmeta validation
lands; `eventreplay.CacheVersion` advances from 4 to 5 only with the named path-
specific conversion invariant. Neither writer may commit a version-5 entry with
another policy. Every older entry is a miss and is rebuilt, including unreleased
version-2 entries whose raw header used `type`. A cache hit
opens the raw file, captures its physical identity/size/nanosecond mtime, reads
and strictly validates the raw first header from that same open plain/zstd
observation, and verifies the descriptor/path observation remains unchanged
before any cached result or replay byte is published. The cache header/key must
match the device/inode or platform `os.SameFile`-equivalent identity, size,
nanosecond mtime, and validated header version/harness. Cached version/harness
are consistency checks against that raw authority, never parser-selection input.
A mismatch, unreadable header, changed path/descriptor, corrupt cache, or unknown
cache version is a miss; failed rebuilds leave no replacement.

A raw-log replay miss performs exactly one complete semantic raw-log pass. Parsed
and converted event data, including any pending compaction run, goes to a
seekable temporary on-disk derived spool so RAM is bounded by the scanner record
limit and fixed conversion/compaction state. Nothing is written to SSE while the
raw scan runs. Only after scanner/decompressor EOF and final unchanged physical
identity/header proof may the completed spool be atomically installed as the
replay cache and then reread for SSE. Cancellation, parse/conversion error,
truncation, identity change, cache-commit failure, or invalid EOF discards the
spool and publishes no history bytes; there is no partial raw-stream fallback.
Thus the contract deliberately makes no immediate first-byte SSE claim while
retaining one raw-log pass, bounded RAM, and fail-closed publication.

Forbidden additions and fallbacks:

- no second authoritative version field outside `caic_meta`; the only approved
  persisted derived copies are identity-bound version/harness consistency fields
  in rebuildable taskmeta and replay cache headers, and a validated snapshot must
  remain in memory rather than persist a second authority;
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
  physical-authority, parsed-message metadata, strict canonical v2 reader and
  fixture, Pi complete-value validation, dedicated v1 extraction, v1 adoption-
  performance results, and the separate latent v2 relay plus `relay.ScriptV2`
  embed and tests are present, including the strict-reader source, tests and
  microbenchmark plus the three v1 adoption benchmark sources and their evidence
  under the named V1 compatibility-performance invariant. Then record actual HEAD
  as `LOCAL_BASE` in the fresh ephemeral manifest. The
  parser has no production consumers and does not establish a published API
  contract.
- **Completed phase role:** after implementation and again after integration,
  freshly capture and record the exact resulting HEAD as `PHASE_FINAL` in
  ephemeral status and the phase handoff. Immediately before review, freshly
  capture and record all three exact values in the reviewer prompt.

Focused and full agent tests, focused changed-path race tests, vet, mandatory
lint, method placement, diff, and status checks passed for the accepted local
parser and strict fast reader. The accepted source contract includes strict v2
control and agent rejection, exact canonical timestamps, a single synchronous
native callback, bounded zero-copy payload extraction, no outer unmarshal or
payload-wide pre-scan, dedicated v1 native fallback, and complete same-decoder Pi
validation before unchanged unknown-event passthrough. The full `agent/...` race
run still has the pre-existing Pi one-second timeout reproduced at its clean
comparison base; it is not evidence against the local parser and remains outside
this rollout. `ParsedMessage` replaced `TimestampMessage` and passed independent
review. The named v1 invariant also passed fresh review; measurements are above.
The strict-reader implementation, fixture, tests, microbenchmark, Pi validation,
dedicated v1 extraction, reviewed v1 benchmark sources, and the separate latent
v2 relay implementation, embed, and tests are durable prerequisites rather than
active phases.

- **Expected worktree condition:** clean or limited to the exact user-accepted
  unshipped rollout delta and the declared writer-owned phase scope; no
  unexplained staged, untracked, or generated paths
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
2. Before dispatch, integration, and review, freshly record exact HEAD, base/
   upstream and pre-integration commits, status paths, and sensitive-file stats in
   ephemeral state. Freeze `SHIPPED_BASE`, `LOCAL_BASE`, and `PHASE_FINAL`; never
   reuse a manifest or moving ref. History or worktree/index changes invalidate
   it. Durable dependency pseudo-versions and content identities remain pinned.
3. Focused validation runs first. The **standard phase validation footer** is
   `make lint-fix`, `make lint-docs`, `git diff --check`, `git diff --cached
   --exit-code`, `git status --short`, `git ls-files --others
   --exclude-standard`, and exact-scope verification from the canonical cwd. Every
   implementation phase runs it; unexpected output fails. When a generator uses
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
relay-v2 -> live-relay-read-path

persistent-read-paths -> persistent-consumer-migration
persistent-consumer-migration -> live-relay-read-path
persistent-consumer-migration -> pure-harness-parsers
live-relay-read-path -> pure-harness-parsers
persistent-consumer-migration -> timestamp-cache-semantics
live-relay-read-path -> timestamp-cache-semantics
persistent-consumer-migration -> versioned-log-sink

persistent-consumer-migration -> v2-adoption-performance
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

The `relay-v2 -> live-relay-read-path` edge is a satisfied prerequisite retained
to make the accepted relay dependency explicit; `relay-v2` is no longer an active
phase. The accepted relay, parsed-message metadata, strict fast reader, shared
fixture, Pi validation, dedicated v1 extraction, and v1 adoption result are
prerequisites of the active graph. `persistent-read-paths` is the earliest active
combined implementation phase: it establishes authority/snapshot/cache primitives,
routes every persistent consumer through them, finalizes both version-5 replay
writers, and deletes the legacy stored-parser seam. Its dependent
`persistent-consumer-migration` slice independently audits that combined gate and
accepts the legacy-seam retirement only on its evidence; it adds no implementation.
Later persistent-consumer dependencies therefore wait for that acceptance slice.
`versioned-log-sink` also waits for that acceptance slice: it may not start from,
or treat the combined implementation gate as, acceptance. `pure-harness-parsers`
and `timestamp-cache-semantics` may run together only after both consumer read
paths are integrated. `v2-adoption-performance`
waits for the acceptance slice and the sink, and both direct-writer migration and
final cutover wait for its hard deterministic gate. Integration remains one phase
at a time.

## Active phased rollout

### Phase 1: Establish and migrate persistent authority and consumers

- **Stable ID:** `persistent-read-paths`
- **Responsible:** one phase-executor subagent
- **Depends on:** none
- **May run with:** none
- **Base state:** accepted reader/v1 extraction, fixture, Pi validation,
  `ParsedMessage`, v1-adoption, and latent-relay prerequisites are integrated;
  apply the global fresh-manifest rule, then refresh every persistent loader,
  export/replay/SSE caller, factory/carrier/cache/streaming path, all `SetParser`
  callers, benchmark hashes/counts, and sensitive-file statistics.
- **VCS authority:** global VCS rule; no fixture/golden regeneration without
  coordinator approval.
- **Write scope:** the combined implementation scope: task plain/zstd
  opener/scanner, authority envelope, snapshots/factory/cache keys/taskmeta/reopen,
  inventory/full/session/tail/iterator/adoption consumers, task-owned export and
  its agent pure-rendering boundary, `agenttest` injected export closure and four
  external harness export suites, eventreplay regeneration/live writer/cache/filter/
  spool, v1 conversion, task replay-writer and fake seams, delayed SSE publication,
  taskmgr, record-trace, app/manager resolver wiring, every remaining `SetParser`
  caller (including authorized mechanical benchmark/router-test updates), the
  immutable backend-map server-test cleanup, and adjacent focused tests. The two
  cache-helper benchmark sources, relay/startup readers, writers/sink, producer
  cutover, and unrelated producer-time consumers remain out of scope.
- **Data authority and carrier:** the task-owned scanner alone validates exact
  first/later version-specific header authority and physical plain/zstd EOF. Only
  after that header it resolves one fresh `Backend.NewWire()` native parser,
  constructs `LogRecordParser` with the validated version, and passes every record
  once; parser authority remains parser-owned only for v1/v2 control
  classification, with fail-closed controls/v2 parsing and no independent
  task-level control reprobe. `ValidatedLogSnapshot` is non-persisted and bound to
  device/inode-equivalent identity, size, nanosecond mtime, validated header, EOF,
  and unchanged path. It carries `[]agent.ParsedMessage`; consumers explicitly
  unwrap only at time-irrelevant boundaries. Runtime and cached metadata only
  corroborate raw authority, never select a parser.
- **Purpose:** complete the combined persistent read-path implementation now,
  including all consumer migration, cache/replay/SSE behavior, and legacy seam
  removal, while preserving the locked parser, authority, carrier, streaming, and
  V1 performance contracts.
- **Deliverables:** implement strict first/later `caic_meta` key/version/harness
  validation; runtime mismatch failure; fresh scan-local parser construction with
  no preliminary pass/state reuse; identity-bound EOF-complete snapshots; and
  taskmeta 2-to-3 raw-header-on-hit, atomic derived-cache behavior. Also migrate
  route inventory, full/session/tail/iteration/adoption/reopen,
  physical export, replay regeneration and replay-cache hits through that
  foundation; move physical export into task and leave agent rendering pure;
  inject the task export closure into `agenttest` from the four external harness
  test packages; migrate every remaining `SetParser` caller and delete its method
  and stored callback without an adapter; eliminate the task-level
  `IsLogControlRecord` predicate and carry version-specific control classification
  from `LogRecordParser` to its consumers without an independent task reprobe;
  carry `ParsedMessage` through snapshots, iterators, replay filters/compaction,
  `RegenerateReplay`, `MessageWriter`, and
  task's replay seam; apply the path-specific producer-time policy in both replay
  writers and atomically advance replay cache 4-to-5 without changing cached event
  bodies. Until `live-relay-read-path`, current live `agent.Message` callers
  explicitly construct zero-time wrappers. Replay cache hits validate same-observation raw authority and
  full identity; old/corrupt/mismatched entries miss and rebuild atomically. A miss
  makes one semantic raw pass into a seekable disk spool, including bounded pending
  compaction; SSE publishes only after EOF/final unchanged proof and atomic cache
  commit/reopen, with no partial fallback. Preserve v1 bytes/fallback/`v1_record.go`
  and prove the universal three-complete-passes-plus-bounded-tail V1 ceiling with
  favorable and native-session/no-top-level-session plain/zstd coverage.
- **Generated artifacts:** none committed; caches, spools, fixtures, profiles, and
  benchmarks use temporary directories and clean failure/cancellation residue.
- **Change budget:** at most 20 non-`_test.go` files, explicitly including the
  test-only `backend/internal/agent/agenttest/agenttest.go` helper and
  `backend/internal/task/tasktest/fake.go` replay test double, and 27 `_test.go`
  files across the stated seams; at most two minimal raw-log fixtures. The
  read-only `backend/internal/task/load_benchmark_cache_linux_test.go` and
  `backend/internal/task/load_benchmark_cache_other_test.go` cache helpers are
  excluded from both buckets. No public DTO, schema, cached-event body encoding,
  duplicate scanner, or unrelated refactor is allowed.
- **Boundary:** private carrier shape is the executor's choice only. It must retain
  single parser-owned v1/v2 classification, strict fail-closed raw authority and
  controls/v2 records, physical identity/EOF cache proof, explicit carrier unwrap,
  no pre-EOF/partial SSE publication, one raw replay pass with bounded-memory
  disk-spooled tail/replay, v1 compatibility/bytes/fallback, and the V1 ceiling.
  No serialized snapshot, alternate authority, parser fallback/reuse, raw replay
  second pass, unbounded buffer, cache-body change, relay/startup migration,
  sink/writer migration, or non-replay timing change is allowed.
- **Decision checkpoints:** stop for any import cycle, second authority or persisted
  snapshot, inability to bind raw header and full identity together, metadata loss
  before replay conversion, pre-EOF SSE byte, second raw pass/unbounded compaction,
  duplicate export scanner or `agenttest`→`task` import, incomplete `SetParser`
  deletion, non-mechanical benchmark change/cache-helper edit, or any weakening of
  parser authority, V1 fallback, cache proof, or performance ceiling.
- **Validation intent:** execute the complete combined implementation matrices:
  plain/zstd authority/key/version/harness/identity/EOF/factory-order coverage;
  all persistent consumer v1/v2 semantic equivalence, strict malformed-v2 and
  corruption cases, one native callback/record delivery, carrier and zero/nonzero
  timing traces, export ownership/import graph, taskmeta-3 and replay-5 old-entry/
  atomic cache matrices, one-pass bounded-spool/no-pre-EOF-SSE/cancellation cleanup,
  every former `SetParser` caller absence, and favorable plus native-session pass
  counts. Record benchmark hashes before/after; only the authorized main-source
  factory diff may change and both cache helpers remain byte-identical.
- **Validation commands:** cwd `/home/user/src/caic`: record/verify SHA-256 hashes
  for all three v1 benchmark sources; run `go test -tags adoption_benchmark
  ./backend/internal/task -run '^$' -bench '^BenchmarkTaskAdoption$' -benchtime=1x
  -count=1`; run focused authority/order/carrier/export/cache/spool/publication/
  corruption/identity/native-session tests; run `go test ./backend/internal/agent/...
  ./backend/internal/task/... ./backend/internal/task/taskmgr/...
  ./backend/internal/app/... ./backend/internal/eventreplay/... ./backend/internal/server/...
  ./backend/internal/cmd/record-trace/...`; audit persistent callers, resolver and
  scanner paths, former `SetParser` references, carrier unwraps, cache fields,
  replay APIs/wrappers, export imports, spool/SSE counts, and stale format; then
  run the standard validation footer.
- **Review:** a fresh authority/API/cache/streaming/performance reviewer receives
  the integrated target, fresh manifest, caller and carrier traces, factory/EOF/
  identity evidence, full corruption/cache/SSE matrices, export import proof,
  `SetParser` deletion audit, all benchmark hashes/counts and mechanical-diff
  proof, exact filesystem delta, and all commands; require `PASS`.
- **Exit gate:** all combined implementation deliverables and validation evidence
  above are present: every persistent consumer uses the one task scanner and fresh
  post-header parser; `SetParser` and its callback are absent; parser classification
  remains sole and fail closed; taskmeta 3/replay 5 are atomic derived caches bound
  to validated raw authority and identity; both replay writers retain
  `ParsedMessage` and the path-specific policy; export is task-owned/pure-rendered;
  replay is one-pass, disk-spooled, bounded, EOF/identity-gated and publishes no
  partial SSE; and v1 compatibility and universal ceiling hold. This combined gate
  requires fresh independent `PASS` review before Phase 2 acceptance or later
  persistent-consumer dependencies proceed.
- **Handoff:** report all migrated entry points, scanner and `SetParser`
  removals, authority/cache/carrier/export/replay/SSE proofs, complete matrices,
  benchmark hashes/counts, exact files/commands/status, residual risks, and the
  evidence for the dependent acceptance slice.

### Phase 2: Verify persistent-consumer migration acceptance

- **Stable ID:** `persistent-consumer-migration`
- **Responsible:** independent reviewer/acceptance subagent
- **Depends on:** `persistent-read-paths`
- **May run with:** none; it is the required dependent acceptance slice.
- **Base state:** Phase 1's combined implementation gate is complete and supplies
  its fresh manifest, exact delta, validation evidence, and specialist `PASS`
  review.
- **VCS authority:** global VCS rule; this slice makes no implementation edits,
  regenerations, staging, or commits.
- **Write scope:** no production or test implementation files; audit the integrated
  Phase 1 target, its evidence, and exact delta only.
- **Purpose:** independently accept the integrated persistent-consumer migration
  without adding implementation. Unlike Phase 1's specialist implementation
  review, this slice verifies that its complete exit-gate evidence is present,
  consistent with the integrated target, and sufficient to release dependent work.
- **Acceptance requirements:** confirm the integrated target satisfies every Phase
  1 exit-gate requirement, and audit the exact delta, changed-path inventory,
  validation results, benchmark/hash evidence, stale-format audit, and absence of
  staged or unexpected files. Confirm the independent specialist `PASS` review is
  present and that no compatibility adapter or second scanner survives. Do not
  accept legacy-seam retirement from prose or partial evidence.
- **Validation commands:** review Phase 1's recorded focused tests, full listed Go
  suite, benchmark/hash evidence, stale-format audit, standard validation footer,
  fresh manifest, exact scope, and specialist `PASS`; rerun only documented
  read-only checks needed to resolve a specific evidence gap.
- **Review:** a named independent persistent-consumer acceptance reviewer audits
  the integrated target and the complete Phase 1 evidence set, records `PASS` or
  a blocking gap, and does not duplicate Phase 1's specialist test execution.
- **Exit gate:** no new implementation is accepted here. This slice passes only if
  the Phase 1 combined exit gate and the independent acceptance audit both pass;
  only then is the already-implemented `SetParser` seam retirement accepted and
  dependent phases, including `versioned-log-sink`, may proceed. Any gap remains
  with Phase 1 and blocks acceptance.
- **Handoff:** identify the audited integrated-target evidence, any gap/blocker,
  legacy-seam retirement decision, exact read-only commands/status, and whether
  dependent phases may proceed.

### Phase 3: Route live and startup relay reads

- **Stable ID:** `live-relay-read-path`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-consumer-migration`; accepted stable phase
  `relay-v2` is also a satisfied prerequisite
- **May run with:** none due to overlap across agent launch/read paths
- **Base state:** accepted combined `persistent-read-paths` implementation and
  `persistent-consumer-migration` acceptance (final replay APIs, named conversion
  invariant, cache 5, live zero wrappers) and `relay-v2` (latent
  embed, strict reader/fixture, parsed metadata) are integrated; apply the global
  fresh-manifest rule and capture the live-read/handshake/session/task-dispatch/
  replay-writer graph and test baselines
- **VCS authority:** global VCS rule
- **Write scope:** shared relay record reader; `DefaultReadMessages`; agent
  connection/session/options message-channel seams; relay tail/attach; Pi startup
  loops; Codex/OpenCode handshake/custom launch paths; task session channels and
  live dispatch/add-message carrier plumbing under `backend/internal/task`;
  adjacent fakes and focused tests. The accepted `eventreplay.MessageWriter`,
  tracker/conversion/filter/cache implementation, and cache constant are read-only
- **Data authority:** caller supplies validated file `LogVersion`; physical relay
  offsets count encoded bytes; decoded payload never becomes an authority. The
  relay reader's `ParsedMessage.ProducerTime` is immutable carrier metadata and
  task dispatch must deliver that same wrapper to the accepted replay-writer API
- **Purpose:** make every live/startup consumer understand v1 and latent v2 and
  carry live producer metadata through task dispatch into the already-final replay
  writer while still running v1 producers
- **Deliverables:** relay reader with an explicit per-consumer persistence policy;
  normal-live log-once behavior; v2 agent records use the accepted strict `t`
  fast reader with no alternate envelope decoding or `type` alias; agent options,
  connection/session callbacks and channels, task session channels, and
  `startMessageDispatch` carry each `agent.ParsedMessage` without reconstructing
  it. Task state transitions, diff side effects, stored semantic messages, and
  existing semantic subscribers inspect or receive `.Message` as appropriate,
  while the task forwards the original wrapper—including exact nonzero
  `ProducerTime`—to the already-final `EventReplayWriter.WriteMessage` API.
  Synthetic/backend messages with no physical producer timestamp are wrapped
  explicitly with zero time. Remove the `persistent-read-paths` current-live zero-time bridge only at the
  point where the original parsed carrier now
  arrives; do not add a metadata-to-`Message` adapter.

  Every v2 envelope result retains its exact immutable millisecond
  `ProducerTime` through that complete reader-to-task-to-writer path without
  reader or dispatch rounding; v1 results remain zero-time. Production version
  selection remains v1, so production replay-writer inputs remain zero-time until
  cutover, while latent v2 tests exercise nonzero propagation. Also deliver v2
  native-payload unwrap for handshakes; Pi startup persistence; unpersisted Codex/
  OpenCode handshakes; `t` control routing; physical offsets; attach-overlap
  deduplication; common script deployment and selection seam consuming the
  accepted latent `relay.ScriptV2` for custom launchers; and v1 byte preservation.
  `live-relay-read-path` only supplies live producer time: the path-specific
  conversion invariant, per-path event bytes, invalidation, and cache version 5
  remain unchanged
- **Generated artifacts:** none
- **Change budget:** at most 14 production files and 10 tests across the named
  agent/backend/task carrier seams; no eventreplay/tracker/cache implementation,
  protocol DTO, or handshake sequence changes
- **Boundary:** selected version remains v1 in production; parsed metadata must
  not implement or be inserted as `Message`. This is carrier plumbing into the
  final `MessageWriter` API, not ownership of tracker, filtering/compaction,
  conversion, event bytes, cache invalidation, or cache version: all remain the
  accepted `persistent-read-paths` behavior at version 5. No alternate v2
  envelope parser, header bump, v2 deployment, sink migration, or harness-parser
  cleanup; v1 bytes and
  the V1 compatibility-performance invariant do not regress
- **Decision checkpoints:** stop if a handshake must receive a caic control as
  native data, if a consumer cannot state its persistence policy, if logging
  twice appears necessary, if offset semantics would change for v1, if any live
  hop cannot carry the original `ParsedMessage`, or if propagation would require
  a replay conversion/filter/tracker/cache change or another cache bump
- **Validation intent:** split reads, interleaved controls, handshake buffering,
  non-zero exits, attach physical offsets, relay-stdin overlap deduplication, no
  live stdin double write, v1 exact logged bytes and zero producer time, v2
  physical-byte counts, exact canonical three-digit encoded `ts`, and decoded
  `ProducerTime` exactly equal to that timestamp with Unix nanoseconds divisible
  by `1_000_000`, without reader or task-dispatch rounding or a semantic timestamp
  event. A carrier-spy test traces the same `ParsedMessage` from v2 relay decode
  through agent session/options channels and task `startMessageDispatch` to the
  accepted `EventReplayWriter`; its nonzero time must be unchanged, while task
  reducers/subscribers see the same concrete `.Message`. V1 and synthetic-message
  cases prove explicit zero time and the live per-message fallback required by the
  path-specific conversion invariant. Before/after replay regressions prove each
  writer's filter/conversion/event bytes, cache version 5, and invalidation remain
  unchanged; no eventreplay production file changes. Require
  strict rejection of v2 `type` records and every noncanonical timestamp form, Pi
  startup persistence, Codex/OpenCode handshake non-persistence, and all three
  startup paths succeeding with synthetic streams. Verify the frozen accepted v1
  benchmark hashes and focused counting evidence under the V1 compatibility-
  performance invariant
- **Validation commands:** cwd `/home/user/src/caic`: verify the frozen accepted
  post-migration v1 benchmark-source hashes; run focused v1 counting tests,
  `go test ./backend/internal/agent/... ./backend/internal/task/...
  ./backend/internal/eventreplay/... ./backend/internal/server/...`; `python3
  backend/internal/agent/relay/test_relay_v2.py`; audit every live parsed-carrier
  hop and explicit unwrap/zero-wrap site, and prove the
  `persistent-read-paths` eventreplay/tracker/cache implementation and
  `CacheVersion` are unchanged; audit live-reader
  tests under stale-format rule 8, then run the standard validation footer; any
  unexpected status or untracked output fails the gate
- **Review:** independent concurrency/protocol reviewer inspects the integrated
  reader, all custom launch paths, offsets, log-once evidence, and the complete
  relay-reader/session-channel/task-dispatch/`EventReplayWriter` carrier trace;
  requires exact immutable millisecond `ProducerTime` preservation, v1/synthetic
  zero-time proof, task `.Message` unwrap behavior, and evidence that the path-
  specific conversion invariant and each writer's event bytes, cache version 5,
  and invalidation are unchanged. The reviewer also checks no reader/dispatch
  rounding, strict `t`-only v2 rejection, and the V1 compatibility-performance
  evidence; require `PASS`
- **Exit gate:** entry-point tests prove v1 byte identity; strict `t`-only latent
  v2 decoding; exact preservation of the encoded millisecond in the same
  `ParsedMessage` through live task dispatch into the final replay-writer API; no
  reader/dispatch rounding; explicit zero time for v1 and synthetic messages;
  rejection of v2 `type` and every noncanonical timestamp; current per-harness
  persistence policy; and no duplicate stdin across live and attach paths.
  Production still selects v1. `live-relay-read-path` only supplies live producer
  time; the path-specific conversion invariant and each writer's version-5 bytes/
  invalidation are unchanged, with no cache bump. The V1 compatibility-
  performance invariant holds against frozen post-migration source hashes
- **Handoff:** report migrated call graph; the exact relay-reader/session-channel/
  task-dispatch/replay-writer carrier trace; every `.Message` unwrap and explicit
  zero-wrap site; strict `t`-only and noncanonical-timestamp rejection results;
  exact immutable millisecond `ProducerTime` and no-reader/dispatch-rounding proof;
  v2 carrier-spy and v1/synthetic-zero/live-per-message results; per-path byte/cache
  comparison confirming the named conversion invariant and cache version 5 are
  unchanged; physical-offset/log-once evidence, protocol/stale-format audits, V1
  compatibility-performance evidence, files, commands, and residual risks

### Phase 4: Make harness parsers native-only

- **Stable ID:** `pure-harness-parsers`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-consumer-migration`, `live-relay-read-path`
- **May run with:** `timestamp-cache-semantics` in an isolated worktree
- **Base state:** accepted dependencies are integrated; apply the global fresh-
  manifest rule and baseline each harness parser/test file
- **VCS authority:** global VCS rule; golden recordings are read-only
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
  direct native parsers reject/ignore caic records consistently; control coverage
  lives in shared parser tests; pending-action deduplication remains at the shared
  entry; Codex native duration and the V1 compatibility-performance invariant hold
- **Validation commands:** cwd `/home/user/src/caic`: verify the frozen accepted
  post-migration v1 benchmark-source hashes; run focused v1 counting tests, `go
  test ./backend/internal/agent/... ./backend/internal/task/...`, then the standard
  validation footer
- **Review:** fresh harness-focused reviewer checks for remaining `caic_` parser
  branches and semantic regressions on integrated target; require `PASS`
- **Exit gate:** repository audit finds no caic routing in native parsers, all
  four golden suites pass without recording changes, and the V1 compatibility-
  performance invariant does not regress
- **Handoff:** provide audit command/result, per-harness tests, deletions/delta,
  validation, and unresolved risks

### Phase 5: Make remaining producer-time consumers version-correct

- **Stable ID:** `timestamp-cache-semantics`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-consumer-migration`, `live-relay-read-path`
- **May run with:** `pure-harness-parsers`
- **Base state:** accepted dependencies are integrated; apply the global fresh-
  manifest rule and capture remaining producer-time sites, final replay APIs and
  named invariant, taskmeta 3/replay 5, each writer's baseline, and fixtures
- **VCS authority:** global VCS rule; no generated replay recording without
  approval
- **Write scope:** remaining parsed producer-time consumers and API conversion
  call sites outside the accepted persistent-replay path, plus adjacent focused
  tests/minimal fixtures. `ToolTimingTracker` implementation and persistent replay
  conversion/filtering/spooling/cache code are read-only regression scope
- **Data authority:** `ParsedMessage.ProducerTime` copied from `agent.ts` is the
  immutable producer observation already rounded to milliseconds; readers and
  consumers never re-round it. The path-specific producer-time conversion
  invariant is final, and taskmeta 3/replay 5 remain rebuildable caches
- **Purpose:** consume exact millisecond-precision v2 producer-time metadata at
  the remaining non-persistent-replay sites without changing v1 files, exposing a
  timestamp event, or changing persistent replay output
- **Deliverables:** extend `ParsedMessage` into every remaining non-persistent-
  replay time-using conversion site and unwrap `.Message` only at no-time
  consumers. Apply the path-specific producer-time conversion invariant at every
  remaining conversion site, preserving one fixed observation for
  `task_handlers` in-memory history and per-message wall clock for live SSE.
  Persisted history is already covered by `RegenerateReplay`'s fixed observation
  per regeneration and its accepted spool/cache path, which remain read-only.
  `RegenerateReplay` and live `MessageWriter` retain their APIs; each path's
  conversion, compaction, event bytes, cached-`EventMessage` encoding, and cache
  version 5 remain unchanged, with no bump. Retain pure-v1/v2 parity fixtures with
  exact `type`/`t` bootstrap and canonical timestamps
- **Generated artifacts:** no committed generated sidecars; tests generate in
  temporary directories only
- **Change budget:** at most 5 production files and 6 tests; two minimal timing
  fixtures maximum; no tracker implementation, persistent-replay conversion,
  cache constant, raw persistent reader, or sidecar format change
- **Boundary:** this phase is persistent-replay-output-neutral: it may not change
  either replay-writer API or either path's conversion/filter/event bytes,
  `ToolTimingTracker`, cache version 5, invalidation, or cached body encoding. No
  parser-side timing relevance, metadata-to-message adapter, API DTO field, v1
  timestamp retrofit, persistent replay/cache rewrite, relay change, or cutover
- **Decision checkpoints:** stop if producer time is persisted twice, becomes an
  event, is lost before the tracker, diverges from the path-specific conversion
  invariant, or requires a persistent-replay/tracker/cache change
- **Validation intent:** remaining consumers satisfy the path-specific producer-
  time conversion invariant without tracker changes. Tests independently prove
  zero-time `task_handlers` in-memory history uses one fixed observation per
  replay, while consecutive live SSE messages use per-message wall clock; no
  refactor may homogenize those policies. `RegenerateReplay` regressions preserve
  its accepted fixed-per-regeneration policy and spool/cache path without making
  them owned conversion sites. Accepted producer time stays byte-derived and
  unrounded, and is transparent to deltas, control searches, exports, and
  measured full restore. Compare each replay writer's cache bodies and events
  before/after `timestamp-cache-semantics`; replay-5 stale-cache and delayed-
  publication regressions prove output neutrality and no bump. Fixtures reject v2
  `type` and noncanonical timestamps, retain exact `t`/three-digit form, and
  enforce the V1
  compatibility-performance invariant
- **Validation commands:** cwd `/home/user/src/caic`: verify frozen accepted post-
  migration v1 benchmark-source hashes; run focused v1 counting tests, `go test
  ./backend/internal/server/api/v1conv/... ./backend/internal/eventreplay/...
  ./backend/internal/server/... ./backend/internal/task/...`; audit every zero-
  time conversion site and remaining non-persistent-replay timing carrier/unwrap
  site, including fixed-observation in-memory history and per-message live SSE,
  plus timing fixtures with the global stale-format audit; compare both accepted
  replay API signatures,
  regeneration/live conversion event bytes, and cache version before/after,
  preserving tracker/persistent replay, v1 framing, and native payload usage under
  rule 8; run the standard validation footer
- **Review:** fresh timing reviewer checks remaining carrier/unwrap sites, every
  site-specific zero-time policy under the path-specific conversion invariant,
  fixed-observation in-memory history versus per-message live tests,
  `RegenerateReplay` fixed-per-regeneration regression proof, each replay path's
  byte identity to its own `persistent-read-paths` baseline, unchanged
  APIs/tracker/replay-5 cache/
  publication, absence of another bump, and fixture delta; require `PASS`
- **Exit gate:** v1/v2 fixture semantic events are equivalent; v1 parsed values
  carry zero time, v2 parsed values carry the exact immutable rounded millisecond
  producer observation, and only millisecond-precision non-persistent-replay
  downstream durations may differ; no reader/consumer re-rounding occurs.
  The path-specific conversion invariant holds at every remaining conversion
  site: in-memory history retains one fixed observation per replay and live SSE
  retains per-message wall clock, with tests preventing homogenization.
  `RegenerateReplay` retains its fixed observation per regeneration through the
  accepted spool/cache path. Both replay APIs and each path's
  `persistent-read-paths` bytes, tracker, replay 5, invalidation, cached
  body, and delayed publication are unchanged. Cache/stale-format audits
  and the V1 compatibility-performance invariant pass; no artifact is committed
- **Handoff:** report remaining carrier/unwrap sites and named-invariant cases,
  including fixed-observation in-memory history, per-message live SSE tests, and
  `RegenerateReplay` fixed-per-regeneration spool/cache regression proof; each
  replay path's byte comparison to its `persistent-read-paths` baseline;
  unchanged parsed APIs, tracker, taskmeta 3/replay 5, invalidation, cached body,
  and no-bump proof;
  fixture hashes/discriminators, audits, filesystem delta, commands, and risks

### Phase 6: Introduce the enforcing versioned log sink

- **Stable ID:** `versioned-log-sink`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-consumer-migration`
- **May run with:** none; it starts only after the dependent acceptance slice.
- **Base state:** accepted `persistent-read-paths` implementation and
  `persistent-consumer-migration` acceptance are integrated; apply the global
  fresh-manifest rule and capture same-inode append, direct writes, snapshot
  identity, shared fixture hash, and v1 evidence
- **VCS authority:** global VCS rule
- **Write scope:** versioned sink API/implementation in `backend/internal/agent`
  and `backend/internal/task`, minimum reopen constructor plumbing, adjacent
  focused tests, and read-only use of the accepted canonical byte fixture;
  callers otherwise remain v1-compatible
- **Data authority:** extend rather than replace the accepted same-inode append
  constructor. A v2 reopen sink consumes a matching non-persisted
  `ValidatedLogSnapshot` and verifies device/inode-equivalent identity, size,
  nanosecond mtime, validated header authority, and EOF against the opened append
  target in O(1); the physical first header remains the authority. For native
  writes the caller supplies its raw `time.Time` producer observation unchanged;
  `VersionedLogSink` is the sole backend physical encoder and rounding/formatting
  authority. The sink owns no persisted version, snapshot,
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
  v1 bytes; preserve only the accepted favorable-path non-regression obligation
  of the V1 compatibility-performance invariant
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
  rerun the frozen favorable-path v1 counting case for the accepted non-regression
  obligation under the compatibility-performance invariant without changing the
  three v1 benchmark sources; run the global stale-
  format audit over sink tests/fixtures under rule 8, then the standard validation
  footer
- **Review:** an independent API/authority reviewer receives the integrated target
  and pre-integration base from fresh ephemeral state, snapshot/sink contract,
  O(1) call-count proof, exact mismatch matrix, explicit fallback if any, read-
  only shared raw-observation fixture parity, raw-`time.Time` sink ownership and
  exactly-once rounding proof, absence of caller pre-rounding or duplicated
  expectations, writer inventory, accepted favorable-path v1 non-regression
  evidence, artifact delta, and gate; require
  `PASS`
- **Exit gate:** a matching validated snapshot enables O(1) physical identity and
  EOF verification before typed v2 reopen/append; missing/stale/mismatched proof
  fails closed or follows only the fully tested explicit validation fallback;
  same-inode authority is unchanged; sink tests consume all shared raw-observation
  vectors read-only and prove v1 byte identity, raw-`time.Time` input, sink-owned
  exactly-once rounding and canonical three-digit typed-only v2 `t` encoding with
  no caller pre-rounding or duplicated expectations, preserved native bytes, null
  diagnostics instead of `msg:null`, final-size enforcement before emission,
  untyped-v2 and v2-`type` rejection, restart-safe pending-action behavior, and
  non-regression of the accepted favorable-path result under the
  V1 compatibility-performance invariant
- **Handoff:** report constructor/reopen signature, snapshot match fields and
  lifetime, syscall/read-count proof, mismatch/fallback results, direct-write
  inventory, shared fixture hash and read-only vector results, v1/v2 discriminator/
  native/null/size behavior, raw-`time.Time` API contract, sink-owned exactly-once
  rounding/canonical formatting proof, caller no-pre-round audit, favorable-path
  v1 non-regression evidence, stale-format audit, exact files, commands, cleanup,
  and remaining migration sites

### Phase 7: Gate v2-primary adoption performance

- **Stable ID:** `v2-adoption-performance`
- **Responsible:** one phase-executor subagent
- **Depends on:** `persistent-consumer-migration`, `versioned-log-sink`
- **May run with:** none
- **Base state:** accepted dependencies and fixed reader/snapshot/sink contracts
  are integrated; apply the global fresh-manifest rule and capture app/taskmgr v2
  adoption/reopen graphs, fixture/frozen-v1 hashes, pass/callback/copy/unmarshal
  behavior, sensitive-file statistics, and an external artifact directory
- **VCS authority:** global VCS rule. The coordinator must obtain a user-owned
  intent-to-add checkpoint for exactly
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
  within scope; CPU profile/runtime-trace analysis; unchanged frozen
  `persistent-read-paths` v1 benchmark sources and compatibility behavior
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
  matches. Only then may production optimization start. The accepted
  `persistent-read-paths` v1 sources remain frozen
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
- **Boundary:** preserve the V1 compatibility-performance invariant and its
  frozen `persistent-read-paths` benchmark sources; do not weaken same-
  inode/snapshot/EOF
  authority, persist a second
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
  benchmark generators, fixtures, tests, and samples under stale-format rule 8;
  run `make lint-fix`, replay the v2 benchmark, then run the standard validation
  footer
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
benchmark sources remain immutable; the three v1 sources remain frozen at their
independently accepted `persistent-read-paths` hashes. Timing, allocation,
and physical-read evidence stays advisory. Each phase records source/evidence
hashes and an absolute external artifact directory, uses
`-tags v2_adoption_benchmark` for any v2
benchmark invocation, follows the same resource/cross-platform/artifact-cleanup
rules, and never commits performance artifacts. V1 uses the V1 compatibility-
performance invariant.

### Phase 8: Migrate every direct log writer

- **Stable ID:** `agent-direct-write-migration`
- **Responsible:** one phase-executor subagent
- **Depends on:** `versioned-log-sink`, `live-relay-read-path`,
  `pure-harness-parsers`, `v2-adoption-performance`
- **May run with:** none; it crosses all agent backends and task writers
- **Base state:** accepted dependencies are integrated; apply the global fresh-
  manifest rule and capture all physical readers/appends, direct writes, and v1
  trace hashes
- **VCS authority:** global VCS rule; no golden recording regeneration without
  approval
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
  reader or append bypass. The carry-forward v2 performance contract and V1
  compatibility-performance invariant hold; its copy gate does not cover sink
  emission
- **Validation commands:** cwd `/home/user/src/caic`: verify frozen accepted v1/v2
  benchmark-source and evidence hashes plus the external artifact path; run the
  focused carry-forward v2 deterministic/corruption/reopen tests and resource-
  qualified canonical 1 GiB v2 benchmark with the frozen tag/benchstat protocol;
  run focused v1 compatibility counting tests, `go test
  ./backend/internal/agent/... ./backend/internal/task/...`, and `python3
  backend/internal/agent/relay/test_relay.py`; hash/clean external results; audit
  direct `.Write` calls and migrated writers/tests under stale-format rule 8,
  then run the standard validation footer
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
  deterministic gate passes, identity/corruption remains closed, and the V1
  compatibility-performance invariant holds; advisory
  evidence follows the external artifact protocol; production still emits v1
- **Handoff:** report inventory closure, byte hashes and exact `t`-only latent
  encodings, canonical three-digit output, raw-observation-to-sink call-contract
  evidence and caller no-pre-round audit, shared rounding-vector hash/read-only
  results, stale-format audits, backend-specific tests, scoped exceptions,
  commands, and risks

### Phase 9: Cut new producers over to pure v2

- **Stable ID:** `v2-producer-cutover`
- **Responsible:** one phase-executor subagent
- **Depends on:** `pure-harness-parsers`, `timestamp-cache-semantics`,
  `agent-direct-write-migration`, `v2-adoption-performance`
- **May run with:** none
- **Base state:** accepted dependencies and the tested latent `relay-v2` embed are
  integrated; apply the global fresh-manifest rule and capture all production
  physical entry points, v1/v2 fixture statistics, and accepted 1 GiB adoption
  benchmark/pass baseline
- **VCS authority:** global VCS rule; unexpected generated delta fails the phase
- **Write scope:** new-file header version, single relay selection site, task
  creation/reopen/resume/adoption plumbing, producer vocabulary, and guard tests
- **Data authority:** a new physical file's exact `t:"caic_meta"`, version 2
  header is authoritative; an existing exact-`type` v1 or exact-`t` v2 header
  remains authoritative; selected relay and sink derive from it
- **Purpose:** enable v2 only after every reader and writer is ready
- **Deliverables:** new files write exact `t:"caic_meta"` version 2 headers;
  exact header-derived relay selection activates the accepted embedded
  `relay.ScriptV2` only for v2, and Codex/OpenCode custom paths share that seam;
  existing exact-`type` v1 resume starts v1; alive adoption never restarts relay;
  all v2 backend controls use `t` and unchanged semantic tokens; the relay and
  sink physical encoders each round their own raw observation exactly once and
  emit exact three-digit timestamps, while backend callers never pre-round;
  purity guard audits every physical line with no v2 `type` alias
- **Generated artifacts:** none expected; `make check` may verify generated files
  but an actual delta must be explained and separately approved
- **Change budget:** at most 8 production files and 8 tests; header change occurs
  in exactly one constructor/default; no v1 relay edit
- **Boundary:** no migration of existing logs, per-line conversion, public API
  change, or automatic repair; missing-header adoption still fails closed
- **Decision checkpoints:** stop if more than one new-version default appears, if
  existing files need rewriting, or if any producer cannot derive from header
- **Validation intent:** pure new v2 across header, the accepted embedded
  `relay.ScriptV2`, direct writes, provisioning, trailer, and both
  `Task.WriteToLog` branches: every physical record uses `t`,
  every agent timestamp is positive/in-range with exactly three fractional digits,
  and the shared read-only encoder vectors prove below/at/above-half plus second
  carry without caller pre-rounding or duplicated expectations. Resumed v1
  remains byte-compatible with `type`; resumed v2 remains exact `t`; wrong script/
  discriminator selection, v2 `type` meta/control/agent, unknown/mixed files, and
  zero/negative/overflow/out-of-range producer observations fail before append;
  every production physical entry point derives authority/parser/sink behavior
  from the header; prompt and pending-action identities occur exactly once across
  live/attach/replay; taskmeta cannot override the header; all line sizes fit.
  The carry-forward v2 performance contract and V1 compatibility-performance
  invariant hold; the copy gate does not cover producer emission
- **Validation commands:** cwd `/home/user/src/caic`: verify frozen accepted v1/v2
  source/evidence hashes and external artifact path; run focused carry-forward v2
  deterministic/corruption/reopen tests and the resource-qualified canonical
  1 GiB v2 benchmark under the frozen tag/benchstat protocol; run focused v1
  compatibility counting tests, `go test ./backend/internal/agent/...
  ./backend/internal/task/... ./backend/internal/eventreplay/...
  ./backend/internal/server/...`, `python3
  backend/internal/agent/relay/test_relay.py`, and `python3
  backend/internal/agent/relay/test_relay_v2.py`; hash evidence and clean
  transient external artifacts; run the global stale-format audit over all
  physical fixtures/examples/writers under stale-format rule 8; run `make check`,
  then the standard validation footer
- **Review:** fresh full-diff reviewer receives pre-integration SHA, authority
  contract, producer inventory, exact fixtures/artifact delta, and all command
  results; require `PASS` before integration acceptance
- **Exit gate:** automated purity and entry-point audits find `t` as the physical
  discriminator on every v2 line, `caic_meta` only as the unchanged header token,
  exact three-digit rounded timestamps on every v2 agent record, no v2 `type`, no
  bare native line, no `Task.WriteToLog` or other physical append bypass, and no
  reader using alternate authority; controlled existing v1 stays entirely v1;
  the complete carry-forward v2 structural gate and every discriminator/
  timestamp/mismatch corruption test pass; the V1 compatibility-performance
  invariant holds; advisory evidence follows the external
  artifact protocol; full `make check` passes with no unexpected filesystem or
  staged delta
- **Handoff:** report new/existing discriminator and rounding cases, relay
  selection proof, purity/stale-format audits, full validation, generated-state
  check, diffstat, and risks

### Phase 10: Prove real-runtime creation and adoption

- **Stable ID:** `runtime-adoption-smoke`
- **Responsible:** one phase-executor subagent
- **Depends on:** `v2-producer-cutover`
- **May run with:** none
- **Base state:** accepted `v2-producer-cutover` is integrated but unshipped until
  release authority confirms it; apply the global fresh-manifest rule and capture
  md, container, caic binary, and controlled fixture identities
- **VCS authority:** global VCS rule; runtime containers and temporary logs may be
  created only under the smoke cleanup contract
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
  smoke host, the carry-forward v2 performance contract and V1 compatibility-
  performance invariant hold; producer emission remains outside the copy gate
- **Validation commands:** cwd `/home/user/src/caic`: verify frozen accepted v1/v2
  source/evidence hashes and external artifact path; where the smoke host meets
  the resource gate, run focused carry-forward v2 deterministic/corruption/
  reopen tests and the canonical 1 GiB v2 benchmark under the frozen tag/benchstat
  protocol plus focused v1 compatibility tests; otherwise block/escalate rather
  than shrinking the gate. Hash evidence and clean transient external artifacts;
  run `make smoke-wrapped-log`, `make check`, and rerun `make smoke-wrapped-log`;
  audit captured samples under stale-format rule 8; run the standard validation
  footer and verify no runtime, temp, generated, staged, untracked, `coverage.out`,
  or repository performance artifact remains
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
