// TxAutoExpSettings: automatic exposure and white-balance settings sent to the device.
package com.caic.halo.msg

import kotlin.math.roundToInt

data class TxAutoExpSettings(
    val meteringIndex: Int = 1,
    val exposure: Double = 0.1,
    val exposureSpeed: Double = 0.45,
    val shutterLimit: Int = 16_383,
    val analogGainLimit: Int = 16,
    val whiteBalanceSpeed: Double = 0.5,
    val rgbGainLimit: Int = 287,
) : TxMessage {
    init {
        require(meteringIndex in 0..2) { "meteringIndex must be 0..2, got $meteringIndex" }
        requireUnitInterval("exposure", exposure)
        requireUnitInterval("exposureSpeed", exposureSpeed)
        require(shutterLimit in 4..16_383) { "shutterLimit must be 4..16383, got $shutterLimit" }
        require(analogGainLimit in 0..248) { "analogGainLimit must be 0..248, got $analogGainLimit" }
        requireUnitInterval("whiteBalanceSpeed", whiteBalanceSpeed)
        require(rgbGainLimit in 0..1023) { "rgbGainLimit must be 0..1023, got $rgbGainLimit" }
    }

    override fun pack(): ByteArray {
        val shutter = shutterLimit.toUInt16Bytes()
        val rgbGain = rgbGainLimit.toUInt16Bytes()
        return byteArrayOf(
            meteringIndex.toByte(),
            exposure.toUnitByte(),
            exposureSpeed.toUnitByte(),
            shutter[0],
            shutter[1],
            analogGainLimit.toByte(),
            whiteBalanceSpeed.toUnitByte(),
            rgbGain[0],
            rgbGain[1],
        )
    }
}

private fun requireUnitInterval(name: String, value: Double) {
    require(value in 0.0..1.0) { "$name must be 0.0..1.0, got $value" }
}

private fun Double.toUnitByte(): Byte = (this * 255.0).roundToInt().coerceIn(0, 255).toByte()

private fun Int.toUInt16Bytes(): ByteArray = byteArrayOf((this shr 8).toByte(), (this and 0xFF).toByte())
