// Unit tests for Go Mode MCP client request envelopes.
package com.fghbuild.gomode.voice

import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
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
    }
}
