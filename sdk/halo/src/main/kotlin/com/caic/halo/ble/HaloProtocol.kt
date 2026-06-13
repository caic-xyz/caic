// HaloProtocol: BLE UUIDs, control bytes, payload limits, and typed-message framing for Brilliant Halo/Frame devices.
package com.caic.halo.ble

import java.util.UUID

/** Wire-level constants for the Brilliant Halo/Frame BLE protocol. */
object HaloProtocol {
    /** Brilliant Lua GATT service used for Lua commands, raw data, and application messages. */
    val LUA_SERVICE: UUID = UUID.fromString("7A230001-5475-A6A4-654C-8431F6AD49C4")

    /** Host-to-device Lua command and raw data characteristic. */
    val LUA_TX_CHAR: UUID = UUID.fromString("7A230002-5475-A6A4-654C-8431F6AD49C4")

    /** Device-to-host Lua stdout and raw data notification characteristic. */
    val LUA_RX_CHAR: UUID = UUID.fromString("7A230003-5475-A6A4-654C-8431F6AD49C4")

    /** Halo-only host-to-device low-latency speaker audio characteristic. */
    val AUDIO_TX_CHAR: UUID = UUID.fromString("7A230005-5475-A6A4-654C-8431F6AD49C4")

    /** Standard BLE Battery service and Battery Level characteristic. */
    val BATTERY_SERVICE: UUID = UUID.fromString("0000180F-0000-1000-8000-00805F9B34FB")
    val BATTERY_LEVEL_CHAR: UUID = UUID.fromString("00002A19-0000-1000-8000-00805F9B34FB")

    /** MCUboot Simple Management Protocol firmware update service and SMP characteristic. */
    val OTA_SERVICE: UUID = UUID.fromString("8D53DC1D-1DB7-4CD3-868B-8A527460AA84")
    val SMP_CHAR: UUID = UUID.fromString("DA2E7828-FBCE-4E01-AE9E-261174997C48")

    /** Legacy Nordic DFU advertising service used by older Frame firmware. */
    val LEGACY_DFU_SERVICE: UUID = UUID.fromString("0000FE59-0000-1000-8000-00805F9B34FB")

    /** Raw binary data prefix on LUA TX/RX. Notifications without this prefix are UTF-8 Lua stdout. */
    const val DATA_PREFIX: Byte = 0x01

    /** Reboot the device. */
    const val CONTROL_REBOOT: Byte = 0x02

    /** Interrupt the running Lua script and return to the REPL. */
    const val CONTROL_BREAK: Byte = 0x03

    /** Restart the Lua VM and run main.lua when present. */
    const val CONTROL_RESET: Byte = 0x04

    /** Reset and remove main.lua. Halo-only in current firmware. */
    const val CONTROL_REMOVE_MAIN: Byte = 0x05

    /** Exit the Lua runtime completely. */
    const val CONTROL_EXIT_LUA: Byte = 0x06

    /** Remove all files and folders except settings. */
    const val CONTROL_REMOVE_ALL_FILES: Byte = 0x07

    /** Maximum Brilliant typed-message body length: 16-bit unsigned length field. */
    const val MAX_MESSAGE_PAYLOAD = 65_535

    /** First typed-message packet: DATA_PREFIX, msgCode, payload length high byte, payload length low byte. */
    const val FIRST_MESSAGE_HEADER_SIZE = 4

    /** Subsequent typed-message packet: DATA_PREFIX, msgCode. */
    const val SUBSEQUENT_MESSAGE_HEADER_SIZE = 2

    /** Successful data.lua receiver-paced ACK after DATA_PREFIX has been stripped from dataResponse. */
    val DATA_ACK_SUCCESS: ByteArray = byteArrayOf(0x00, 0x00)

    /** Failed data.lua receiver-paced ACK after DATA_PREFIX has been stripped from dataResponse. */
    val DATA_ACK_FAILURE: ByteArray = byteArrayOf(0x00, 0x01)

    /** Lua strings fit in MTU minus ATT overhead. */
    fun maxStringLength(mtu: Int): Int = mtu - 3

    /** Raw data payloads reserve ATT overhead, DATA_PREFIX, and two extra bytes on Halo when audio TX coexists. */
    fun maxDataLength(mtu: Int, type: HaloDeviceType): Int = if (type == HaloDeviceType.HALO) mtu - 6 else mtu - 4
}
