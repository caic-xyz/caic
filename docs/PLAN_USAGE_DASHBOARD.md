# Cross-Task Usage Dashboard from Append-Only Rollups

A `/usage` dashboard, separate from Settings, showing tokens/day per model,
turns, cache hit rate, skill reads, cost/day, and per-harness/repo
leaderboards. Data comes from a new append-only daily JSONL usage rollup fed
by the backend's existing task message dispatch. Legacy v1 task logs (no
record timestamps) are out of scope for attribution.

Plan-level constraints:

- Usage rollup files are append-only JSONL, one file per day under the
  existing caic data directory. Deltas are keyed by producer-time day, not
  flush time. Readers tolerate a truncated trailing line.
- The rollup is the only cross-task aggregation surface; task logs stay the
  per-task record of truth and are never scanned by dashboard requests.
- Task-log JSON schemas stay backward-compatible with released binaries
  (additive fields and new message types only).

## Phase 1 — usage-dashboard-api: Dashboard query API and SDK

- **Depends on:** rollup-hardening
- **Scope:** `backend/internal/server` (new handler + route under
  `/api/caic/v1/usage/dashboard`), `apisdkgen` DTOs, regenerated
  `sdk/caic/ts/v1/types.gen.ts`.
- **Preserve:** existing `GET /usage` quota snapshot behavior unchanged;
  auth middleware applies to the new route; DTO conversion lives in
  `apiconv` with the existing patterns.
- **Verify:** handler tests over a seeded rollup; `make refresh-generated`;
  `make fix && make verify`.

## Phase 2 — usage-dashboard-ui: `/usage` dashboard page

- **Depends on:** usage-dashboard-api
- **Scope:** `frontend/src` (new route in `routes.tsx`, page component,
  chart components reusing `StatsCharts` patterns), e2e coverage, and a
  day-range selector, provider quota panel, and data-since indicator.
- **Preserve:** per-task `/task/:id/stats` stays as-is; styling follows
  `lint_frontend_styles` token rules; the dashboard is reachable without
  touching Settings.
- **Verify:** `make test-e2e` with a fake-backend dashboard fixture,
  including a mobile-viewport spec matching the existing stats-mobile
  pattern; `make fix && make verify`.
