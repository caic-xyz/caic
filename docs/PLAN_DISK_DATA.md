# Independent data contracts for persisted files

Move the remaining cache and durable-log schemas into data-only packages,
independent of runtime and API types, while preserving existing file formats.

## Boundaries

- Each file format owns its complete nested representation under its owner's
  `data` package. Duplication across runtime, API, and disk types, and across
  disk versions, is intentional; do not use aliases or embed runtime structs.
- Name schema files `data.go`. Data packages import only the standard library
  and contain data declarations plus serialization methods when necessary.
  Validation, defaults, migrations, I/O, hashing, TTLs, folds, and conversions
  stay with behavioral owners.
- Use `/vN` for existing format versions. Keep `currentVersion` local to the
  persistence owner; data packages do not export a `Version` constant. Preserve
  field names, omission rules, null/empty distinctions, units, discriminators,
  and version placement. Do not add or bump on-disk versions for extraction.
- Genai types and native harness payloads remain out of scope; caic envelopes
  retain opaque native JSON. External formats, opaque bytes, generated source,
  and in-memory caches do not require new disk schemas.

## Phase 1 — task-history: Explicit versioned task-log and header contracts

- **Scope:** Extract caic-owned [task-log records](../backend/internal/agent/types.go)
  into `taskslog/data/v1`, `/v2`, and `/v3`, and the
  [header-cache snapshot](../backend/internal/taskslog/log_header_cache.go)
  into `taskslog/data/headercache/v6`. Replace direct serialization of agent
  messages and `LoadedTask` with explicit projections. Cover Go writers,
  Python relays, replay loaders, and log-validation tooling; keep parsing,
  state application, and task `State`/`Result` behavior outside data packages.
- **Preserve:** V1 field order where contractual, strict v2/v3 decoding,
  canonical envelope bytes, timestamp precision, record-size bounds,
  generation-local relay offsets, pending actions, and restart adoption.
- **Verify:** Historical fixtures for all three log versions restore tasks and
  results; Go and relay fixtures agree on physical records. Existing v6 caches
  load without a version bump and rebuild correctly when invalid. Run agent,
  taskslog, task/taskmgr, relay, and check-agent-logs tooling tests. Compare
  focused parse/adoption benchmarks before and after.

## Phase 2 — durable-observations: Independent usage, metrics, and audit records

- **Scope:** Extract [usage rows](../backend/internal/usagedb/usagedb.go) into
  `usagedb/data`, [metrics records](../metricsdb/log.go) into `metricsdb/data/v2`,
  and [audit events](../backend/internal/server/audit.go) into `server/data/audit`,
  including nested records and scalar types. Keep aggregation, event inputs,
  dashboard projections, and backfill logic with runtime owners; convert at
  storage boundaries.
- **Preserve:** Restart watermarks, delta semantics, missing-cost repair,
  unknown-field preservation during backfill, compression, and metrics header
  acceptance rules.
- **Verify:** Historical rows recover identical aggregates and survive late
  writes, compression, restart, and cost repair. Audit output retains its shape.
  Run usagedb, metricsdb, and audit tests; compare focused usage-ingest and
  metric-append benchmarks before and after.

## Phase 3 — oauth-owner: Complete the dependency-owned persistence boundary

- **Scope:** In gomode's `oauth/oauthserver/storage.go`, extract the OAuth
  store's supported versioned schemas and nested records. Keep migrations and
  authorization behavior in oauthserver; consume the released dependency in
  caic. Gomode owns the writer, so copying its schema into caic is insufficient.
- **Preserve:** Hashed secrets/user codes, grant/token-family lifecycle,
  signing-key rotation, replay protection, and supported historical migrations.
- **Verify:** Gomode historical-store and token-lifecycle tests preserve
  authorization across save/restart/migration; caic auth/MCP integration tests
  pass with the updated dependency. Audit all caic-owned structured disk writers
  to confirm they use independent data types with no domain dependencies.
