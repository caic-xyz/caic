# Kotlin SDK Design

Pure Kotlin module (no Android dependencies) providing a type-safe client for the
caic API. Mirrors the generated TypeScript SDK (`sdk/api.gen.ts`, `sdk/types.gen.ts`).

## Code Generation

Extend `backend/internal/cmd/gen-api-client/main.go` with `--lang=kotlin` to emit
Kotlin from the same `dto.Routes` and Go structs used for TypeScript.

Output directory: `android/sdk/src/main/kotlin/com/caic/sdk/`

Two generated files:
- `Types.kt` — data classes, type aliases, constants
- `ApiClient.kt` — suspend functions for JSON endpoints, `Flow<EventMessage>` for SSE

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

### Build Integration

Add to `Makefile` `types` target:
```makefile
go run ./backend/internal/cmd/gen-api-client --lang=kotlin \
    --out=android/sdk/src/main/kotlin/com/caic/sdk
```

## Module Setup

### Gradle

`android/settings.gradle.kts`: add `include(":sdk")`

`android/sdk/build.gradle.kts`: pure Kotlin/JVM module.

Dependencies:
- `com.squareup.okhttp3:okhttp:4.12.0`
- `com.squareup.okhttp3:okhttp-sse:4.12.0`
- `org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3`
- `org.jetbrains.kotlinx:kotlinx-coroutines-core:1.9.0`

App module: `implementation(project(":sdk"))`

## Generated Types (`Types.kt`)

### Constants and Aliases

```kotlin
typealias Harness = String
object Harnesses {
    const val Claude: Harness = "claude"
    const val Gemini: Harness = "gemini"
}

typealias EventKind = String
object EventKinds {
    const val Init: EventKind = "init"
    const val Text: EventKind = "text"
    const val ToolUse: EventKind = "toolUse"
    const val ToolResult: EventKind = "toolResult"
    const val Ask: EventKind = "ask"
    const val Usage: EventKind = "usage"
    const val Result: EventKind = "result"
    const val System: EventKind = "system"
    const val UserInput: EventKind = "userInput"
    const val Todo: EventKind = "todo"
}

object ErrorCodes {
    const val BadRequest = "BAD_REQUEST"
    const val NotFound = "NOT_FOUND"
    const val Conflict = "CONFLICT"
    const val InternalError = "INTERNAL_ERROR"
}
```

### Data Classes

Core request/response types, event payloads — all `@Serializable`.
See the TypeScript types in `sdk/types.gen.ts` as the canonical reference.

Key types: `TaskJSON`, `RepoJSON`, `HarnessJSON`, `CreateTaskReq`, `CreateTaskResp`,
`InputReq`, `RestartReq`, `SyncReq`, `SyncResp`, `SafetyIssue`, `StatusResp`,
`UsageResp`, `UsageWindow`, `ExtraUsage`, `DiffFileStat`.

Event types: `EventMessage` (discriminated on `kind`), `EventInit`, `EventText`,
`EventToolUse`, `EventToolResult`, `EventAsk`, `AskQuestion`, `AskOption`,
`EventUsage`, `EventResult`, `EventSystem`, `EventUserInput`, `EventTodo`, `TodoItem`.

Voice types: `VoiceTokenResp`.

Error types: `ErrorResponse`, `ErrorDetails`.

## API Client (`ApiClient.kt`)

Constructor: `ApiClient(baseURL: String)`

Internal: `OkHttpClient`, `Json { ignoreUnknownKeys = true }`

### JSON Endpoints

Generated `suspend` functions, one per `dto.Route`:

| Method | Path | Function | Request | Response |
|--------|------|----------|---------|----------|
| GET | `/api/v1/harnesses` | `listHarnesses()` | — | `List<HarnessJSON>` |
| GET | `/api/v1/repos` | `listRepos()` | — | `List<RepoJSON>` |
| GET | `/api/v1/tasks` | `listTasks()` | — | `List<TaskJSON>` |
| POST | `/api/v1/tasks` | `createTask(req)` | `CreateTaskReq` | `CreateTaskResp` |
| POST | `/api/v1/tasks/{id}/input` | `sendInput(id, req)` | `InputReq` | `StatusResp` |
| POST | `/api/v1/tasks/{id}/restart` | `restartTask(id, req)` | `RestartReq` | `StatusResp` |
| POST | `/api/v1/tasks/{id}/terminate` | `terminateTask(id)` | — | `StatusResp` |
| POST | `/api/v1/tasks/{id}/sync` | `syncTask(id, req)` | `SyncReq` | `SyncResp` |
| GET | `/api/v1/usage` | `getUsage()` | — | `UsageResp` |
| GET | `/api/v1/voice/token` | `getVoiceToken()` | — | `VoiceTokenResp` |

### SSE Endpoints

Return `Flow<EventMessage>`, using OkHttp SSE:

| Path | Function |
|------|----------|
| `/api/v1/tasks/{id}/events` | `taskEvents(id): Flow<EventMessage>` |
| `/api/v1/events` | `globalEvents(): Flow<GlobalEvent>` (task list + usage) |

### SSE Reconnection

Wrapper `taskEventsReconnecting()` / `globalEventsReconnecting()`:
- Initial delay: 500ms
- Backoff factor: 1.5x
- Max delay: 4s
- Reset delay on successful event receipt
- Rethrow `CancellationException`

This matches the web frontend's backoff logic in `App.tsx` and `TaskView.tsx`.

### Error Handling

`ApiException(statusCode, code, message, details?)` thrown for non-200 responses.
Callers branch on `code` (from `ErrorCodes`).

## Voice Token Endpoint (Backend)

New backend endpoint that proxies ephemeral token creation from the Gemini API.
The Gemini API key stays on the server; the app only receives short-lived tokens.

### Backend: `GET /api/v1/voice/token`

Response:
```json
{
  "token": "auth_tokens/4969d96da97639001864...",
  "expiresAt": "2025-06-15T12:30:00Z"
}
```

Go types:
```go
type VoiceTokenResp struct {
    Token     string `json:"token"`
    ExpiresAt string `json:"expiresAt"`
}
```

Backend implementation:
1. Read Gemini API key from environment (`GEMINI_API_KEY`)
2. POST to `https://generativelanguage.googleapis.com/v1alpha/auth_tokens`
   with `x-goog-api-key` header
3. Request body: `{"config": {"uses": 1, "expireTime": "<now+30m>", "newSessionExpireTime": "<now+2m>"}}`
4. Return the token name and expiry to the app

Add to `dto.Routes`:
```go
{Name: "getVoiceToken", Method: "GET", Path: "/api/v1/voice/token", RespType: "VoiceTokenResp"},
```

Kotlin type (generated):
```kotlin
@Serializable
data class VoiceTokenResp(
    val token: String,
    val expiresAt: String,
)
```

## Testing

JVM unit tests using `MockWebServer` (OkHttp):

1. **Deserialization**: `listTasks()` returns correct types from JSON fixture
2. **Request body**: `createTask()` sends expected JSON
3. **Error handling**: non-200 → `ApiException` with correct code
4. **SSE**: `taskEvents()` emits `EventMessage` from SSE stream
5. **Round-trip**: every data class serializes/deserializes correctly

## References

### caic source (canonical)
- `backend/internal/server/dto/routes.go` — route declarations (source of truth for code generation)
- `backend/internal/server/dto/types.go` — Go request/response types
- `backend/internal/server/dto/events.go` — SSE event types
- `backend/internal/cmd/gen-api-client/main.go` — TypeScript code generator (extend for Kotlin)
- `sdk/types.gen.ts` — generated TypeScript types (reference for Kotlin output)
- `sdk/api.gen.ts` — generated TypeScript API client (reference for Kotlin output)

### Ephemeral tokens
- [Ephemeral tokens docs](https://ai.google.dev/gemini-api/docs/ephemeral-tokens) — token creation API, expiration, constraints
- [python-genai tokens.py](https://github.com/googleapis/python-genai/blob/main/google/genai/tokens.py) — reference implementation of `auth_tokens.create()`
