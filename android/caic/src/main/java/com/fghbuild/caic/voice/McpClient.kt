// MCP JSON-RPC client for listing backend tool descriptors and dispatching tool calls via the MCP endpoint.
package com.fghbuild.caic.voice

import android.util.Base64
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
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
    val outputSchema: JsonObject? = null,
    val annotations: JsonObject? = null,
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
    private var toolsByName: Map<String, McpToolDescriptor> = emptyMap()

    private suspend fun request(
        method: String,
        params: JsonObject,
        name: String? = null,
        paramHeaders: Map<String, String> = emptyMap(),
    ): JsonObject {
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
            .header("Accept", "application/json, text/event-stream")
            .header("Mcp-Protocol-Version", MCP_PROTOCOL_VERSION)
            .header("Mcp-Method", method)
        if (name != null) reqBuilder.header("Mcp-Name", name)
        paramHeaders.forEach { (header, value) -> reqBuilder.header(header, value) }
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
        val tools = mutableListOf<McpToolDescriptor>()
        var cursor: String? = null
        do {
            val params = cursor?.let { buildJsonObject { put("cursor", it) } } ?: JsonObject(emptyMap())
            val result = request("tools/list", params)
            val toolsArray = result["tools"]?.jsonArray ?: return emptyList()
            tools += toolsArray.mapNotNull { el ->
                val obj = el.jsonObject
                val name = obj["name"]?.jsonPrimitive?.content ?: return@mapNotNull null
                val desc = obj["description"]?.jsonPrimitive?.content ?: ""
                val inputSchema = obj["inputSchema"]?.jsonObject ?: JsonObject(emptyMap())
                McpToolDescriptor(
                    name = name,
                    title = obj["title"]?.jsonPrimitive?.content,
                    description = desc,
                    inputSchema = inputSchema,
                    outputSchema = obj["outputSchema"]?.jsonObject,
                    annotations = obj["annotations"]?.jsonObject,
                )
            }
            cursor = result["nextCursor"]?.jsonPrimitive?.content
        } while (!cursor.isNullOrEmpty())
        toolsByName = tools.associateBy { it.name }
        return tools
    }

    suspend fun callTool(name: String, args: JsonObject): McpToolResult {
        val params = buildJsonObject {
            put("name", name)
            put("arguments", args)
        }
        val result = request("tools/call", params, name, mcpParamHeaders(toolsByName[name], args))
        val structuredContent = result["structuredContent"]?.jsonObject ?: textContentAsStructuredError(result)
        val isError = result["isError"]?.jsonPrimitive?.booleanOrNull ?: false
        return McpToolResult(structuredContent = structuredContent, isError = isError)
    }
}

private fun textContentAsStructuredError(result: JsonObject): JsonObject {
    val content = result["content"]?.jsonArray ?: return JsonObject(emptyMap())
    val text = content.firstNotNullOfOrNull { block ->
        val obj = block.jsonObject
        obj.takeIf { it["type"]?.jsonPrimitive?.content == "text" }
            ?.get("text")
            ?.jsonPrimitive
            ?.content
    } ?: return JsonObject(emptyMap())
    return buildJsonObject { put("error", text) }
}

private fun mcpParamHeaders(tool: McpToolDescriptor?, args: JsonObject): Map<String, String> {
    if (tool == null) return emptyMap()
    val headers = mutableMapOf<String, String>()
    collectMcpParamHeaders(tool.inputSchema, args, headers)
    return headers
}

private fun collectMcpParamHeaders(schema: JsonObject, args: JsonElement?, headers: MutableMap<String, String>) {
    val headerName = schema["x-mcp-header"]?.jsonPrimitive?.content
    if (headerName != null && args != null && args !is JsonNull) {
        headers["Mcp-Param-$headerName"] = encodeMcpHeaderValue(args.jsonPrimitive.content)
    }
    val properties = schema["properties"]?.jsonObject ?: return
    val argObject = args as? JsonObject ?: return
    properties.forEach { (key, child) ->
        val childSchema = child as? JsonObject ?: return@forEach
        collectMcpParamHeaders(childSchema, argObject[key], headers)
    }
}

private fun encodeMcpHeaderValue(value: String): String {
    if (isPlainMcpHeaderValue(value)) return value
    val encoded = Base64.encodeToString(value.toByteArray(Charsets.UTF_8), Base64.NO_WRAP)
    return "=?base64?$encoded?="
}

private fun isPlainMcpHeaderValue(value: String): Boolean {
    if (value.startsWith("=?base64?") && value.endsWith("?=")) return false
    if (value.trim() != value) return false
    return value.all { it == ' ' || it in '!'..'~' }
}
