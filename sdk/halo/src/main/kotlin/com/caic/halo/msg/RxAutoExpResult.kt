// RxAutoExpResult: parse auto-exposure and white-balance results from dataResponse.
package com.caic.halo.msg

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.map
import java.nio.ByteBuffer
import java.nio.ByteOrder

data class AutoExpResult(
    val error: Float,
    val shutter: Float,
    val analogGain: Float,
    val redGain: Float,
    val greenGain: Float,
    val blueGain: Float,
    val brightness: Brightness,
)

data class Brightness(
    val centerWeightedAverage: Float,
    val scene: Float,
    val matrix: ColorMeter,
    val spot: ColorMeter,
)

data class ColorMeter(
    val r: Float,
    val g: Float,
    val b: Float,
    val average: Float,
)

class RxAutoExpResult(private val msgCode: Int = HaloMessageCodes.RX_AUTO_EXPOSURE_RESULT) {
    fun attach(dataResponse: Flow<ByteArray>): Flow<AutoExpResult> = dataResponse
        .filter { it.isNotEmpty() && it[0].toInt() and 0xFF == msgCode }
        .map { data ->
            require(data.size >= AUTO_EXP_PACKET_SIZE) { "Auto exposure packet too short: ${data.size} bytes" }
            val floats = ByteBuffer.wrap(data, 1, AUTO_EXP_PAYLOAD_SIZE)
                .order(ByteOrder.LITTLE_ENDIAN)
                .let { buffer -> FloatArray(AUTO_EXP_FLOAT_COUNT) { buffer.getFloat() } }
            AutoExpResult(
                error = floats[0],
                shutter = floats[1],
                analogGain = floats[2],
                redGain = floats[3],
                greenGain = floats[4],
                blueGain = floats[5],
                brightness = Brightness(
                    centerWeightedAverage = floats[6],
                    scene = floats[7],
                    matrix = ColorMeter(floats[8], floats[9], floats[10], floats[11]),
                    spot = ColorMeter(floats[12], floats[13], floats[14], floats[15]),
                ),
            )
        }

    private companion object {
        const val AUTO_EXP_FLOAT_COUNT = 16
        const val AUTO_EXP_PAYLOAD_SIZE = AUTO_EXP_FLOAT_COUNT * 4
        const val AUTO_EXP_PACKET_SIZE = 1 + AUTO_EXP_PAYLOAD_SIZE
    }
}
