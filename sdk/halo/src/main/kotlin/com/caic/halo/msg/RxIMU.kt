// RxIMU: parse IMU data (6 × float32) from dataResponse.
// msgCode 0x0A, payload: 24 bytes (6 little-endian IEEE 754 floats).
// Order: compassX, compassY, compassZ, accelX, accelY, accelZ.
package com.caic.halo.msg

import com.caic.halo.ble.HaloDevice
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.map
import java.nio.ByteBuffer
import java.nio.ByteOrder

data class IMUData(
    val compassX: Float, val compassY: Float, val compassZ: Float,
    val accelX: Float, val accelY: Float, val accelZ: Float,
)

class RxIMU(private val msgCode: Int = HaloMessageCodes.RX_IMU) {

    /** Attach to [HaloDevice.dataResponse] and emit [IMUData] values. */
    fun attach(dataResponse: Flow<ByteArray>): Flow<IMUData> = dataResponse
        .filter { it.isNotEmpty() && it[0].toInt() and 0xFF == msgCode }
        .map { data ->
            require(data.size >= 25) { "IMU packet too short: ${data.size} bytes" }
            val buf = ByteBuffer.wrap(data, 1, 24).order(ByteOrder.LITTLE_ENDIAN)
            IMUData(
                compassX = buf.getFloat(), compassY = buf.getFloat(), compassZ = buf.getFloat(),
                accelX = buf.getFloat(), accelY = buf.getFloat(), accelZ = buf.getFloat(),
            )
        }
}
