// Unit tests for HaloServiceDiscovery — pure functions, no Android BLE I/O.
package com.caic.halo.ble

import android.bluetooth.BluetoothGattCharacteristic
import android.bluetooth.BluetoothGattService
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import java.util.UUID

@RunWith(RobolectricTestRunner::class)
class HaloServiceDiscoveryTest {

    private fun makeChar(uuid: UUID): BluetoothGattCharacteristic =
        BluetoothGattCharacteristic(uuid, 0, 0)

    private fun makeService(uuid: UUID, vararg chars: BluetoothGattCharacteristic): BluetoothGattService {
        val svc = BluetoothGattService(uuid, BluetoothGattService.SERVICE_TYPE_PRIMARY)
        chars.forEach { svc.addCharacteristic(it) }
        return svc
    }

    // ---- findLuaService ----

    @Test
    fun `findLuaService returns matching service`() {
        val luaSvc = makeService(HaloServiceDiscovery.LUA_SERVICE)
        val other = makeService(UUID.randomUUID())
        assertEquals(luaSvc, HaloServiceDiscovery.findLuaService(listOf(other, luaSvc)))
    }

    @Test
    fun `findLuaService returns null when absent`() {
        val other = makeService(UUID.randomUUID())
        assertNull(HaloServiceDiscovery.findLuaService(listOf(other)))
    }

    @Test
    fun `findLuaService returns null for empty list`() {
        assertNull(HaloServiceDiscovery.findLuaService(emptyList()))
    }

    // ---- deviceType ----

    @Test
    fun `deviceType is HALO when AUDIO TX present`() {
        val svc = makeService(
            HaloServiceDiscovery.LUA_SERVICE,
            makeChar(HaloServiceDiscovery.LUA_TX_CHAR),
            makeChar(HaloServiceDiscovery.LUA_RX_CHAR),
            makeChar(HaloServiceDiscovery.AUDIO_TX_CHAR),
        )
        assertEquals(HaloDeviceType.HALO, HaloServiceDiscovery.deviceType(svc))
    }

    @Test
    fun `deviceType is FRAME when AUDIO TX absent`() {
        val svc = makeService(
            HaloServiceDiscovery.LUA_SERVICE,
            makeChar(HaloServiceDiscovery.LUA_TX_CHAR),
            makeChar(HaloServiceDiscovery.LUA_RX_CHAR),
        )
        assertEquals(HaloDeviceType.FRAME, HaloServiceDiscovery.deviceType(svc))
    }

    // ---- requiredCharacteristics ----

    @Test
    fun `requiredCharacteristics extracts all three on Halo`() {
        val tx = makeChar(HaloServiceDiscovery.LUA_TX_CHAR)
        val rx = makeChar(HaloServiceDiscovery.LUA_RX_CHAR)
        val audio = makeChar(HaloServiceDiscovery.AUDIO_TX_CHAR)
        val svc = makeService(HaloServiceDiscovery.LUA_SERVICE, tx, rx, audio)

        val (t, r, a) = HaloServiceDiscovery.requiredCharacteristics(svc)
        assertEquals(tx, t)
        assertEquals(rx, r)
        assertEquals(audio, a)
    }

    @Test
    fun `requiredCharacteristics returns null audio on Frame`() {
        val tx = makeChar(HaloServiceDiscovery.LUA_TX_CHAR)
        val rx = makeChar(HaloServiceDiscovery.LUA_RX_CHAR)
        val svc = makeService(HaloServiceDiscovery.LUA_SERVICE, tx, rx)

        val (_, _, a) = HaloServiceDiscovery.requiredCharacteristics(svc)
        assertNull(a)
    }

    @Test
    fun `requiredCharacteristics throws when TX missing`() {
        val rx = makeChar(HaloServiceDiscovery.LUA_RX_CHAR)
        val svc = makeService(HaloServiceDiscovery.LUA_SERVICE, rx)
        try {
            HaloServiceDiscovery.requiredCharacteristics(svc)
            fail("Expected HaloException")
        } catch (e: HaloException) {
            assertTrue(e.message!!.contains("TX"))
        }
    }

    @Test
    fun `requiredCharacteristics throws when RX missing`() {
        val tx = makeChar(HaloServiceDiscovery.LUA_TX_CHAR)
        val svc = makeService(HaloServiceDiscovery.LUA_SERVICE, tx)
        try {
            HaloServiceDiscovery.requiredCharacteristics(svc)
            fail("Expected HaloException")
        } catch (e: HaloException) {
            assertTrue(e.message!!.contains("RX"))
        }
    }

    // ---- payloadLimits ----

    @Test
    fun `payload limits at MTU 23`() {
        val limits = HaloServiceDiscovery.payloadLimits(23, HaloDeviceType.FRAME)
        assertEquals(20, limits.maxStringLen) // 23 - 3
        assertEquals(19, limits.maxDataLen)   // 23 - 4
    }

    @Test
    fun `payload limits at MTU 517 for Frame`() {
        val limits = HaloServiceDiscovery.payloadLimits(517, HaloDeviceType.FRAME)
        assertEquals(514, limits.maxStringLen) // 517 - 3
        assertEquals(513, limits.maxDataLen)   // 517 - 4
    }

    @Test
    fun `payload limits at MTU 517 for Halo`() {
        val limits = HaloServiceDiscovery.payloadLimits(517, HaloDeviceType.HALO)
        assertEquals(514, limits.maxStringLen) // 517 - 3
        assertEquals(511, limits.maxDataLen)   // 517 - 6
    }

    @Test
    fun `payload limits rejects MTU below 23`() {
        try {
            HaloServiceDiscovery.payloadLimits(22, HaloDeviceType.FRAME)
            fail("Expected IllegalArgumentException")
        } catch (e: IllegalArgumentException) {
            assertTrue(e.message!!.contains("23"))
        }
    }

    // ---- UUID constants ----

    @Test
    fun `Lua service UUID is correct`() {
        assertEquals(
            UUID.fromString("7A230001-5475-A6A4-654C-8431F6AD49C4"),
            HaloServiceDiscovery.LUA_SERVICE,
        )
    }

    @Test
    fun `Battery service is standard 0x180F`() {
        assertEquals(
            UUID.fromString("0000180F-0000-1000-8000-00805F9B34FB"),
            HaloServiceDiscovery.BATTERY_SERVICE,
        )
    }

    @Test
    fun `OTA service and SMP characteristic UUIDs are correct`() {
        assertEquals(
            UUID.fromString("8D53DC1D-1DB7-4CD3-868B-8A527460AA84"),
            HaloServiceDiscovery.OTA_SERVICE,
        )
        assertEquals(
            UUID.fromString("DA2E7828-FBCE-4E01-AE9E-261174997C48"),
            HaloServiceDiscovery.SMP_CHAR,
        )
    }
}
