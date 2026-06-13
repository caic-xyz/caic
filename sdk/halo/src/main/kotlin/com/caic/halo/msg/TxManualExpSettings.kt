// TxManualExpSettings: manual camera exposure and gain settings sent to the device.
package com.caic.halo.msg

data class TxManualExpSettings(
    val manualShutter: Int = 4096,
    val manualAnalogGain: Int = 1,
    val manualRedGain: Int = 121,
    val manualGreenGain: Int = 64,
    val manualBlueGain: Int = 140,
) : TxMessage {
    init {
        require(manualShutter in 4..16_383) { "manualShutter must be 4..16383, got $manualShutter" }
        require(manualAnalogGain in 0..248) { "manualAnalogGain must be 0..248, got $manualAnalogGain" }
        require(manualRedGain in 0..1023) { "manualRedGain must be 0..1023, got $manualRedGain" }
        require(manualGreenGain in 0..1023) { "manualGreenGain must be 0..1023, got $manualGreenGain" }
        require(manualBlueGain in 0..1023) { "manualBlueGain must be 0..1023, got $manualBlueGain" }
    }

    override fun pack(): ByteArray {
        val shutter = manualShutter.toUInt16Bytes()
        val red = manualRedGain.toUInt10Bytes()
        val green = manualGreenGain.toUInt10Bytes()
        val blue = manualBlueGain.toUInt10Bytes()
        return byteArrayOf(
            shutter[0],
            shutter[1],
            manualAnalogGain.toByte(),
            red[0],
            red[1],
            green[0],
            green[1],
            blue[0],
            blue[1],
        )
    }

    private fun Int.toUInt16Bytes(): ByteArray = byteArrayOf((this shr 8).toByte(), (this and 0xFF).toByte())

    private fun Int.toUInt10Bytes(): ByteArray = byteArrayOf(((this shr 8) and 0x03).toByte(), (this and 0xFF).toByte())
}
