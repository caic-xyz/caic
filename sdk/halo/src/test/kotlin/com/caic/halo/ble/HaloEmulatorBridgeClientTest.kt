// Unit tests for HaloEmulatorBridgeClient WebSocket request/response and event handling.
package com.caic.halo.ble

import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.Base64
import java.util.concurrent.atomic.AtomicReference

class HaloEmulatorBridgeClientTest {
    private lateinit var server: MockWebServer
    private lateinit var client: HaloEmulatorBridgeClient
    private lateinit var serverSocket: AtomicReference<WebSocket>
    private lateinit var lastRequest: AtomicReference<JsonObject>

    @Before
    fun setUp() {
        server = MockWebServer()
        serverSocket = AtomicReference()
        lastRequest = AtomicReference()
        server.enqueue(
            MockResponse().withWebSocketUpgrade(
                object : WebSocketListener() {
                    override fun onOpen(webSocket: WebSocket, response: okhttp3.Response) {
                        serverSocket.set(webSocket)
                    }

                    override fun onMessage(webSocket: WebSocket, text: String) {
                        val request = Json.parseToJsonElement(text) as JsonObject
                        lastRequest.set(request)
                        val response = buildJsonObject {
                            put("id", request.value("id"))
                            put("ok", JsonPrimitive(true))
                            if (request.text("op") == "ping") {
                                put("running", JsonPrimitive(true))
                            }
                        }
                        webSocket.send(response.toString())
                    }

                    override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                        webSocket.close(code, reason)
                    }
                },
            ),
        )
        server.start()
        client = HaloEmulatorBridgeClient(OkHttpClient())
    }

    @After
    fun tearDown() {
        client.close()
        server.shutdown()
    }

    @Test
    fun `ping parses bridge status`() = runBlocking {
        client.connect(server.url("/bridge").toString().replace("http://", "ws://"))

        val status = client.ping()

        assertTrue(status.running)
        assertNull(status.error)
        assertEquals("ping", lastRequest.get().text("op"))
    }

    @Test
    fun `sendMessage encodes payload`() = runBlocking {
        client.connect(server.url("/bridge").toString().replace("http://", "ws://"))

        client.sendMessage(0x10, byteArrayOf(1, 2, 3))

        val request = lastRequest.get()
        assertEquals("send_message", request.text("op"))
        assertEquals(0x10, request.value("msgCode").jsonPrimitive.int)
        assertArrayEquals(byteArrayOf(1, 2, 3), Base64.getDecoder().decode(request.text("payload")))
    }

    @Test
    fun `bluetooth event emits decoded data`() = runBlocking {
        client.connect(server.url("/bridge").toString().replace("http://", "ws://"))
        serverSocket.get().send(
            buildJsonObject {
                put("event", JsonPrimitive("bluetooth_sent"))
                put("data", JsonPrimitive(Base64.getEncoder().encodeToString(byteArrayOf(4, 5, 6))))
            }.toString(),
        )

        val event = client.events.first() as HaloEmulatorEvent.BluetoothSent

        assertArrayEquals(byteArrayOf(4, 5, 6), event.data)
    }
}

private fun JsonObject.text(key: String): String = value(key).jsonPrimitive.content

private fun JsonObject.value(key: String) = getValue(key)
