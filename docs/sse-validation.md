# SSE Schema Validation Plan

## Problem

The frontend receives SSE messages at 4 ingress points, each using a TypeScript
`as` type assertion with zero runtime validation:

| Location | Type asserted | Try/catch |
|---|---|---|
| `sdk/ts/v1/api.gen.ts` `taskEvents` | `as EventMessage` | no |
| `sdk/ts/v1/api.gen.ts` `taskRawEvents` | `as EventMessage` | no |
| `sdk/ts/v1/api.gen.ts` `globalTaskEvents` | `as TaskListEvent` | no |
| `frontend/src/App.tsx:394` (manual ES) | `as TaskListEvent` | yes |
| `frontend/src/App.tsx:478` (manual ES) | `as UsageResp` | yes |

A malformed event with the right `kind` but wrong shape causes runtime
exceptions deep in the UI. The `try/catch` on `JSON.parse` only catches
syntax errors, not structural mismatches.

## End state

Every SSE ingress point validates the parsed JSON against its expected
schema before the payload reaches application code. The validation lives in
the SDK so all consumers (frontend, future clients) benefit.

Validation is auto-generated alongside the types by `gen-api-sdk`, keeping
types and validators permanently in sync.

## Delivery plan

### Step 1: Add TypeScript type generation to `gen-api-sdk`

`gen-api-sdk` (`backend/internal/cmd/gen-api-sdk/main.go`) already generates
`api.gen.ts`, `Types.kt`, `ApiClient.kt`, `Types.swift`, `ApiClient.swift`,
and `API.md`. It already walks every dto struct reachable from route Req/Resp
types and reflects on their fields.

Add a `generateTSTypes()` function that produces `sdk/ts/v1/types.gen.ts`,
replacing `tygo`. The walk is identical to:
- `discoverKotlinStructs()` → Kotlin data classes
- `discoverSwiftStructs()` → Swift structs

**Output format**: TypeScript interfaces with JSDoc comments, matching the
existing `types.gen.ts` structure to avoid breaking consumers.

### Step 2: Add auto-generated validators to `gen-api-sdk`

Add a `generateTSValidate()` function that produces
`sdk/ts/v1/validate.gen.ts`. For each dto struct, generate:

```typescript
export function validateEventMessage(raw: unknown): EventMessage {
    // Check raw is object, kind is string, ts is number.
    // Dispatch on kind to validate the corresponding sub-object.
    // Throw TypeError on mismatch. Unknown kinds pass through.
}
```

Each sub-type gets its own validator checking required fields.
Optional fields are checked for type only when present.

The generator outputs validators for:
- `EventMessage` and all 23 `Event*` sub-types
- `TaskListEvent`
- `UsageResp`

The validators use zero dependencies (no zod, no valibot) — just vanilla
type guards with `typeof` checks.

### Step 3: Wire validation into the generated SSE methods

Update `writeTSSSEMethod()` in gen-api-sdk to emit validated wrappers instead
of raw casts:

```typescript
// Before:
onMessage(JSON.parse(e.data) as EventMessage);

// After:
import { validateEventMessage } from "./validate.gen";
// ...
onMessage((raw) => {
    try {
        onMessage(validateEventMessage(JSON.parse(e.data)));
    } catch (err) {
        console.warn("[caic] invalid SSE event", err);
    }
});
```

### Step 4: Update the manual EventSource handlers in `App.tsx`

Replace the two manual `JSON.parse(e.data) as ...` sites in App.tsx with
calls to the SDK validators. Merge the task list handler into the generated
`globalTaskEvents` path so there's a single ingress point.

### Step 5: Remove `tygo`

Delete `backend/tygo.yaml`. Remove `go tool tygo generate` from the
`go:generate` directive in `dto/v1/types.go`. `gen-api-sdk` now produces
everything.

### Step 6: Verify

- `make types` regenerates `types.gen.ts` and `validate.gen.ts`
- `make lint-frontend` passes
- `make frontend-e2e` passes
- `make test` passes

## Validation strategy

Each validator:
1. Checks the value is a plain object (not null, not array)
2. Checks required fields exist and have the right JavaScript type
3. Checks optional fields have the right type when present
4. Recurses into nested object/array fields
5. Throws `TypeError` with a descriptive message on failure
6. Allows unknown `kind` values (forward-compatible with new event types)

The validators are not full JSON Schema — they validate that required
fields exist with correct runtime types (`string`, `number`, `boolean`,
`object`, `array`), which catches the vast majority of real-world
mismatches.

## Risks

- **Generator complexity**: gen-api-sdk grows. The TypeScript emitter is
  simpler than the Kotlin/Swift ones (no serialization annotations,
  no nullable mapping, no reserve-word escaping), so this is manageable.
- **Performance**: Validation runs on every SSE event. With `typeof` checks
  only (no schema library), the overhead is negligible — under 1µs per
  event for the deepest structs (EventResult containing EventUsage).
- **Breaking tygo consumers**: The only consumer is `types.gen.ts` itself.
  No other code imports tygo output directly except through
  `@sdk/types.gen`.

## Future

Once gen-api-sdk owns TypeScript type generation, it can also generate:
- Kotlin validators for the Android client (same principle)
- Swift validators for iOS clients
- JSON Schema for documentation/testing
