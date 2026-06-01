// Pure functions for parsing GATT services/characteristics from a Halo or Frame device.
// No Android BLE I/O — these operate on already-discovered data structures.
package com.caic.halo.ble

import android.bluetooth.BluetoothGattCharacteristic
import android.bluetooth.BluetoothGattService
import java.util.UUID

object HaloServiceDiscovery {

    // ---- UUIDs ----

    val LUA_SERVICE: UUID = UUID.fromString("7A230001-5475-A6A4-654C-8431F6AD49C4")
    val LUA_TX_CHAR: UUID = UUID.fromString("7A230002-5475-A6A4-654C-8431F6AD49C4")
    val LUA_RX_CHAR: UUID = UUID.fromString("7A230003-5475-A6A4-654C-8431F6AD49C4")
    val AUDIO_TX_CHAR: UUID = UUID.fromString("7A230005-5475-A6A4-654C-8431F6AD49C4")
    val DFU_SERVICE: UUID = UUID.fromString("0000FE59-0000-1000-8000-00805F9B34FB")
    val BATTERY_SERVICE: UUID = UUID.fromString("0000180F-0000-1000-8000-00805F9B34FB")
    val BATTERY_LEVEL_CHAR: UUID = UUID.fromString("00002A19-0000-1000-8000-00805F9B34FB")

    // ---- Parsing ----

    /**
     * Find the Brilliant Lua service among the discovered GATT services.
     * Returns null if not present (device is not a Halo/Frame, or in DFU mode).
     */
    fun findLuaService(services: List<BluetoothGattService>): BluetoothGattService? =
        services.find { it.uuid == LUA_SERVICE }

    /**
     * Determine the device type from the presence of the AUDIO TX characteristic.
     * Halo has AUDIO TX; Frame does not.
     */
    fun deviceType(service: BluetoothGattService): HaloDeviceType =
        if (service.getCharacteristic(AUDIO_TX_CHAR) != null) HaloDeviceType.HALO
        else HaloDeviceType.FRAME

    /**
     * Validate that the Lua service has the required TX and RX characteristics.
     * Returns the TX, RX, and optional AUDIO TX characteristics.
     * Throws [HaloException] if required characteristics are missing.
     */
    fun requiredCharacteristics(service: BluetoothGattService): Triple<
        BluetoothGattCharacteristic,  // TX
        BluetoothGattCharacteristic,  // RX
        BluetoothGattCharacteristic?, // AUDIO TX (null on Frame)
    > {
        val tx = service.getCharacteristic(LUA_TX_CHAR)
            ?: throw HaloException("LUA TX characteristic not found")
        val rx = service.getCharacteristic(LUA_RX_CHAR)
            ?: throw HaloException("LUA RX characteristic not found")
        val audioTx = service.getCharacteristic(AUDIO_TX_CHAR)
        return Triple(tx, rx, audioTx)
    }

    // ---- Payload limits ----

    /**
     * Compute maximum Lua string and data payload lengths from the negotiated MTU.
     *
     * Lua string overhead: 3 bytes (ATT header)
     * Raw data overhead: 4 bytes (ATT header + 0x01 prefix)
     * Halo data overhead: 6 bytes (ATT header + 0x01 prefix + 2 extra for AUDIO TX coexistence)
     */
    fun payloadLimits(mtu: Int, type: HaloDeviceType): PayloadLimits {
        require(mtu >= 23) { "MTU must be at least 23, got $mtu" }
        val maxString = mtu - 3
        val maxData = if (type == HaloDeviceType.HALO) mtu - 6 else mtu - 4
        return PayloadLimits(maxString, maxData)
    }

    data class PayloadLimits(val maxStringLen: Int, val maxDataLen: Int)
}
