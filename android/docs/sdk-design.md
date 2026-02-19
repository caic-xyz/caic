# Kotlin SDK Design

Pure Kotlin module (no Android dependencies) providing a type-safe client for the
caic API. Mirrors the generated TypeScript SDK (`sdk/ts/api.gen.ts`, `sdk/ts/types.gen.ts`).

## Code Generation

`backend/internal/cmd/gen-api-sdk/main.go` emits Kotlin from the same
`dto.Routes` and Go structs used for TypeScript.

Output directory: `sdk/kotlin/src/main/kotlin/com/caic/sdk/`

Two generated files:
- `Types.kt` — data classes, type aliases, constants
- `ApiClient.kt` — suspend functions for JSON endpoints, `Flow<T>` for SSE

See `sdk/API.md` for the full route table and type reference.

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

### Gradle

`android/settings.gradle.kts`: add `include(":sdk")`

`sdk/kotlin/build.gradle.kts`: pure Kotlin/JVM module.

Dependencies:
- `com.squareup.okhttp3:okhttp:4.12.0`
- `com.squareup.okhttp3:okhttp-sse:4.12.0`
- `org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3`
- `org.jetbrains.kotlinx:kotlinx-coroutines-core:1.9.0`

App module: `implementation(project(":sdk"))`

## Voice Token Design

The `GET /api/v1/voice/token` endpoint returns a Gemini API credential. The
`ephemeral` flag tells the client which WebSocket endpoint and auth parameter
to use.

**Current mode: raw key (`ephemeral: false`)**

Returns the raw `GEMINI_API_KEY`. The client connects to
`v1beta.GenerativeService.BidiGenerateContent` with `?key=`. This produces
higher-quality voice responses but exposes the API key to the client.

**Ephemeral mode (`ephemeral: true`) — disabled, kept for future use**

Creates a short-lived token via POST `/v1alpha/auth_tokens` with
`x-goog-api-key` header (ephemeral tokens are **v1alpha only**; `v1beta`
returns 404). The client must connect to
`v1alpha.GenerativeService.BidiGenerateContentConstrained` with
`?access_token=`. This is more secure but currently produces lower-quality
responses.

## Testing

JVM unit tests using `MockWebServer` (OkHttp):

1. **Deserialization**: `listTasks()` returns correct types from JSON fixture
2. **Request body**: `createTask()` sends expected JSON
3. **Error handling**: non-200 → `ApiException` with correct code
4. **SSE**: `taskEvents()` emits `EventMessage` from SSE stream
5. **Round-trip**: every data class serializes/deserializes correctly

## References

- [Ephemeral tokens docs](https://ai.google.dev/gemini-api/docs/ephemeral-tokens) — token creation API, expiration, constraints
- [python-genai tokens.py](https://github.com/googleapis/python-genai/blob/main/google/genai/tokens.py) — reference implementation of `auth_tokens.create()`
