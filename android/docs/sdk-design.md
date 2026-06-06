# Kotlin SDK Design

Pure Kotlin module (no Android dependencies) providing a type-safe client for the
caic API. Mirrors the generated TypeScript SDK (`sdk/caic/ts/v1/api.gen.ts`, `sdk/caic/ts/v1/types.gen.ts`).

## Code Generation

`backend/internal/cmd/gen-api-sdk/main.go` emits Kotlin from the same
`v1.Routes` and Go structs used for TypeScript.

Output directory: `sdk/caic/kotlin/src/main/kotlin/com/caic/sdk/v1/`

Two generated files:
- `Types.kt` — data classes, type aliases, constants
- `ApiClient.kt` — suspend functions for JSON endpoints, `Flow<T>` for SSE

See `sdk/caic/API.md` for the full route table and type reference.

### Go → Kotlin Type Mapping

| Go | Kotlin |
|----|--------|
| `string` | `String` |
| `int`, `int64` | `Long` |
| `float64` | `Double` |
| `bool` | `Boolean` |
| `[]T` | `List<T>` |
| `map[string]any` | `Map<String, JsonElement>` |
| `json.RawMessage` | `JsonElement` |
| `ksid.ID` | `String` |
| `*T` (pointer) | `T?` |
| `omitempty` tag | `T? = null` with `@EncodeDefault(NEVER)` |

Field names: use `@SerialName` matching the `json` struct tag.

## Module Setup

The SDK is a pure Kotlin/JVM module at the repository root (`sdk/caic/kotlin/`).
The Android app includes it via a project dependency.

Dependencies:
- `com.squareup.okhttp3:okhttp` — HTTP + SSE + WebSocket
- `org.jetbrains.kotlinx:kotlinx-serialization-json` — JSON (not Gson/Moshi)
- `org.jetbrains.kotlinx:kotlinx-coroutines-core` — async

## Testing

JVM unit tests using `MockWebServer` (OkHttp):

1. **Deserialization**: `listTasks()` returns correct types from JSON fixture
2. **Request body**: `createTask()` sends expected JSON
3. **Error handling**: non-200 → `ApiException` with correct code
4. **SSE**: `taskEvents()` emits `EventMessage` from SSE stream
5. **Round-trip**: every data class serializes/deserializes correctly
