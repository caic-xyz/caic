// Unit tests for HaloDevice: Lua REPL, raw data, control signals, typed messages, file upload.
// Uses Robolectric to shadow android.bluetooth.BluetoothDevice.
//
// Test strategy: notificationFlow is backed by a Channel.  writeSink emits
// responses into the channel AFTER capturing the write.  HaloDevice's
// send-then-receive pattern means `stringResponse.first()` / `dataResponse.first()`
// picks up the response from the channel.  Channel.receiveAsFlow() supports
// multiple independent collectors (unlike consumeAsFlow).
@file:Suppress("MaxLineLength")

package com.caic.halo.ble

import android.bluetooth.BluetoothAdapter
import android.bluetooth.BluetoothDevice
import android.bluetooth.BluetoothGattCharacteristic
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.flow.take
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import java.util.UUID
import java.util.concurrent.atomic.AtomicReference

@OptIn(ExperimentalCoroutinesApi::class)
@RunWith(RobolectricTestRunner::class)
class HaloDeviceTest {

    companion object {
        private const val TX_UUID_STR = "7A230002-5475-A6A4-654C-8431F6AD49C4"
    }

    private lateinit var device: HaloDevice
    private lateinit var btDevice: BluetoothDevice
    private lateinit var txChar: BluetoothGattCharacteristic
    private lateinit var notificationChannel: Channel<ByteArray>
    private lateinit var lastWrite: AtomicReference<HaloDevice.RawWrite>

    @Before
    fun setUp() {
        val adapter = BluetoothAdapter.getDefaultAdapter()
        btDevice = adapter.getRemoteDevice("00:11:22:33:44:55")
        shadowOf(btDevice).setName("Halo AB")

        txChar = BluetoothGattCharacteristic(
            UUID.fromString(TX_UUID_STR),
            BluetoothGattCharacteristic.PROPERTY_WRITE or BluetoothGattCharacteristic.PROPERTY_WRITE_NO_RESPONSE,
            BluetoothGattCharacteristic.PERMISSION_WRITE,
        )

        notificationChannel = Channel(Channel.UNLIMITED)
        lastWrite = AtomicReference()

        device = HaloDevice(
            platformDevice = btDevice,
            type = HaloDeviceType.HALO,
            txChar = txChar,
            rxChar = null,
            audioTxChar = null,
            maxStringLen = 500,
            maxDataLen = 500,
            rawNotifications = notificationChannel.receiveAsFlow(),
        )
        device.writeSink = { lastWrite.set(it) }
    }

    // ---- helpers ----

    private fun stringPacket(text: String) = text.toByteArray(Charsets.UTF_8)
    private fun dataPacket(vararg bytes: Byte) = byteArrayOf(0x01.toByte()) + bytes
    private val dataAck = dataPacket(0x00)

    // ---- writeSink variants that also send a response into the channel ----

    private fun sinkWithString(text: String): suspend (HaloDevice.RawWrite) -> Unit = { w ->
        lastWrite.set(w)
        notificationChannel.send(stringPacket(text))
    }

    private fun sinkWithData(bytes: ByteArray): suspend (HaloDevice.RawWrite) -> Unit = { w ->
        lastWrite.set(w)
        notificationChannel.send(byteArrayOf(0x01.toByte()) + bytes)
    }

    private fun sinkWithDataAck(): suspend (HaloDevice.RawWrite) -> Unit = sinkWithData(byteArrayOf(0x00))

    // =========================================================================
    // Control signals
    // =========================================================================

    @Test
    fun `sendBreakSignal writes 0x03`() = runTest {
        device.sendBreakSignal()
        assertEquals(0x03.toByte(), lastWrite.get().data[0])
    }

    @Test
    fun `sendResetSignal writes 0x04`() = runTest {
        device.sendResetSignal()
        assertEquals(0x04.toByte(), lastWrite.get().data[0])
    }

    @Test
    fun `sendRemoveSignal writes 0x05`() = runTest {
        device.sendRemoveSignal()
        assertEquals(0x05.toByte(), lastWrite.get().data[0])
    }

    // =========================================================================
    // Lua REPL
    // =========================================================================

    @Test
    fun `sendString without awaiting response`() = runTest {
        assertEquals(null, device.sendString("print(1)", awaitResponse = false))
        assertEquals("print(1)", String(lastWrite.get().data))
    }

    @Test
    fun `sendString with awaitResponse`() = runTest {
        device.writeSink = sinkWithString("hello")
        assertEquals("hello", device.sendString("print(1)", awaitResponse = true))
        assertEquals("print(1)", String(lastWrite.get().data))
    }

    @Test
    fun `sendString rejects oversized payload`() = runTest {
        device = HaloDevice(platformDevice = btDevice, txChar = txChar, maxStringLen = 5)
        device.writeSink = { lastWrite.set(it) }
        try {
            device.sendString("too long string")
            fail("Expected HaloException")
        } catch (e: HaloException) {
            assertTrue(e.message!!.contains("exceeds max length"))
        }
    }

    @Test
    fun `isLuaInReplState true`() = runTest {
        device.writeSink = sinkWithString("1")
        assertTrue(device.isLuaInReplState(timeoutMs = 1000))
    }

    @Test
    fun `isLuaInReplState false on timeout`() = runTest {
        assertFalse(device.isLuaInReplState(timeoutMs = 1))
    }

    // =========================================================================
    // Raw data
    // =========================================================================

    @Test
    fun `sendData prepends 0x01 header`() = runTest {
        device.writeSink = sinkWithDataAck()
        device.sendData(byteArrayOf(0x10, 0x20, 0x30))
        val sent = lastWrite.get().data
        assertEquals(0x01.toByte(), sent[0])
        assertArrayEquals(byteArrayOf(0x10, 0x20, 0x30), sent.copyOfRange(1, sent.size))
    }

    @Test
    fun `sendData rejects oversized payload`() = runTest {
        device = HaloDevice(platformDevice = btDevice, txChar = txChar, maxDataLen = 3)
        device.writeSink = { lastWrite.set(it) }
        try {
            device.sendData(ByteArray(5))
            fail("Expected HaloException")
        } catch (e: HaloException) {
            assertTrue(e.message!!.contains("exceeds max length"))
        }
    }

    // =========================================================================
    // Typed messages
    // =========================================================================

    @Test
    fun `sendMessage splits payload across chunks`() = runTest {
        val writes = mutableListOf<HaloDevice.RawWrite>()
        device = HaloDevice(platformDevice = btDevice, txChar = txChar, maxDataLen = 20, rawNotifications = notificationChannel.receiveAsFlow())
        device.writeSink = { w -> writes.add(w); notificationChannel.send(dataAck) }

        val payload = ByteArray(30) { (it + 1).toByte() }
        device.sendMessage(msgCode = 0x42, payload = payload)

        assertEquals(2, writes.size)
        val f = writes[0].data
        assertEquals(0x01.toByte(), f[0]); assertEquals(0x42.toByte(), f[1])
        assertEquals(0x00.toByte(), f[2]); assertEquals(0x1E.toByte(), f[3])
        assertArrayEquals(payload.copyOf(16), f.copyOfRange(4, f.size))

        val s = writes[1].data
        assertEquals(0x01.toByte(), s[0]); assertEquals(0x42.toByte(), s[1])
        assertArrayEquals(payload.copyOfRange(16, 30), s.copyOfRange(2, s.size))
    }

    @Test
    fun `sendMessage single chunk`() = runTest {
        device.writeSink = sinkWithDataAck()
        val p = byteArrayOf(1, 2, 3, 4, 5)
        device.sendMessage(msgCode = 0x10, payload = p)
        val s = lastWrite.get().data
        assertArrayEquals(p, s.copyOfRange(4, s.size))
    }

    @Test
    fun `sendMessage rejects bad msgCode`() = runTest {
        try { device.sendMessage(256, byteArrayOf()); fail() }
        catch (e: IllegalArgumentException) { assertTrue(e.message!!.contains("0–255")) }
    }

    @Test
    fun `sendMessage rejects oversized payload`() = runTest {
        try { device.sendMessage(1, ByteArray(65536)); fail() }
        catch (e: IllegalArgumentException) { assertTrue(e.message!!.contains("65535")) }
    }

    // =========================================================================
    // clearDisplay
    // =========================================================================

    @Test
    fun `clearDisplay HALO`() = runTest {
        device.writeSink = sinkWithString("1")
        device.clearDisplay()
        assertEquals("frame.display.clear()print(1)", String(lastWrite.get().data))
    }

    @Test
    fun `clearDisplay FRAME`() = runTest {
        device = HaloDevice(platformDevice = btDevice, type = HaloDeviceType.FRAME, txChar = txChar, maxStringLen = 500, rawNotifications = notificationChannel.receiveAsFlow())
        device.writeSink = sinkWithString("1")
        device.clearDisplay()
        assertTrue(String(lastWrite.get().data).contains("frame.display.bitmap"))
    }

    // =========================================================================
    // File upload
    // =========================================================================

    @Test
    fun `uploadFile open write close`() = runTest {
        val w = mutableListOf<String>()
        device.writeSink = { r -> w.add(String(r.data)); notificationChannel.send(stringPacket("2")) }
        device.uploadFile("test.lua", "Hello\nWorld")
        assertTrue("Need >= 3 writes, got ${w.size}", w.size >= 3)
        assertEquals("f=frame.file.open(\"test.lua\",\"w\");print(2)", w[0])
        assertTrue(w[1].startsWith("f:write(\""))
        assertEquals("f:close();print(2)", w[w.size - 1])
    }

    @Test
    fun `uploadFile escapes special chars`() = runTest {
        val w = mutableListOf<String>()
        device.writeSink = { r -> w.add(String(r.data)); notificationChannel.send(stringPacket("2")) }
        device.uploadFile("t.lua", "a\\b\nc'd\"e")
        assertEquals("f=frame.file.open(\"t.lua\",\"w\");print(2)", w[0])
        val cmd = w[1]
        assertTrue(cmd.contains("\\\\"))
        assertTrue(cmd.contains("\\n"))
        assertTrue(cmd.contains("\\'"))
        assertTrue(cmd.contains("\\\""))
    }

    // =========================================================================
    // Flow streams
    // =========================================================================

    @Test
    fun `stringResponse filters data`() = runTest {
        // Collect a fixed number of items then stop, to avoid timing issues.
        val r = mutableListOf<String>()
        val job = backgroundScope.launch {
            device.stringResponse.take(2).collect { r.add(it) }
        }
        notificationChannel.send(stringPacket("hello"))
        notificationChannel.send(dataPacket(0x42)) // filtered out
        notificationChannel.send(stringPacket("world"))
        job.join() // waits until take(2) completes (2 strings collected)
        assertEquals(listOf("hello", "world"), r)
    }

    @Test
    fun `dataResponse strips prefix`() = runTest {
        val r = mutableListOf<ByteArray>()
        val job = backgroundScope.launch {
            device.dataResponse.take(1).collect { r.add(it) }
        }
        notificationChannel.send(stringPacket("ignored")) // filtered out
        notificationChannel.send(dataPacket(0x42, 0x55))
        job.join()
        assertEquals(1, r.size)
        assertArrayEquals(byteArrayOf(0x42, 0x55), r[0])
    }

    // =========================================================================
    // Misc
    // =========================================================================

    @Test
    fun `uuid returns address`() = assertEquals("00:11:22:33:44:55", device.uuid)

    @Test
    fun `type is HALO`() = assertEquals(HaloDeviceType.HALO, device.type)
}
