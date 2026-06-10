// MCP JSON-RPC client for listing backend tool descriptors and dispatching tool calls via the MCP endpoint.
package com.fghbuild.caic.voice

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.util.concurrent.atomic.AtomicInteger

private const val MCP_PROTOCOL_VERSION = "2026-07-28"
private val idCounter = AtomicInteger(0)

data class McpToolDescriptor(
    val name: String,
    val title: String? = null,
    val description: String,
    val inputSchema: JsonObject,
)

data class McpToolResult(
    val structuredContent: JsonObject,
    val isError: Boolean = false,
)

class McpClient(
    private val baseURL: String,
    private val tokenProvider: () -> String?,
) {
    private val http = OkHttpClient()
    private val json = Json { ignoreUnknownKeys = true }

    private suspend fun request(method: String, params: JsonObject, name: String? = null): JsonObject {
        val id = idCounter.incrementAndGet()
        val meta = buildJsonObject {
            put("io.modelcontextprotocol/protocolVersion", MCP_PROTOCOL_VERSION)
            put("io.modelcontextprotocol/clientInfo", buildJsonObject {
                put("name", "caic-android")
                put("version", "1.0.0")
            })
            put("io.modelcontextprotocol/clientCapabilities", buildJsonObject { })
        }
        val paramsWithMeta = buildJsonObject {
            params.forEach { (k, v) -> put(k, v) }
            put("_meta", meta)
        }
        val body = buildJsonObject {
            put("jsonrpc", "2.0")
            put("id", id)
            put("method", method)
            put("params", paramsWithMeta)
        }

        val reqBuilder = Request.Builder()
            .url("$baseURL/api/caic/v1/mcp")
            .post(body.toString().toRequestBody("application/json".toMediaType()))
            .header("Mcp-Protocol-Version", MCP_PROTOCOL_VERSION)
            .header("Mcp-Method", method)
        if (name != null) reqBuilder.header("Mcp-Name", name)
        tokenProvider()?.takeIf { it.isNotBlank() }?.let { token ->
            reqBuilder.header("Authorization", "Bearer $token")
        }

        return withContext(Dispatchers.IO) {
            http.newCall(reqBuilder.build()).execute().use { resp ->
                val text = resp.body?.string() ?: error("Empty MCP response")
                val rpc = json.parseToJsonElement(text).jsonObject
                val err = rpc["error"]?.jsonObject
                if (err != null) {
                    val msg = err["message"]?.jsonPrimitive?.content ?: "MCP error"
                    error(msg)
                }
                rpc["result"]?.jsonObject ?: error("Missing result in MCP response")
            }
        }
    }

    suspend fun listTools(): List<McpToolDescriptor> {
        val result = request("tools/list", JsonObject(emptyMap()))
        val toolsArray = result["tools"]?.jsonArray ?: return emptyList()
        return toolsArray.mapNotNull { el ->
            val obj = el.jsonObject
            val name = obj["name"]?.jsonPrimitive?.content ?: return@mapNotNull null
            val desc = obj["description"]?.jsonPrimitive?.content ?: ""
            val inputSchema = obj["inputSchema"]?.jsonObject ?: JsonObject(emptyMap())
            McpToolDescriptor(
                name = name,
                title = obj["title"]?.jsonPrimitive?.content,
                description = desc,
                inputSchema = inputSchema,
            )
        }
    }

    suspend fun callTool(name: String, args: JsonObject): McpToolResult {
        val params = buildJsonObject {
            put("name", name)
            put("arguments", args)
        }
        val result = request("tools/call", params, name)
        val structuredContent = result["structuredContent"]?.jsonObject ?: JsonObject(emptyMap())
        val isError = result["isError"]?.jsonPrimitive?.booleanOrNull ?: false
        return McpToolResult(structuredContent = structuredContent, isError = isError)
    }
}
