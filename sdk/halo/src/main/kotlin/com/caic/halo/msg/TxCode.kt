// TxCode: send a single byte value to the device.
// Wire format: value(1).
package com.caic.halo.msg

data class TxCode(val value: Byte) : TxMessage {
    override fun pack(): ByteArray = byteArrayOf(value)
}
