// HaloDevice: connected Halo/Frame over BLE — Lua REPL, raw data, control signals, audio streaming, typed messages, file upload.
package com.caic.halo.ble

import android.bluetooth.BluetoothDevice
import android.bluetooth.BluetoothGattCharacteristic
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.merge
import kotlinx.coroutines.withTimeoutOrNull
import java.util.UUID

class HaloDevice(
    val platformDevice: BluetoothDevice,
    val type: HaloDeviceType = HaloDeviceType.UNKNOWN,

    // GATT characteristics discovered by HaloConnection
    internal var txChar: BluetoothGattCharacteristic? = null,
    internal var rxChar: BluetoothGattCharacteristic? = null,
    internal var audioTxChar: BluetoothGattCharacteristic? = null,

    // Payload size limits derived from negotiated MTU.
    // Lua strings: MTU − 3; raw data: MTU − 4 (one byte for 0x01 header).
    internal var maxStringLen: Int = 0,
    internal var maxDataLen: Int = 0,

    // Shared notification Flow — populated by HaloConnection.enableServices.
    internal var rawNotifications: Flow<ByteArray>? = null,
) {
    val uuid: String get() = platformDevice.address

    // ---- Rx streams filtered from the shared notification Flow ----

    /** Lua print() output and errors — UTF-8 strings (data[0] != 0x01). */
    val stringResponse: Flow<String>
        get() = rawNotifications!!
            .filter { it.isNotEmpty() && it[0] != 0x01.toByte() }
            .map { String(it, Charsets.UTF_8) }

    /** Raw data sent by the device via frame.bluetooth.send() — first byte 0x01 stripped. */
    val dataResponse: Flow<ByteArray>
        get() = rawNotifications!!
            .filter { it.isNotEmpty() && it[0] == 0x01.toByte() }
            .map { it.copyOfRange(1, it.size) }

    // ---- Control signals (single-byte writes on LUA TX) ----

    /** Break any running Lua script (0x03). Pauses 200ms for the device to settle. */
    suspend fun sendBreakSignal(): Unit = sendControl(0x03)

    /** Reset Lua VM, run main.lua if present (0x04). Pauses 200ms for the device to settle. */
    suspend fun sendResetSignal(): Unit = sendControl(0x04)

    /** Remove main.lua file (Halo-only, 0x05). Pauses 200ms for the device to settle. */
    suspend fun sendRemoveSignal(): Unit = sendControl(0x05)

    private suspend fun sendControl(byte: Int) {
        val tx = txChar ?: throw HaloException("TX characteristic not available")
        sendRaw(RawWrite(tx, byteArrayOf(byte.toByte()), writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))
    }

    // ---- Lua REPL ----

    /**
     * Send a Lua string and optionally await the next string response.
     * Returns the response string, or null when awaitResponse is false.
     * Throws [HaloException] on timeout or disconnected state.
     */
    suspend fun sendString(
        string: String,
        awaitResponse: Boolean = true,
        timeoutMs: Long = 10000,
    ): String? {
        val tx = txChar ?: throw HaloException("TX characteristic not available")
        val bytes = string.toByteArray(Charsets.UTF_8)

        if (bytes.size > maxStringLen) {
            throw HaloException("Lua string exceeds max length ($maxStringLen): ${bytes.size}")
        }

        if (awaitResponse) {
            sendRaw(RawWrite(tx, bytes, writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))
            return withTimeoutOrNull(timeoutMs) {
                stringResponse.first()
            } ?: throw HaloException("Timeout waiting for Lua string response")
        } else {
            sendRaw(RawWrite(tx, bytes, writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))
            return null
        }
    }

    /** Check whether the Lua REPL is active by sending `print(1)` and expecting `"1"`. */
    suspend fun isLuaInReplState(timeoutMs: Long = 200): Boolean {
        @Suppress("SwallowedException")  // Intentional: probe timeout/error → not in REPL
        return try {
            sendString("print(1)", awaitResponse = true, timeoutMs = timeoutMs) == "1"
        } catch (e: HaloException) {
            false
        }
    }

    // ---- Raw data exchange ----

    /**
     * Send raw bytes to the device's receive_callback().
     * The 0x01 header byte is prepended automatically.
     * If [awaitResponse] is true, waits for the device to echo back (ACK).
     */
    suspend fun sendData(
        data: ByteArray,
        awaitResponse: Boolean = true,
        timeoutMs: Long = 5000,
    ) {
        val tx = txChar ?: throw HaloException("TX characteristic not available")
        val packet = ByteArray(1 + data.size)
        packet[0] = 0x01.toByte()
        data.copyInto(packet, 1)

        if (packet.size > maxDataLen + 1) {
            throw HaloException("Data payload exceeds max length ($maxDataLen)")
        }

        if (awaitResponse) {
            sendRaw(RawWrite(tx, packet, writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))
            withTimeoutOrNull(timeoutMs) {
                dataResponse.first()
            } ?: throw HaloException("Timeout waiting for data response")
        } else {
            sendRaw(RawWrite(tx, packet, writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))
        }
    }

    // ---- Audio streaming ----

    /** Stream audio (PCM or LC3) to the device speaker via the dedicated AUDIO TX characteristic. */
    suspend fun sendAudio(data: ByteArray) {
        val audioTx = audioTxChar
            ?: throw HaloException("AUDIO TX characteristic not available (only on Halo)")
        val maxAudioPayload = maxDataLen + 1 // AUDIO TX has no 0x01 header overhead
        if (data.size > maxAudioPayload) {
            throw HaloException("Audio payload ${data.size} exceeds MTU ($maxAudioPayload)")
        }
        sendRaw(RawWrite(audioTx, data, writeType = BluetoothGattCharacteristic.WRITE_TYPE_NO_RESPONSE))
    }

    // ---- Clear display (device-aware) ----

    suspend fun clearDisplay() {
        if (type == HaloDeviceType.HALO) {
            sendString("frame.display.clear()print(1)", awaitResponse = true)
        } else {
            sendString(
                "frame.display.bitmap(1,1,4,2,15,\"\\xFF\")frame.display.show()print(1)",
                awaitResponse = true,
            )
        }
    }

    // ---- Typed message protocol (sendMessage) ----
    // First packet:  [0x01] [msgCode] [length_high] [length_low] [payload...]
    // Subsequent:     [0x01] [msgCode] [payload...]
    // Max total payload: 65535 bytes.

    companion object {
        private const val MAX_MESSAGE_PAYLOAD = 65535
        private const val FIRST_HEADER_SIZE = 4 // 0x01 + msgCode + 2-byte length
        private const val SUBSEQ_HEADER_SIZE = 2 // 0x01 + msgCode
    }

    /**
     * Send a typed message split across MTU-sized chunks.
     * [msgCode] identifies the message type (0–255). [payload] is the serialized body.
     * The device-side data.lua library reassembles by msgCode.
     */
    suspend fun sendMessage(msgCode: Int, payload: ByteArray) {
        require(msgCode in 0..255) { "Message code must be 0–255, got $msgCode" }
        require(payload.size <= MAX_MESSAGE_PAYLOAD) {
            "Payload size ${payload.size} exceeds maximum $MAX_MESSAGE_PAYLOAD"
        }

        val tx = txChar ?: throw HaloException("TX characteristic not available")
        val firstChunkMax = maxDataLen - FIRST_HEADER_SIZE
        val chunkMax = maxDataLen - SUBSEQ_HEADER_SIZE

        var sent = 0
        var first = true

        while (sent < payload.size) {
            val remaining = payload.size - sent
            val chunkSize: Int
            val headerSize: Int

            if (first) {
                headerSize = FIRST_HEADER_SIZE
                chunkSize = minOf(firstChunkMax, remaining)
                first = false
            } else {
                headerSize = SUBSEQ_HEADER_SIZE
                chunkSize = minOf(chunkMax, remaining)
            }

            val packet = ByteArray(headerSize + chunkSize)
            packet[0] = 0x01.toByte()
            packet[1] = msgCode.toByte()

            if (headerSize == FIRST_HEADER_SIZE) {
                packet[2] = (payload.size shr 8).toByte()
                packet[3] = (payload.size and 0xFF).toByte()
            }

            payload.copyInto(packet, headerSize, sent, sent + chunkSize)

            // Each chunk waits for an application-level ACK from the device
            sendRaw(RawWrite(tx, packet, writeType = BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT))

            // Each chunk waits for an application-level ACK from the device
            withTimeoutOrNull(5000) {
                dataResponse.first()
            } ?: throw HaloException("Timeout waiting for message ACK (msgCode=0x${msgCode.toString(16)})")

            sent += chunkSize
        }
    }

    // ---- File upload ----

    /**
     * Upload [contents] as a Lua file at [path] (e.g. "main.lua") on the device filesystem.
     * Escapes Lua string literals and writes in MTU-sized chunks.
     */
    suspend fun uploadFile(path: String, contents: String) {
        val escaped = contents
            .replace("\\", "\\\\")
            .replace("\n", "\\n")
            .replace("\r\n", "\\n")
            .replace("\t", "\\t")
            .replace("'", "\\'")
            .replace("\"", "\\\"")

        // Open file
        val openResp = sendString("f=frame.file.open(\"$path\",\"w\");print(2)", awaitResponse = true)
        if (openResp != "2") throw HaloException("Error opening file on device: $openResp")

        var pos = 0
        val chunkDataSize = maxStringLen - 22 // overhead of f:write("");print(2)

        while (pos < escaped.length) {
            var chunkSize = minOf(chunkDataSize, escaped.length - pos)

            // Avoid splitting on a backslash (escape char in the escaped string)
            if (pos + chunkSize < escaped.length) {
                while (chunkSize > 0 && escaped[pos + chunkSize - 1] == '\\') {
                    chunkSize--
                }
            }
            if (chunkSize == 0) {
                throw HaloException("Chunk size reduced to zero at position $pos (malformed escape sequence?)")
            }

            val chunk = escaped.substring(pos, pos + chunkSize)
            val writeResp = sendString("f:write(\"$chunk\");print(2)", awaitResponse = true)
            if (writeResp != "2") throw HaloException("Error writing file on device: $writeResp")

            pos += chunkSize
        }

        val closeResp = sendString("f:close();print(2)", awaitResponse = true)
        if (closeResp != "2") throw HaloException("Error closing file on device: $closeResp")
    }

    // ---- Raw GATT write (wired by HaloConnection.wireWriteSink) ----

    data class RawWrite(
        val characteristic: BluetoothGattCharacteristic,
        val data: ByteArray,
        val writeType: Int,
    )

    @Suppress("MemberVisibilityCanBePrivate") // Public for test wiring via HaloConnection subclass override.
    var writeSink: suspend (RawWrite) -> Unit = { throw HaloException("HaloDevice not wired to a HaloConnection") }

    private suspend fun sendRaw(write: RawWrite) {
        writeSink(write)
    }
}
