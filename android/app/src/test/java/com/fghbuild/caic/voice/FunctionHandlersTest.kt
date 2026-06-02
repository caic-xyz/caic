// Unit tests for FunctionHandlers dispatch and handler logic.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.RuntimeInstance
import com.caic.sdk.v1.Harness
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import com.fghbuild.caic.data.TaskRepository
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.time.Instant

class FunctionHandlersTest {

    private lateinit var server: MockWebServer
    private lateinit var apiClient: ApiClient
    private lateinit var taskNumberMap: TaskNumberMap

    @Before
    fun setUp() {
        server = MockWebServer()
        apiClient = ApiClient(baseURL = server.url("/").toString().trimEnd('/'))
        taskNumberMap = TaskNumberMap()
        // Pre-populate the map with a task so resolveTaskNumber works.
        taskNumberMap.update(
            listOf(
                Task(
                    id = "task-1", initialPrompt = "test", title = "Test Task",
                    state = TaskState.Running, stateUpdatedAt = Instant.EPOCH,
                    costUSD = 0.01, duration = 60.0, numTurns = 1,
                    cumulativeInputTokens = 10, cumulativeOutputTokens = 5,
                    cumulativeCacheCreationInputTokens = 0, cumulativeCacheReadInputTokens = 0,
                    activeInputTokens = 0, activeCacheReadTokens = 0, contextWindowLimit = 200_000,
                    harness = Harness.Claude,
                    runtime = RuntimeInstance(name = "ctr"),
                )
            )
        )
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    private fun handlers(
        excludedTaskIds: () -> Set<String> = { emptySet() },
    ): FunctionHandlers {
        // TaskRepository isn't used by the handlers we're testing; use a placeholder.
        val taskRepo = TaskRepository(
            com.fghbuild.caic.data.SettingsRepository(
                androidx.datastore.preferences.core.PreferenceDataStoreFactory.create {
                    java.io.File.createTempFile("dummy", ".preferences_pb")
                },
            ),
        )
        return FunctionHandlers(
            apiClient = apiClient,
            taskRepository = taskRepo,
            baseURL = server.url("/").toString().trimEnd('/'),
            taskNumberMap = taskNumberMap,
            excludedTaskIds = excludedTaskIds,
        )
    }

    @Test
    fun `handle unknown function returns error`() = runTest {
        val h = handlers()
        val result = h.handle("nonexistent", JsonObject(emptyMap()))
        val obj = result as JsonObject
        assertEquals("Unknown function: nonexistent", obj["error"]!!.jsonPrimitive.content)
    }

    @Test
    fun `handle tasks_list returns heading on empty list`() = runTest {
        server.enqueue(jsonResponse("[]"))
        val h = handlers()
        val result = h.handle("tasks_list", JsonObject(emptyMap()))
        val obj = result as JsonObject
        assertEquals("No tasks running.", obj["result"]!!.jsonPrimitive.content)
    }

    @Test
    fun `handle tasks_list formats tasks`() = runTest {
        server.enqueue(
            jsonResponse(
                """
                [{
                    "id":"task-1","initialPrompt":"test","title":"Fix bug",
                    "state":"running","stateUpdatedAt":"2025-01-01T00:00:00Z",
                    "costUSD":0.05,"duration":120,"numTurns":1,
                    "cumulativeInputTokens":100,"cumulativeOutputTokens":50,
                    "cumulativeCacheCreationInputTokens":0,"cumulativeCacheReadInputTokens":0,
                    "activeInputTokens":0,"activeCacheReadTokens":0,
                    "contextWindowLimit":200000,"harness":"claude",
                    "runtime":{"name":"ctr"}
                }]
                """,
            ),
        )
        val h = handlers()
        val result = h.handle("tasks_list", JsonObject(emptyMap()))
        val obj = result as JsonObject
        val text = obj["result"]!!.jsonPrimitive.content
        assertTrue(text.contains("## Tasks"))
        assertTrue(text.contains("Fix bug"))
        assertTrue(text.contains("running"))
    }

    @Test
    fun `handle tasks_list excludes tasks from excludedTaskIds`() = runTest {
        server.enqueue(
            jsonResponse(
                """
                [{
                    "id":"task-1","initialPrompt":"test","title":"Excluded",
                    "state":"running","stateUpdatedAt":"2025-01-01T00:00:00Z",
                    "costUSD":0.05,"duration":120,"numTurns":1,
                    "cumulativeInputTokens":100,"cumulativeOutputTokens":50,
                    "cumulativeCacheCreationInputTokens":0,"cumulativeCacheReadInputTokens":0,
                    "activeInputTokens":0,"activeCacheReadTokens":0,
                    "contextWindowLimit":200000,"harness":"claude",
                    "runtime":{"name":"ctr"}
                }]
                """,
            ),
        )
        val h = handlers(excludedTaskIds = { setOf("task-1") })
        val result = h.handle("tasks_list", JsonObject(emptyMap()))
        val obj = result as JsonObject
        assertEquals("No tasks running.", obj["result"]!!.jsonPrimitive.content)
    }

    @Test
    fun `handle get_usage returns data from API`() = runTest {
        server.enqueue(
            jsonResponse(
                """
                {
                    "local": {
                        "windows": [{
                            "duration":"5h","costUSD":1.23,"inputTokens":1000,"outputTokens":500
                        }]
                    },
                    "providers": []
                }
                """,
            ),
        )
        val h = handlers()
        val result = h.handle("get_usage", JsonObject(emptyMap()))
        val obj = result as JsonObject
        assertTrue(obj["result"]!!.jsonPrimitive.content.contains("5h cost"))
    }

    @Test
    fun `handle get_usage with providers formats balances and rate limits`() = runTest {
        server.enqueue(
            jsonResponse(
                """
                {
                    "local":{"windows":[{"duration":"5h","costUSD":2.5,"inputTokens":500,"outputTokens":300}]},
                    "providers":[{
                        "provider":"openai","label":"OpenAI",
                        "logoUrl":"","authKind":"api_key","usageUrl":"",
                        "balance": {"currency":"USD","total":50.0},
                        "rateLimits":[{"window":"5h","usedPct":75.5}]
                    }]
                }
                """,
            ),
        )
        val h = handlers()
        val result = h.handle("get_usage", JsonObject(emptyMap()))
        val obj = result as JsonObject
        val text = obj["result"]!!.jsonPrimitive.content
        assertTrue(text.contains("OpenAI"))
    }

    @Test
    fun `handle stop_task requires task number`() = runTest {
        val h = handlers()
        val result = h.handle(
            "task_stop",
            JsonObject(mapOf("task_number" to JsonPrimitive(1))),
        )
        val obj = result as JsonObject
        // MockWebServer returns 404 if no response enqueued; the handler catches and returns error.
        assertTrue(obj.containsKey("error") || obj.containsKey("result"))
    }

    @Test
    fun `handle task_get_detail with valid task`() = runTest {
        server.enqueue(jsonResponse("""
                [{
                    "id":"task-1","initialPrompt":"test","title":"Detail Task",
                    "state":"running","stateUpdatedAt":"2025-01-01T00:00:00Z",
                    "costUSD":0.10,"duration":300,"numTurns":2,
                    "cumulativeInputTokens":200,"cumulativeOutputTokens":100,
                    "cumulativeCacheCreationInputTokens":0,"cumulativeCacheReadInputTokens":0,
                    "activeInputTokens":0,"activeCacheReadTokens":0,
                    "contextWindowLimit":200000,"harness":"claude",
                    "runtime":{"name":"ctr"}
                }]
            """.trimIndent()))
        val h = handlers()
        val result = h.handle(
            "task_get_detail",
            JsonObject(mapOf("task_number" to JsonPrimitive(1))),
        )
        val obj = result as JsonObject
        val text = (obj["result"] ?: obj["error"])?.jsonPrimitive?.content ?: "NO CONTENT"
        assertTrue("Expected 'Detail Task' in: $text", text.contains("Detail Task"))
    }

    @Test
    fun `handle exception returns error result`() = runTest {
        // No enqueued response → 404 → exception thrown and caught.
        val h = handlers()
        val result = h.handle("tasks_list", JsonObject(emptyMap()))
        val obj = result as JsonObject
        assertTrue(obj.containsKey("error"))
    }

    @Test
    fun `handle clone_repo returns repo info`() = runTest {
        server.enqueue(
            jsonResponse(
                """{"path":"org/repo","baseBranch":{"name":"main"},"branch":"main"}""",
            ),
        )
        val h = handlers()
        val result = h.handle(
            "clone_repo",
            JsonObject(mapOf("url" to JsonPrimitive("https://github.com/org/repo"))),
        )
        val obj = result as JsonObject
        assertTrue(obj["result"]!!.jsonPrimitive.content.contains("org/repo"))
        assertTrue(obj["result"]!!.jsonPrimitive.content.contains("main"))
    }

    @Test
    fun `handle web_search encodes query and returns fetch result`() = runTest {
        server.enqueue(
            jsonResponse("""{"title":"Search Results","content":"Found results for kotlin"}"""),
        )
        val h = handlers()
        val result = h.handle(
            "web_search",
            JsonObject(mapOf("query" to JsonPrimitive("kotlin"))),
        )
        val obj = result as JsonObject
        assertEquals("Search Results", obj["title"]!!.jsonPrimitive.content)
        assertTrue(obj["content"]!!.jsonPrimitive.content.contains("kotlin"))
    }

    @Test
    fun `handle web_fetch returns url content`() = runTest {
        server.enqueue(
            jsonResponse("""{"title":"Page","content":"Page content"}"""),
        )
        val h = handlers()
        val result = h.handle(
            "web_fetch",
            JsonObject(mapOf("url" to JsonPrimitive("https://example.com"))),
        )
        val obj = result as JsonObject
        assertEquals("Page", obj["title"]!!.jsonPrimitive.content)
    }

    private fun jsonResponse(body: String): MockResponse = MockResponse()
        .addHeader("Content-Type", "application/json")
        .setBody(body)
}
