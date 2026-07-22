// Unit tests for Go Mode service monitoring resource orchestration.
package com.fghbuild.gomode.service

import com.fghbuild.gomode.sdk.v1.Settings
import com.fghbuild.gomode.sdk.v1.ToolGroup
import com.fghbuild.gomode.sdk.v1.VoiceGatewaySettings
import com.fghbuild.gomode.sdk.v1.WebShellSettings
import com.fghbuild.gomode.voice.McpClient
import com.fghbuild.mcp.sdk.v1.CacheScope
import com.fghbuild.mcp.sdk.v1.JSONRPCNotification
import com.fghbuild.mcp.sdk.v1.NotificationMethod
import com.fghbuild.mcp.sdk.v1.ResourceContent
import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import com.fghbuild.mcp.sdk.v1.ResultType
import com.fghbuild.mcp.sdk.v1.SubscriptionFilter
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import okhttp3.mockwebserver.SocketPolicy
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException

@OptIn(ExperimentalCoroutinesApi::class)
class ServiceMonitorTest {
    @Test
    fun `generic items update native attention notification and voice context state`() = runTest {
        val client = FakeServiceResourceClient().apply {
            enqueueReadResult(
                """
                    [
                      {"id":"i1","title":"Build feature","state":"active","needsAttention":false},
                      {"id":"i2","title":"Review plan","state":"awaiting input","needsAttention":true}
                    ]
                """.trimIndent(),
            )
        }
        var endpointURL: String? = null
        var protocolVersion: String? = null
        val monitor = ServiceMonitor(this) { endpoint, protocol ->
            endpointURL = endpoint
            protocolVersion = protocol
            client
        }

        monitor.start("https://service.test/mobile", serviceSettings())
        advanceUntilIdle()

        val state = monitor.state.value
        assertEquals("https://service.test/api/service/v1/mcp", endpointURL)
        assertEquals("2026-07-28", protocolVersion)
        assertEquals(1, state.attentionCount)
        assertEquals("Review plan needs attention", state.notificationText)
        assertTrue(state.voiceContext?.contains("Build feature: active") == true)
        assertTrue(state.voiceContext?.contains("Review plan: awaiting input needs attention") == true)
        monitor.stop()
    }

    @Test
    fun `resource update notifications re-read resources and update state`() = runTest {
        val client = FakeServiceResourceClient().apply {
            enqueueReadResult("""[{"id":"i1","title":"Build feature","state":"active","needsAttention":false}]""")
            enqueueReadResult("""[{"id":"i2","title":"Fix tests","state":"failed","needsAttention":true,"needsAttention":true}]""")
        }
        val monitor = ServiceMonitor(this) { _, _ -> client }
        monitor.start("https://service.test", serviceSettings())
        advanceUntilIdle()

        client.emit(resourceUpdated(GoModeItemsResourceURI))
        advanceUntilIdle()

        assertEquals(listOf(GoModeItemsResourceURI, GoModeItemsResourceURI), client.readURIs)
        assertEquals("Fix tests needs attention", monitor.state.value.notificationText)
        assertEquals(listOf(GoModeItemsResourceURI), client.subscriptionFilters.single().resourceSubscriptions)
        assertEquals(true, client.subscriptionFilters.single().resourcesListChanged)
        monitor.stop()
        advanceUntilIdle()
        assertTrue(client.subscriptionCancelled)
    }

    @Test
    fun `generic service notifications are delivered once`() = runTest {
        val client = FakeServiceResourceClient().apply {
            resources = listOf(
                ResourceDescriptor(uri = GoModeItemsResourceURI, name = "items", mimeType = "application/json"),
                ResourceDescriptor(uri = GoModeNotificationsResourceURI, name = "notifications", mimeType = "application/json"),
            )
            enqueueReadResult("""[{"id":"i1","title":"Build feature","state":"awaiting input","needsAttention":true}]""")
            enqueueNotificationReadResult("[]")
            enqueueReadResult("""[{"id":"i1","title":"Build feature","state":"awaiting input","needsAttention":true}]""")
            enqueueNotificationReadResult("""[{"id":"event-1","title":"Item ready","body":"Build feature needs your input."}]""")
            enqueueReadResult("""[{"id":"i1","title":"Build feature","state":"awaiting input","needsAttention":true}]""")
            enqueueNotificationReadResult("""[{"id":"event-1","title":"Item ready","body":"Build feature needs your input."}]""")
        }
        val monitor = ServiceMonitor(this) { _, _ -> client }

        monitor.start("https://service.test", serviceSettings())
        advanceUntilIdle()
        client.emit(resourceUpdated(GoModeNotificationsResourceURI))
        advanceUntilIdle()
        assertEquals(listOf("Item ready"), monitor.state.value.notifications.map { it.title })

        client.emit(resourceUpdated(GoModeNotificationsResourceURI))
        advanceUntilIdle()
        assertTrue(monitor.state.value.notifications.isEmpty())
        monitor.stop()
    }

    @Test
    fun `unsupported resources disable monitoring without subscribing`() = runTest {
        val client = FakeServiceResourceClient().apply {
            resources = listOf(ResourceDescriptor(uri = "service://other", name = "other", mimeType = "application/json"))
        }
        val monitor = ServiceMonitor(this) { _, _ -> client }

        monitor.start("https://service.test", serviceSettings())
        advanceUntilIdle()

        assertNull(monitor.state.value.snapshot)
        assertNull(monitor.state.value.notificationText)
        assertTrue(client.subscriptionFilters.isEmpty())
        monitor.stop()
    }

    @Test
    fun `startup failures retry and recover`() = runTest {
        val client = FakeServiceResourceClient().apply {
            listFailuresRemaining = 1
            enqueueReadResult("""[{"id":"t1","title":"Build feature","state":"active","needsAttention":false}]""")
        }
        val monitor = ServiceMonitor(this) { _, _ -> client }

        monitor.start("https://service.test", serviceSettings())
        runCurrent()
        assertEquals("HTTP 401", monitor.state.value.error)
        assertNull(monitor.state.value.snapshot)

        advanceTimeBy(1000)
        runCurrent()

        assertEquals(2, client.listCalls)
        assertNull(monitor.state.value.error)
        assertEquals("Build feature", monitor.state.value.snapshot?.items?.single()?.title)
        monitor.stop()
    }

    @Test
    fun `closed subscription streams clear stale state and retry`() = runTest {
        val client = FakeServiceResourceClient().apply {
            closeSubscriptionsImmediately = true
            enqueueReadResult("""[{"id":"t1","title":"Build feature","state":"active","needsAttention":false}]""")
            enqueueReadResult("""[{"id":"t2","title":"Fix tests","state":"failed","needsAttention":true}]""")
        }
        val monitor = ServiceMonitor(this) { _, _ -> client }

        monitor.start("https://service.test", serviceSettings())
        runCurrent()

        assertEquals("MCP subscription stream ended", monitor.state.value.error)
        assertNull(monitor.state.value.snapshot)
        assertEquals(1, client.readURIs.size)

        advanceTimeBy(1000)
        runCurrent()

        assertEquals(2, client.readURIs.size)
        assertEquals("MCP subscription stream ended", monitor.state.value.error)
        assertNull(monitor.state.value.snapshot)
        monitor.stop()
    }

    @Test
    fun `real mcp client monitoring sends resource name header`() = runBlocking {
        val server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.getHeader("Mcp-Method")) {
                "resources/list" -> jsonResponse(RESOURCES_LIST_JSON)
                "resources/read" -> {
                    if (request.getHeader("Mcp-Name") != "gomode://items") {
                        mcpErrorResponse("Header mismatch: Mcp-Name header is required")
                    } else {
                        jsonResponse(RESOURCE_READ_JSON)
                    }
                }
                "subscriptions/listen" -> MockResponse()
                    .setHeader("Content-Type", "text/event-stream")
                    .setBody(SUBSCRIPTION_ACK_SSE)
                    .setSocketPolicy(SocketPolicy.KEEP_OPEN)
                else -> mcpErrorResponse("unexpected MCP method ${request.getHeader("Mcp-Method")}")
            }
        }
        server.start()
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
        val monitor = ServiceMonitor(scope) { endpointURL, protocolVersion ->
            McpClient(
                endpointURL = endpointURL,
                protocolVersion = protocolVersion,
                cookieProvider = { null },
            )
        }
        try {
            monitor.start(server.url("/").toString(), serviceSettings(endpoint = "/mcp"))
            val state = withTimeout(3000) {
                monitor.state.filter { it.snapshot != null || it.error != null }.first()
            }

            assertNull(state.error)
            assertNotNull(state.snapshot)
            assertEquals("Build feature", state.snapshot?.items?.single()?.title)
        } finally {
            monitor.stop()
            scope.cancel()
            server.shutdown()
        }
    }

    private class FakeServiceResourceClient : ServiceResourceClient {
        var resources = listOf(ResourceDescriptor(uri = "gomode://items", name = "service", mimeType = "application/json"))
        val readURIs = mutableListOf<String>()
        val subscriptionFilters = mutableListOf<SubscriptionFilter>()
        var closeSubscriptionsImmediately = false
        var listCalls = 0
        var listFailuresRemaining = 0
        var subscriptionCancelled = false

        private val readResults = ArrayDeque<ResourcesReadResult>()
        private val subscriptionEvents = MutableSharedFlow<JSONRPCNotification>(extraBufferCapacity = 8)

        fun enqueueReadResult(text: String) {
            readResults.addLast(itemsReadResult(text))
        }

        fun enqueueNotificationReadResult(text: String) {
            readResults.addLast(goModeNotificationsReadResult(text))
        }

        fun emit(notification: JSONRPCNotification) {
            check(subscriptionEvents.tryEmit(notification))
        }

        override suspend fun listResources(): List<ResourceDescriptor> {
            listCalls++
            if (listFailuresRemaining > 0) {
                listFailuresRemaining--
                throw IOException("HTTP 401")
            }
            return resources
        }

        override suspend fun readResource(uri: String): ResourcesReadResult {
            readURIs += uri
            return readResults.removeFirst()
        }

        override fun listenSubscriptions(notifications: SubscriptionFilter): Flow<JSONRPCNotification> {
            subscriptionFilters += notifications
            if (closeSubscriptionsImmediately) return flow {}
            return callbackFlow {
                val eventsJob = launch {
                    subscriptionEvents.collect { send(it) }
                }
                awaitClose {
                    subscriptionCancelled = true
                    eventsJob.cancel()
                }
            }
        }
    }

    private companion object {
        fun serviceSettings(endpoint: String = "/api/service/v1/mcp"): Settings = Settings(
            service = "example",
            apiVersion = 1,
            webShell = WebShellSettings(
                bridgeVersion = 1,
                toolGroups = listOf(
                    ToolGroup(
                        name = "service",
                        endpoint = endpoint,
                        protocolVersion = "2026-07-28",
                        authRequired = false,
                    ),
                ),
                voiceGateway = VoiceGatewaySettings(required = false),
            ),
        )

        fun itemsReadResult(text: String): ResourcesReadResult = ResourcesReadResult(
            resultType = ResultType.Complete,
            contents = listOf(
                ResourceContent(
                    uri = "gomode://items",
                    mimeType = "application/json",
                    text = text,
                ),
            ),
            ttlMs = 1000,
            cacheScope = CacheScope.Private,
        )

        fun goModeNotificationsReadResult(text: String): ResourcesReadResult = ResourcesReadResult(
            resultType = ResultType.Complete,
            contents = listOf(
                ResourceContent(
                    uri = GoModeNotificationsResourceURI,
                    mimeType = "application/json",
                    text = text,
                ),
            ),
            ttlMs = 1000,
            cacheScope = CacheScope.Private,
        )

        fun resourceUpdated(uri: String): JSONRPCNotification = JSONRPCNotification(
            jsonrpc = "2.0",
            method = NotificationMethod.ResourcesUpdated,
            params = buildJsonObject { put("uri", uri) },
        )

        fun jsonResponse(body: String): MockResponse = MockResponse()
            .setHeader("Content-Type", "application/json")
            .setBody(body)

        fun mcpErrorResponse(message: String): MockResponse = MockResponse()
            .setResponseCode(400)
            .setHeader("Content-Type", "application/json")
            .setBody(
                """
                    {"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"$message"}}
                """.trimIndent(),
            )

        const val RESOURCES_LIST_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 1,
              "result": {
                "resultType": "complete",
                "resources": [
                  {"uri": "gomode://items", "name": "items", "mimeType": "application/json"}
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val RESOURCE_READ_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 2,
              "result": {
                "resultType": "complete",
                "contents": [
                  {
                    "uri": "gomode://items",
                    "mimeType": "application/json",
                    "text": "[{\"id\":\"t1\",\"title\":\"Build feature\",\"state\":\"running\"}]"
                  }
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val SUBSCRIPTION_ACK_SSE =
            "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/subscriptions/acknowledged\"," +
                "\"params\":{\"notifications\":{\"resourceSubscriptions\":[\"gomode://items\"]}}}\n\n"
    }
}
