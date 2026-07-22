// Unit tests for Go Mode MCP client request envelopes.
package com.fghbuild.gomode.voice

import com.fghbuild.mcp.sdk.v1.NotificationMethod
import com.fghbuild.mcp.sdk.v1.SubscriptionFilter
import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Test

class McpClientTest {
    @Test
    fun `server instructions request includes json rpc id`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setBody(SERVER_DISCOVER_JSON).setResponseCode(200))
            val client = McpClient(
                endpointURL = server.url("/mcp").toString(),
                protocolVersion = "2026-07-28",
                cookieProvider = { null },
            )

            assertEquals("Use the tools.", client.serverInstructions())

            val request = server.takeRequest()
            val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
            assertEquals("/mcp", request.path)
            assertEquals("2026-07-28", request.getHeader("Mcp-Protocol-Version"))
            assertEquals("server/discover", request.getHeader("Mcp-Method"))
            assertEquals("2.0", body["jsonrpc"]?.jsonPrimitive?.content)
            assertNotNull(body["id"])
            assertEquals("server/discover", body["method"]?.jsonPrimitive?.content)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `resources list request paginates`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setBody(RESOURCES_LIST_PAGE_1_JSON).setResponseCode(200))
            server.enqueue(MockResponse().setBody(RESOURCES_LIST_PAGE_2_JSON).setResponseCode(200))
            val client = McpClient(
                endpointURL = server.url("/mcp").toString(),
                protocolVersion = "2026-07-28",
                cookieProvider = { "session=abc" },
            )

            val resources = client.listResources()

            assertEquals(listOf("service://items", "service://usage"), resources.map { it.uri })
            val firstRequest = server.takeRequest()
            val firstBody = Json.parseToJsonElement(firstRequest.body.readUtf8()).jsonObject
            assertEquals("/mcp", firstRequest.path)
            assertEquals("session=abc", firstRequest.getHeader("Cookie"))
            assertEquals("resources/list", firstRequest.getHeader("Mcp-Method"))
            assertEquals("resources/list", firstBody["method"]?.jsonPrimitive?.content)

            val secondRequest = server.takeRequest()
            val secondBody = Json.parseToJsonElement(secondRequest.body.readUtf8()).jsonObject
            assertEquals("resources/list", secondRequest.getHeader("Mcp-Method"))
            assertEquals("next", secondBody["params"]?.jsonObject?.get("cursor")?.jsonPrimitive?.content)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `resource templates list request uses resource template method`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setBody(RESOURCE_TEMPLATES_LIST_JSON).setResponseCode(200))
            val client = McpClient(
                endpointURL = server.url("/mcp").toString(),
                protocolVersion = "2026-07-28",
                cookieProvider = { null },
            )

            val templates = client.listResourceTemplates()

            assertEquals(listOf("item"), templates.map { it.name })
            val request = server.takeRequest()
            val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
            assertEquals("resources/templates/list", request.getHeader("Mcp-Method"))
            assertEquals("resources/templates/list", body["method"]?.jsonPrimitive?.content)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `resource read request sends resource uri`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(MockResponse().setBody(RESOURCE_READ_JSON).setResponseCode(200))
            val client = McpClient(
                endpointURL = server.url("/mcp").toString(),
                protocolVersion = "2026-07-28",
                cookieProvider = { null },
            )

            val result = client.readResource("service://items")

            assertEquals("[]", result.contents.single().text)
            val request = server.takeRequest()
            val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
            assertEquals("resources/read", request.getHeader("Mcp-Method"))
            assertEquals("service://items", request.getHeader("Mcp-Name"))
            assertEquals("resources/read", body["method"]?.jsonPrimitive?.content)
            assertEquals("service://items", body["params"]?.jsonObject?.get("uri")?.jsonPrimitive?.content)
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun `subscriptions listen posts sse request and parses notifications`() = runBlocking {
        val server = MockWebServer()
        server.start()
        try {
            server.enqueue(
                MockResponse()
                    .setHeader("Content-Type", "text/event-stream")
                    .setBody(SUBSCRIPTION_SSE)
                    .setResponseCode(200),
            )
            val client = McpClient(
                endpointURL = server.url("/mcp").toString(),
                protocolVersion = "2026-07-28",
                cookieProvider = { null },
            )

            val events = client.listenSubscriptions(
                SubscriptionFilter(resourceSubscriptions = listOf("service://items")),
            ).take(2).toList()

            assertEquals(NotificationMethod.SubscriptionsAcknowledged, events[0].method)
            assertEquals(NotificationMethod.ResourcesUpdated, events[1].method)
            val request = server.takeRequest()
            val body = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
            val params = body["params"]?.jsonObject
            val resourceSubscriptions = params
                ?.get("notifications")
                ?.jsonObject
                ?.get("resourceSubscriptions")
                ?.jsonArray
                ?.map { it.jsonPrimitive.content }
            assertEquals("POST", request.method)
            assertEquals("text/event-stream", request.getHeader("Accept"))
            assertEquals("subscriptions/listen", request.getHeader("Mcp-Method"))
            assertEquals("subscriptions/listen", body["method"]?.jsonPrimitive?.content)
            assertEquals(listOf("service://items"), resourceSubscriptions)
        } finally {
            server.shutdown()
        }
    }

    private companion object {
        const val SERVER_DISCOVER_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 1,
              "result": {
                "resultType": "complete",
                "supportedVersions": ["2026-07-28"],
                "capabilities": {"tools": {"listChanged": true}},
                "serverInfo": {"name": "test", "version": "1.0.0"},
                "instructions": "Use the tools.",
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val RESOURCES_LIST_PAGE_1_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 2,
              "result": {
                "resultType": "complete",
                "nextCursor": "next",
                "resources": [
                  {"uri": "service://items", "name": "items", "mimeType": "application/json"}
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val RESOURCES_LIST_PAGE_2_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 3,
              "result": {
                "resultType": "complete",
                "resources": [
                  {"uri": "service://usage", "name": "usage", "mimeType": "application/json"}
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val RESOURCE_TEMPLATES_LIST_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 4,
              "result": {
                "resultType": "complete",
                "resourceTemplates": [
                  {"name": "item", "uriTemplate": "service://items/{id}", "mimeType": "application/json"}
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val RESOURCE_READ_JSON = """
            {
              "jsonrpc": "2.0",
              "id": 5,
              "result": {
                "resultType": "complete",
                "contents": [
                  {"uri": "service://items", "mimeType": "application/json", "text": "[]"}
                ],
                "ttlMs": 1000,
                "cacheScope": "private"
              }
            }
        """

        const val SUBSCRIPTION_SSE =
            "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/subscriptions/acknowledged\"," +
                "\"params\":{\"notifications\":{\"resourceSubscriptions\":[\"service://items\"]}}}\n\n" +
                "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/resources/updated\"," +
                "\"params\":{\"uri\":\"service://items\"}}\n\n"
    }
}
