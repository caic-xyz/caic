// TxPlainText: send plain text for display on the device.
// Wire format: x(2) + y(2) + paletteOffset(1) + spacing(1) + text(UTF-8).
package com.caic.halo.msg

data class TxPlainText(
    val text: String,
    val x: Int = 1,
    val y: Int = 1,
    val paletteOffset: Int = 1, // 1–15
    val spacing: Int = 4,
) : TxMessage {

    init {
        require(paletteOffset in 1..15) { "paletteOffset must be 1–15, got $paletteOffset" }
        require(x in 1..256) { "x must be 1–256 (Halo display), got $x" }
        require(y in 1..256) { "y must be 1–256 (Halo display), got $y" }
    }

    override fun pack(): ByteArray {
        val textBytes = text.toByteArray(Charsets.UTF_8)
        val out = ByteArray(6 + textBytes.size)
        out[0] = (x shr 8).toByte()
        out[1] = (x and 0xFF).toByte()
        out[2] = (y shr 8).toByte()
        out[3] = (y and 0xFF).toByte()
        out[4] = (paletteOffset and 0x0F).toByte()
        out[5] = (spacing and 0xFF).toByte()
        textBytes.copyInto(out, 6)
        return out
    }
}
