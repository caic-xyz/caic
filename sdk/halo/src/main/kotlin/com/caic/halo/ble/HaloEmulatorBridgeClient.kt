// HaloEmulatorBridgeClient: WebSocket client for the Python Halo emulator bridge.
package com.caic.halo.ble

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import java.io.Closeable
import java.util.Base64
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

sealed interface HaloEmulatorEvent {
    data class Print(val text: String) : HaloEmulatorEvent

    data class BluetoothSent(val data: ByteArray) : HaloEmulatorEvent {
        override fun equals(other: Any?): Boolean = other is BluetoothSent && data.contentEquals(other.data)
        override fun hashCode(): Int = data.contentHashCode()
    }
}

data class HaloEmulatorStatus(
    val running: Boolean,
    val error: String?,
)

class HaloEmulatorBridgeClient(
    private val okHttpClient: OkHttpClient = OkHttpClient(),
) : Closeable {
    private val nextRequestId = AtomicLong(1)
    private val pending = ConcurrentHashMap<Long, CompletableDeferred<JsonObject>>()
    private val eventChannel = Channel<HaloEmulatorEvent>(Channel.UNLIMITED)

    private var webSocket: WebSocket? = null

    val events: Flow<HaloEmulatorEvent> = eventChannel.receiveAsFlow()

    suspend fun connect(url: String, timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        close()
        val opened = CompletableDeferred<Unit>()
        val request = Request.Builder().url(url).build()
        webSocket = okHttpClient.newWebSocket(
            request,
            object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    opened.complete(Unit)
                }

                override fun onMessage(webSocket: WebSocket, text: String) {
                    handleMessage(text)
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    opened.completeExceptionally(HaloException("Emulator bridge connection failed: ${t.message}", t))
                    failPending(t)
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    failPending(HaloException("Emulator bridge closed: $code $reason"))
                }
            },
        )
        withTimeout(timeoutMs) { opened.await() }
    }

    suspend fun ping(timeoutMs: Long = DEFAULT_TIMEOUT_MS): HaloEmulatorStatus {
        val response = request("ping", timeoutMs = timeoutMs)
        return HaloEmulatorStatus(
            running = response.boolean("running") ?: false,
            error = response.string("error"),
        )
    }

    suspend fun connectRepl(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("connect_repl", timeoutMs = timeoutMs)
    }

    suspend fun executeLua(code: String, timeoutMs: Long = DEFAULT_TIMEOUT_MS): String? {
        val response = request("execute_lua", mapOf("code" to JsonPrimitive(code)), timeoutMs)
        return response.string("result")
    }

    suspend fun start(script: String = "main.lua", timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("start", mapOf("script" to JsonPrimitive(script)), timeoutMs)
    }

    suspend fun stop(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("stop", timeoutMs = timeoutMs)
    }

    suspend fun sendBreak(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("break", timeoutMs = timeoutMs)
    }

    suspend fun reset(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("reset", timeoutMs = timeoutMs)
    }

    suspend fun removeAllFiles(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("remove_all_files", timeoutMs = timeoutMs)
    }

    suspend fun uploadFile(path: String, content: String, timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request(
            "upload_file",
            mapOf("path" to JsonPrimitive(path), "content" to JsonPrimitive(content)),
            timeoutMs,
        )
    }

    suspend fun clearDisplay(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("clear_display", timeoutMs = timeoutMs)
    }

    suspend fun sendMessage(msgCode: Int, payload: ByteArray, timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        require(msgCode in 0..255) { "Message code must be 0..255, got $msgCode" }
        request(
            "send_message",
            mapOf(
                "msgCode" to JsonPrimitive(msgCode),
                "payload" to JsonPrimitive(Base64.getEncoder().encodeToString(payload)),
            ),
            timeoutMs,
        )
    }

    suspend fun buttonSingle(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("button_single", timeoutMs = timeoutMs)
    }

    suspend fun buttonDouble(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("button_double", timeoutMs = timeoutMs)
    }

    suspend fun buttonLong(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("button_long", timeoutMs = timeoutMs)
    }

    suspend fun imuTap(timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        request("imu_tap", timeoutMs = timeoutMs)
    }

    suspend fun getFramebufferPng(timeoutMs: Long = DEFAULT_TIMEOUT_MS): ByteArray {
        val response = request("get_framebuffer", timeoutMs = timeoutMs)
        return Base64.getDecoder().decode(response.requiredString("imagePng"))
    }

    override fun close() {
        webSocket?.close(NORMAL_CLOSE, "closing")
        webSocket = null
        failPending(HaloException("Emulator bridge client closed"))
    }

    private suspend fun request(
        op: String,
        fields: Map<String, JsonElement> = emptyMap(),
        timeoutMs: Long = DEFAULT_TIMEOUT_MS,
    ): JsonObject {
        val socket = webSocket ?: throw HaloException("Emulator bridge is not connected")
        val requestId = nextRequestId.getAndIncrement()
        val response = CompletableDeferred<JsonObject>()
        pending[requestId] = response
        val payload = buildJsonObject {
            put("id", JsonPrimitive(requestId))
            put("op", JsonPrimitive(op))
            fields.forEach { (key, value) -> put(key, value) }
        }
        if (!socket.send(payload.toString())) {
            pending.remove(requestId)
            throw HaloException("Failed to send emulator bridge request")
        }
        return withTimeout(timeoutMs) { response.await() }
    }

    private fun handleMessage(text: String) {
        val message = Json.parseToJsonElement(text) as? JsonObject ?: return
        if (message.containsKey("event")) {
            handleEvent(message)
            return
        }
        val requestId = message["id"]?.jsonPrimitive?.content?.toLongOrNull() ?: return
        val response = pending.remove(requestId) ?: return
        if (message.boolean("ok") == true) {
            response.complete(message)
        } else {
            response.completeExceptionally(HaloException(message.string("error") ?: "Emulator bridge error"))
        }
    }

    private fun handleEvent(message: JsonObject) {
        when (message.string("event")) {
            "bluetooth_sent" -> eventChannel.trySend(
                HaloEmulatorEvent.BluetoothSent(Base64.getDecoder().decode(message.requiredString("data"))),
            )
            "print" -> eventChannel.trySend(HaloEmulatorEvent.Print(message.string("text") ?: ""))
        }
    }

    private fun failPending(error: Throwable) {
        pending.values.forEach { it.completeExceptionally(error) }
        pending.clear()
    }

    private fun JsonObject.string(key: String): String? {
        val element = this[key]
        if (element == null || element is JsonNull) return null
        return element.jsonPrimitive.content.takeIf { it.isNotBlank() }
    }

    private fun JsonObject.requiredString(key: String): String =
        string(key) ?: throw HaloException("Emulator bridge response missing '$key'")

    private fun JsonObject.boolean(key: String): Boolean? = this[key]?.jsonPrimitive?.booleanOrNull

    private companion object {
        const val DEFAULT_TIMEOUT_MS = 5_000L
        const val NORMAL_CLOSE = 1000
    }
}
