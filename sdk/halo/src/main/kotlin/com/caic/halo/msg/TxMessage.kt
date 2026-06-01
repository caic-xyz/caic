// TxMessage: base type for messages the host sends to the device via HaloDevice.sendMessage().
package com.caic.halo.msg

interface TxMessage {
    fun pack(): ByteArray
}
