// MCP client for Go Mode service tools and resources exposed to the native shell.
package com.fghbuild.gomode.voice

import android.util.Base64
import com.fghbuild.gomode.service.ServiceResourceClient
import com.fghbuild.mcp.sdk.v1.ApiClient
import com.fghbuild.mcp.sdk.v1.ClientCapabilities
import com.fghbuild.mcp.sdk.v1.Implementation
import com.fghbuild.mcp.sdk.v1.JSONRPCNotification
import com.fghbuild.mcp.sdk.v1.JSONRPCRequest
import com.fghbuild.mcp.sdk.v1.Method
import com.fghbuild.mcp.sdk.v1.PaginatedRequestParams
import com.fghbuild.mcp.sdk.v1.RequestMeta
import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourceTemplateDescriptor
import com.fghbuild.mcp.sdk.v1.ResourceTemplatesListResult
import com.fghbuild.mcp.sdk.v1.ResourcesListResult
import com.fghbuild.mcp.sdk.v1.ResourcesReadParams
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import com.fghbuild.mcp.sdk.v1.ServerDiscoverResult
import com.fghbuild.mcp.sdk.v1.SubscriptionFilter
import com.fghbuild.mcp.sdk.v1.SubscriptionsListenParams
import com.fghbuild.mcp.sdk.v1.ToolCallResult
import com.fghbuild.mcp.sdk.v1.ToolDescriptor
import com.fghbuild.mcp.sdk.v1.ToolsCallParams
import com.fghbuild.mcp.sdk.v1.ToolsListResult
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.launch
import kotlinx.serialization.encodeToString
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
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException
import java.util.concurrent.atomic.AtomicInteger

private val idCounter = AtomicInteger(0)
private val jsonMediaType = "application/json".toMediaType()

data class McpToolResult(
    val structuredContent: JsonObject,
    val isError: Boolean = false,
)

class McpClient(
    endpointURL: String,
    private val protocolVersion: String,
    private val cookieProvider: () -> String?,
) : ServiceResourceClient {
    private val endpointURL = endpointURL.trimEnd('/')
    private val api = ApiClient(this.endpointURL)
    private val httpClient = OkHttpClient()
    private val json = Json { ignoreUnknownKeys = true }
    private var toolsByName: Map<String, ToolDescriptor> = emptyMap()

    private val requestMeta: RequestMeta
        get() = RequestMeta(
            protocolVersion = protocolVersion,
            clientInfo = Implementation(name = "gomode-android", version = "1.0.0"),
            clientCapabilities = ClientCapabilities(),
        )

    private fun cookieHeaders(): Map<String, String> =
        cookieProvider()?.takeIf { it.isNotBlank() }?.let { mapOf("Cookie" to it) } ?: emptyMap()

    private suspend inline fun <reified T> request(
        method: Method,
        params: JsonElement,
        name: String? = null,
        paramHeaders: Map<String, String> = emptyMap(),
    ): T {
        val headers = mcpHeaders(method, name, paramHeaders)
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

    suspend fun serverDiscover(): ServerDiscoverResult = request(
        method = Method.ServerDiscover,
        params = buildJsonObject { put("_meta", json.encodeToJsonElement(requestMeta)) },
    )

    suspend fun serverInstructions(): String = serverDiscover().instructions.orEmpty()

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

    override suspend fun listResources(): List<ResourceDescriptor> {
        val resources = mutableListOf<ResourceDescriptor>()
        var cursor: String? = null
        do {
            val result = request<ResourcesListResult>(
                method = Method.ResourcesList,
                params = json.encodeToJsonElement(
                    PaginatedRequestParams(
                        _meta = requestMeta,
                        cursor = cursor,
                    )
                ),
            )
            resources += result.resources
            cursor = result.nextCursor
        } while (!cursor.isNullOrEmpty())
        return resources
    }

    suspend fun listResourceTemplates(): List<ResourceTemplateDescriptor> {
        val templates = mutableListOf<ResourceTemplateDescriptor>()
        var cursor: String? = null
        do {
            val result = request<ResourceTemplatesListResult>(
                method = Method.ResourceTemplatesList,
                params = json.encodeToJsonElement(
                    PaginatedRequestParams(
                        _meta = requestMeta,
                        cursor = cursor,
                    )
                ),
            )
            templates += result.resourceTemplates
            cursor = result.nextCursor
        } while (!cursor.isNullOrEmpty())
        return templates
    }

    override suspend fun readResource(uri: String): ResourcesReadResult = request(
        method = Method.ResourcesRead,
        params = json.encodeToJsonElement(
            ResourcesReadParams(
                _meta = requestMeta,
                uri = uri,
            )
        ),
        name = uri,
    )

    override fun listenSubscriptions(notifications: SubscriptionFilter): Flow<JSONRPCNotification> = callbackFlow {
        val body = json.encodeToString(
            JSONRPCRequest.serializer(),
            JSONRPCRequest(
                jsonrpc = "2.0",
                id = JsonPrimitive(idCounter.incrementAndGet()),
                method = Method.SubscriptionsListen,
                params = json.encodeToJsonElement(
                    SubscriptionsListenParams(
                        _meta = requestMeta,
                        notifications = notifications,
                    )
                ),
            ),
        )
        val request = Request.Builder()
            .url(endpointURL)
            .post(body.toRequestBody(jsonMediaType))
            .header("Accept", "text/event-stream")
            .apply { mcpHeaders(Method.SubscriptionsListen).forEach { (name, value) -> header(name, value) } }
            .build()
        val call = httpClient.newCall(request)
        val readerJob = launch(Dispatchers.IO) {
            try {
                call.execute().use { response ->
                    if (!response.isSuccessful) {
                        close(IOException("MCP subscription request failed: HTTP ${response.code}"))
                        return@launch
                    }
                    val responseBody = response.body ?: run {
                        close(IOException("MCP subscription response is missing a body"))
                        return@launch
                    }
                    responseBody.charStream().buffered().use { reader ->
                        val dataLines = mutableListOf<String>()
                        while (true) {
                            val line = reader.readLine() ?: break
                            val eventData = collectSSEData(line, dataLines) ?: continue
                            val notification = json.decodeFromString(JSONRPCNotification.serializer(), eventData)
                            if (trySend(notification).isFailure) return@launch
                        }
                        flushSSEData(dataLines)?.let { eventData ->
                            val notification = json.decodeFromString(JSONRPCNotification.serializer(), eventData)
                            if (trySend(notification).isFailure) return@launch
                        }
                    }
                }
                close()
            } catch (e: IOException) {
                if (call.isCanceled()) {
                    close()
                } else {
                    close(e)
                }
            }
        }
        awaitClose {
            call.cancel()
            readerJob.cancel()
        }
    }

    private fun mcpHeaders(
        method: Method,
        name: String? = null,
        paramHeaders: Map<String, String> = emptyMap(),
    ): Map<String, String> = buildMap {
        putAll(cookieHeaders())
        put("Mcp-Protocol-Version", protocolVersion)
        put("Mcp-Method", method.value)
        if (name != null) put("Mcp-Name", name)
        putAll(paramHeaders)
    }
}

private fun collectSSEData(line: String, dataLines: MutableList<String>): String? {
    if (line.isEmpty()) return flushSSEData(dataLines)
    if (line.startsWith("data:")) {
        dataLines += line.removePrefix("data:").trimStart()
    }
    return null
}

private fun flushSSEData(dataLines: MutableList<String>): String? {
    if (dataLines.isEmpty()) return null
    val data = dataLines.joinToString("\n")
    dataLines.clear()
    return data
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
