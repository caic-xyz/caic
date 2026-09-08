# Goal

All caic REST, MCP, and voice-gateway APIs return stable semantic error codes;
HTTP status remains transport-level classification.

## Phase 1 — taxonomy: Define the shared error-code catalog

- **Scope:** API error contract, versioned API specifications, generated SDKs, and API reference documentation.
- **Preserve:** Released semantic codes and their HTTP statuses remain stable.
- **Verify:** Every catalog entry is represented in generated SDKs and has an API-contract test.

## Phase 2 — task-repository: Migrate task, repository, runtime, and CI failures

- **Depends on:** taxonomy
- **Scope:** Task lifecycle, repository management, runtime, model, harness, and CI endpoints and MCP tools.
- **Preserve:** MCP recovery is offered only when the client has the capability required to resolve the failure.
- **Verify:** REST, MCP, and browser tests assert semantic codes and their intended recovery behavior.

## Phase 3 — access-configuration: Migrate identity, authorization, and server-configuration failures

- **Depends on:** taxonomy
- **Scope:** Authentication, OAuth grants, preferences, updates, caches, and server configuration endpoints.
- **Preserve:** Authorization failures do not reveal unavailable resources or credentials.
- **Verify:** API contract tests cover unauthenticated, unauthorized, invalid, and unavailable states.

## Phase 4 — gateway-clients: Align voice gateway and generated clients

- **Depends on:** taxonomy, task-repository, access-configuration
- **Scope:** Voice-gateway API, MCP client surfaces, and generated TypeScript, Kotlin, and Swift SDKs.
- **Preserve:** Client behavior remains compatible with published error envelopes during each migration.
- **Verify:** Generated-client type checks and gateway/MCP integration tests cover every semantic code they consume.

## Phase 5 — completion: Retire ambiguous category-only errors

- **Depends on:** task-repository, access-configuration, gateway-clients
- **Scope:** Remaining public API handlers and error-construction paths.
- **Preserve:** `BAD_REQUEST` remains only for malformed or intentionally unclassified input.
- **Verify:** The error-construction inventory has no unclassified domain failures and the complete API test suite passes.
