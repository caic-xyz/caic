// HaloMessageCodes: default Brilliant message-code assignments for host↔device typed messages.
package com.caic.halo.msg

object HaloMessageCodes {
    const val RX_AUDIO_NON_FINAL = 0x05
    const val RX_AUDIO_FINAL = 0x06
    const val RX_PHOTO_NON_FINAL = 0x07
    const val RX_PHOTO_FINAL = 0x08
    const val RX_TAP = 0x09
    const val RX_IMU = 0x0A
    const val RX_CLICK = 0x0B
    const val RX_AUTO_EXPOSURE_RESULT = 0x11
    const val RX_METERING_DATA = 0x12

    const val TX_USER_SPRITE_BASE = 0x20
}
