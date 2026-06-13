// Unit tests for Rx message parsers.
package com.caic.halo.msg

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.launch
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import java.nio.ByteBuffer
import java.nio.ByteOrder

@OptIn(ExperimentalCoroutinesApi::class)
@RunWith(RobolectricTestRunner::class)
class RxMessageTest {

    // ---- RxClick ----

    @Test
    fun `click parses single click`() = runTest {
        val parser = RxClick()
        val flow = parser.attach(flowOf(byteArrayOf(0x0B, 0x01)))
        assertEquals(ClickType.SINGLE, flow.toList().single())
    }

    @Test
    fun `click parses double and long`() = runTest {
        val parser = RxClick()
        val flow = parser.attach(flowOf(
            byteArrayOf(0x0B, 0x02),
            byteArrayOf(0x0B, 0x03),
        ))
        assertEquals(listOf(ClickType.DOUBLE, ClickType.LONG), flow.toList())
    }

    @Test
    fun `click filters out other msgCodes`() = runTest {
        val parser = RxClick()
        val flow = parser.attach(flowOf(
            byteArrayOf(0x0A, 0x01), // IMU, not click
            byteArrayOf(0x0B, 0x01), // click
        ))
        assertEquals(listOf(ClickType.SINGLE), flow.toList())
    }

    @Test
    fun `click filters empty packets`() = runTest {
        val parser = RxClick()
        val flow = parser.attach(flowOf(ByteArray(0)))
        assertTrue(flow.toList().isEmpty())
    }

    // ---- RxIMU ----

    @Test
    fun `imu parses 6 float values`() = runTest {
        val buf = ByteBuffer.allocate(25).order(ByteOrder.LITTLE_ENDIAN)
        buf.put(0x0A.toByte()) // msgCode
        buf.putFloat(1.0f)     // compassX
        buf.putFloat(2.0f)     // compassY
        buf.putFloat(3.0f)     // compassZ
        buf.putFloat(-1.0f)    // accelX
        buf.putFloat(0.5f)     // accelY
        buf.putFloat(9.8f)     // accelZ

        val parser = RxIMU()
        val result = parser.attach(flowOf(buf.array())).toList().single()
        assertEquals(1.0f, result.compassX)
        assertEquals(2.0f, result.compassY)
        assertEquals(3.0f, result.compassZ)
        assertEquals(-1.0f, result.accelX)
        assertEquals(0.5f, result.accelY)
        assertEquals(9.8f, result.accelZ)
    }

    @Test
    fun `imu filters out non-IMU packets`() = runTest {
        val parser = RxIMU()
        val flow = parser.attach(flowOf(byteArrayOf(0x0B, 0x01))) // click, not IMU
        assertTrue(flow.toList().isEmpty())
    }

    @Test
    fun `tap groups taps inside threshold`() = runTest {
        val packets = Channel<ByteArray>(Channel.UNLIMITED)
        var now = 0L
        val parser = RxTap(thresholdMs = 100, nowMs = { now })
        val values = mutableListOf<Int>()
        val job = backgroundScope.launch {
            parser.attach(packets.receiveAsFlow()).toList(values)
        }

        packets.send(byteArrayOf(0x09))
        runCurrent()
        now = 50L
        packets.send(byteArrayOf(0x09))
        advanceTimeBy(100)
        runCurrent()

        assertEquals(listOf(2), values)
        job.cancel()
    }

    // ---- RxPhoto ----

    @Test
    fun `photo reassembles chunks`() = runTest {
        val parser = RxPhoto()
        val flow = parser.attach(flowOf(
            byteArrayOf(0x07, 0x01, 0x02), // non-final
            byteArrayOf(0x07, 0x03),       // non-final
            byteArrayOf(0x08, 0x04),       // final
        ))
        val result = flow.toList().single()
        assertArrayEquals(byteArrayOf(0x01, 0x02, 0x03, 0x04), result)
    }

    // ---- RxMeteringData ----

    @Test
    fun `metering data parses unsigned channels`() = runTest {
        val parser = RxMeteringData()
        val result = parser.attach(flowOf(byteArrayOf(0x12, 1, 2, 3, 4, 5, 0xFF.toByte()))).toList().single()
        assertEquals(MeteringData(1, 2, 3, 4, 5, 255), result)
    }

    // ---- RxAutoExpResult ----

    @Test
    fun `auto exposure result parses 16 float values`() = runTest {
        val buf = ByteBuffer.allocate(65).order(ByteOrder.LITTLE_ENDIAN)
        buf.put(0x11.toByte())
        repeat(16) { buf.putFloat((it + 1).toFloat()) }

        val parser = RxAutoExpResult()
        val result = parser.attach(flowOf(buf.array())).toList().single()

        assertEquals(1.0f, result.error)
        assertEquals(6.0f, result.blueGain)
        assertEquals(7.0f, result.brightness.centerWeightedAverage)
        assertEquals(12.0f, result.brightness.matrix.average)
        assertEquals(16.0f, result.brightness.spot.average)
    }

    // ---- RxAudio ----

    @Test
    fun `audio reassembles chunks`() = runTest {
        val parser = RxAudio()
        val flow = parser.attach(flowOf(
            byteArrayOf(0x05, 0x10, 0x20),
            byteArrayOf(0x06, 0x30),
        ))
        val result = flow.toList().single()
        assertArrayEquals(byteArrayOf(0x10, 0x20, 0x30), result)
    }

    @Test
    fun `audio creates wav bytes`() {
        val wav = RxAudio.toWavBytes(byteArrayOf(0x01, 0x02), sampleRate = 8000, bitsPerSample = 16, channels = 1)
        assertEquals('R'.code.toByte(), wav[0])
        assertEquals('W'.code.toByte(), wav[8])
        assertEquals('d'.code.toByte(), wav[36])
        assertEquals(46, wav.size)
    }
}
