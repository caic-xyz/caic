// TxCaptureSettings: camera capture request settings sent to the device.
package com.caic.halo.msg

data class TxCaptureSettings(
    val resolution: Int = 512,
    val qualityIndex: Int = 4,
    val pan: Int = 0,
    val raw: Boolean = false,
) : TxMessage {
    init {
        require(resolution in 100..720 && resolution % 2 == 0) { "resolution must be an even value in 100..720, got $resolution" }
        require(qualityIndex in 0..4) { "qualityIndex must be 0..4, got $qualityIndex" }
        require(pan in -140..140) { "pan must be -140..140, got $pan" }
    }

    override fun pack(): ByteArray {
        val halfResolution = resolution / 2
        val shiftedPan = pan + 140
        return byteArrayOf(
            qualityIndex.toByte(),
            (halfResolution shr 8).toByte(),
            (halfResolution and 0xFF).toByte(),
            (shiftedPan shr 8).toByte(),
            (shiftedPan and 0xFF).toByte(),
            if (raw) 0x01 else 0x00,
        )
    }
}
