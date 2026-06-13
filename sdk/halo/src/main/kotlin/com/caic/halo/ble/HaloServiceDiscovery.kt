// Pure functions for parsing GATT services/characteristics from a Halo or Frame device.
// No Android BLE I/O — these operate on already-discovered data structures.
package com.caic.halo.ble

import android.bluetooth.BluetoothGattCharacteristic
import android.bluetooth.BluetoothGattService
object HaloServiceDiscovery {

    // ---- UUID aliases ----

    val LUA_SERVICE = HaloProtocol.LUA_SERVICE
    val LUA_TX_CHAR = HaloProtocol.LUA_TX_CHAR
    val LUA_RX_CHAR = HaloProtocol.LUA_RX_CHAR
    val AUDIO_TX_CHAR = HaloProtocol.AUDIO_TX_CHAR
    val OTA_SERVICE = HaloProtocol.OTA_SERVICE
    val SMP_CHAR = HaloProtocol.SMP_CHAR
    val LEGACY_DFU_SERVICE = HaloProtocol.LEGACY_DFU_SERVICE
    val BATTERY_SERVICE = HaloProtocol.BATTERY_SERVICE
    val BATTERY_LEVEL_CHAR = HaloProtocol.BATTERY_LEVEL_CHAR

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
        return PayloadLimits(
            maxStringLen = HaloProtocol.maxStringLength(mtu),
            maxDataLen = HaloProtocol.maxDataLength(mtu, type),
        )
    }

    data class PayloadLimits(val maxStringLen: Int, val maxDataLen: Int)
}
