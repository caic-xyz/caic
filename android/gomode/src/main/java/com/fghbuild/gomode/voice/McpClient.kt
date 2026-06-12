// MCP client for Go Mode service tools exposed to the native voice session.
package com.fghbuild.gomode.voice

import android.util.Base64
import com.fghbuild.mcp.sdk.v1.ApiClient
import com.fghbuild.mcp.sdk.v1.ClientCapabilities
import com.fghbuild.mcp.sdk.v1.Implementation
import com.fghbuild.mcp.sdk.v1.JSONRPCRequest
import com.fghbuild.mcp.sdk.v1.Method
import com.fghbuild.mcp.sdk.v1.PaginatedRequestParams
import com.fghbuild.mcp.sdk.v1.RequestMeta
import com.fghbuild.mcp.sdk.v1.ToolCallResult
import com.fghbuild.mcp.sdk.v1.ToolDescriptor
import com.fghbuild.mcp.sdk.v1.ToolsCallParams
import com.fghbuild.mcp.sdk.v1.ToolsListResult
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.encodeToJsonElement
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import java.util.concurrent.atomic.AtomicInteger

private val idCounter = AtomicInteger(0)

data class McpToolResult(
    val structuredContent: JsonObject,
    val isError: Boolean = false,
)

class McpClient(
    endpointURL: String,
    private val protocolVersion: String,
    private val cookieProvider: () -> String?,
) {
    private val api = ApiClient(endpointURL)
    private val json = Json { ignoreUnknownKeys = true }
    private var toolsByName: Map<String, ToolDescriptor> = emptyMap()

    private val requestMeta: RequestMeta
        get() = RequestMeta(
            protocolVersion = protocolVersion,
            clientInfo = Implementation(name = "gomode-android", version = "1.0.0"),
            clientCapabilities = ClientCapabilities(),
        )

    private suspend inline fun <reified T> request(
        method: Method,
        params: JsonElement,
        name: String? = null,
        paramHeaders: Map<String, String> = emptyMap(),
    ): T {
        val headers = buildMap {
            cookieProvider()?.takeIf { it.isNotBlank() }?.let { put("Cookie", it) }
            put("Mcp-Protocol-Version", protocolVersion)
            put("Mcp-Method", method.value)
            if (name != null) put("Mcp-Name", name)
            putAll(paramHeaders)
        }
        val response = api.mcp(
            req = JSONRPCRequest(
                jsonrpc = "2.0",
                id = JsonPrimitive(idCounter.incrementAndGet()),
                method = method,
                params = params,
            ),
            headers = headers,
        )
        val rpcError = response.error
        if (rpcError != null) error(rpcError.message)
        val result = response.result ?: error("Missing result in MCP response")
        return json.decodeFromJsonElement(result)
    }

    suspend fun listTools(): List<ToolDescriptor> {
        val tools = mutableListOf<ToolDescriptor>()
        var cursor: String? = null
        do {
            val result = request<ToolsListResult>(
                method = Method.ToolsList,
                params = json.encodeToJsonElement(
                    PaginatedRequestParams(
                        _meta = requestMeta,
                        cursor = cursor,
                    )
                ),
            )
            tools += result.tools
            cursor = result.nextCursor
        } while (!cursor.isNullOrEmpty())
        toolsByName = tools.associateBy { it.name }
        return tools
    }

    suspend fun callTool(name: String, args: JsonObject): McpToolResult {
        val result = request<ToolCallResult>(
            method = Method.ToolsCall,
            params = json.encodeToJsonElement(
                ToolsCallParams(
                    _meta = requestMeta,
                    name = name,
                    arguments = args,
                )
            ),
            name = name,
            paramHeaders = mcpParamHeaders(toolsByName[name], args),
        )
        val structuredContent = result.structuredContent?.jsonObject ?: textContentAsStructuredError(result)
        return McpToolResult(
            structuredContent = structuredContent,
            isError = result.isError ?: false,
        )
    }
}

private fun textContentAsStructuredError(result: ToolCallResult): JsonObject {
    val text = result.content.firstNotNullOfOrNull { block ->
        block.takeIf { it.type.value == "text" }?.text
    } ?: return JsonObject(emptyMap())
    return buildJsonObject { put("error", text) }
}

private fun mcpParamHeaders(tool: ToolDescriptor?, args: JsonObject): Map<String, String> {
    val inputSchema = tool?.inputSchema?.jsonObject ?: return emptyMap()
    val headers = mutableMapOf<String, String>()
    collectMcpParamHeaders(inputSchema, args, headers)
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
