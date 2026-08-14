# Replay architecture improvements

This document records architectural directions discovered while hardening
wrapped task logs and replay-sidecar lifecycle. The primary direction is
implemented; the remaining sections are deliberately proposals, not an
incremental implementation plan. The raw task log remains the sole persistent
authority; every replay artifact is replaceable derived data.

## Primary direction: terminal and on-demand replay caches

Do not maintain replay sidecars while a task is active. Live task SSE already
has in-memory messages and dispatch. Build a replay cache when a task becomes
terminal, or on the first history request after server restart.

This removes live cache seeding, append proofs, replay-writer attachment,
restart races, spool ownership for active writers, and cache commit failures
from normal task execution. A missing cache is an ordinary cache miss, rebuilt
from the verified raw log. Stopped-but-revivable tasks receive a cache only
while their session is ended; a later revival invalidates it by appending to the
raw log.

## Separate storage from API conversion

Terminal publication and cache-miss regeneration run through the server replay
bridge. The manager reloads and validates the raw source, then calls its
required publisher dependency; app only composes that dependency.

`eventreplay` owns generic proof-bound sidecar storage: paths, compression,
atomic publication, validated byte reads, and SSE framing. The server bridge
owns parsed-message scanning, delta compaction, and `apiconv` conversion to the
current API event schema. Storage accepts the schema version and line validator
from that bridge, so it has no agent or API dependency.

This keeps task lifecycle independent of server packages while making API
version changes an explicit cache-schema concern.

## Make unavailable live replay explicit

If live replay remains, model the result explicitly instead of translating
sentinel errors across `eventreplay`, `app`, and `task`.

A construction result should distinguish:

- attached: a complete sidecar was seeded and may receive append-only updates;
- unavailable: the raw log contains history without a complete sidecar, so
  replay must be regenerated later;
- failed: storage, validation, or authority observation failed unexpectedly.

Only the last case should be operationally noisy. The unavailable case is safe
and expected after a server was unavailable while a relay continued producing
output.

## Make authority values explicit inputs

`Task` should own task state and its validated log identity, but it should not
select proof policy for every caller. Lifecycle owners should pass a named,
immutable authority value to the operation they are starting:

- a validated EOF snapshot for adopted-history reads and replay regeneration;
- a current bounded header/identity observation for a local live append writer;
- a completed semantic-scan proof for publishing a regenerated cache.

This eliminates provider callbacks whose behavior depends on call site and
makes proof transitions inspectable in tests.

## Define one task-log lifecycle owner

The manager currently coordinates adoption, sessions, terminal trailers,
compression, and replay attachment across several packages. Introduce a
small lifecycle component that owns state transitions involving persistent
files:

1. open or re-open raw log;
2. append controls and native records;
3. close a session and append terminal metadata;
4. publish or invalidate derived artifacts;
5. atomically compress an eligible terminal log.

Session code should only emit parsed messages. HTTP replay code should only
request verified history. Neither should need to know when a raw log was
replaced with its compressed form.

## Unify derived-data publication

Task summaries, replay sidecars, and compressed logs all derive from a raw log
and use separate publication transactions. Define a shared derived-artifact
protocol with:

- input authority (identity, header, and relevant EOF/scan proof);
- a temporary artifact path outside the source namespace;
- full validation before publication;
- a final authority recheck immediately before rename;
- deletion only of artifacts whose temporary identity is known.

The common protocol would prevent subtle differences in how each cache handles
source mutation, cancellation, and cleanup.

## Separate startup inventory from maintenance

Startup currently needs both a stable inventory for adoption and filesystem
maintenance such as compression and stale-cache pruning. Make the boundary
explicit:

1. inventory and validate raw logs;
2. finish only maintenance that cannot race live writers;
3. create the task manager and attach/adopt live sessions;
4. run later maintenance only through lifecycle-aware ownership.

Do not run a generic directory sweeper after live replay writers may exist.

## Isolate smoke runtime inventory

Smoke uses a temporary caic cache, but the underlying `md` runtime inventory
can expose containers created outside the test. Require a unique smoke-run
label and use it for discovery, adoption, diagnostics, and cleanup. Ideally,
smoke also uses a dedicated `md` state directory.

A smoke failure should report only resources belonging to its run; a successful
run should be able to assert that no labeled containers, logs, or cache entries
remain.

## Add fault-injection integration coverage

Most replay correctness risks occur between individually unit-tested steps.
Add a package-level integration fixture that can interrupt at lifecycle
boundaries:

- after raw append and before replay publication;
- after replay compression and before rename;
- after terminal trailer and before raw-log compression;
- after server stop while the relay writes output;
- while startup pruning scans an abandoned spool.

Each case should restart the application and assert either a verified replay or
a safe cache miss that regenerates from the raw log. This is more valuable than
adding more unit tests around individual proof comparisons.

## Bound retention by product policy

Log compression, replay caching, summary caching, and task purge are currently
technical lifecycle choices. Define a product-level retention policy with
separate horizons for:

- active/revivable task raw logs;
- terminal raw logs;
- derived replay and summary caches;
- purged task metadata;
- runtime/container resources.

Then implement one retention coordinator. It gives operators predictable disk
behavior and prevents cleanup code from making implicit product decisions.
