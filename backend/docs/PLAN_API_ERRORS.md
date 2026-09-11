# Goal

All caic REST, MCP, and voice-gateway APIs return stable semantic error codes;
HTTP status remains transport-level classification.

## Phase 1 — completion: Retire ambiguous category-only errors

- **Scope:** Remaining public API handlers and error-construction paths.
- **Preserve:** `BAD_REQUEST` remains only for malformed or intentionally unclassified input.
- **Verify:** The error-construction inventory has no unclassified domain failures and the complete API test suite passes.
