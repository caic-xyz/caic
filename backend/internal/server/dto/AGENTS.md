# DTO Guidelines

## Time Fields

Do not use `float64` to represent timestamps. Use `time.Time` instead;
it serializes to ISO 8601 and is unambiguous. Existing `float64` timestamp
fields (`stateUpdatedAt`, `startedAt`, `turnStartedAt`, `cacheExpiresAt`)
are legacy and should not be used as a pattern for new fields.
