# API Data Transfer Objects

## Dependency Boundary

`api/v1` defines the versioned API contract. It may import the standard
library, this parent `api` package, `github.com/maruel/ksid` for API IDs, and
`apisdkgen` for SDK metadata. It must not import domain, runtime, or other
application packages. Keep cross-layer contract checks in a dependent package,
not in `api/v1` tests.

## API Reachability

Generated SDK methods document the declared API surface; generation alone does
not make a route or symbol used. Treat an API as used only when the frontend
calls it or MCP exposes it. Tests and generated clients do not keep an otherwise
unreferenced API alive.

## Time Fields

Do not use `float64` to represent timestamps. Use `time.Time` instead;
it serializes to ISO 8601 and is unambiguous. Existing `float64` timestamp
fields (`stateUpdatedAt`, `startedAt`, `turnStartedAt`, `cacheExpiresAt`)
are legacy and should not be used as a pattern for new fields.
