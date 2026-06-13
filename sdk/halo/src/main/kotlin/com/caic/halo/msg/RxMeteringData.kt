// RxMeteringData: parse camera metering samples from dataResponse.
package com.caic.halo.msg

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.map

data class MeteringData(
    val spotR: Int,
    val spotG: Int,
    val spotB: Int,
    val matrixR: Int,
    val matrixG: Int,
    val matrixB: Int,
)

class RxMeteringData(private val msgCode: Int = HaloMessageCodes.RX_METERING_DATA) {
    fun attach(dataResponse: Flow<ByteArray>): Flow<MeteringData> = dataResponse
        .filter { it.isNotEmpty() && it[0].toInt() and 0xFF == msgCode }
        .map { data ->
            require(data.size >= 7) { "Metering packet too short: ${data.size} bytes" }
            MeteringData(
                spotR = data[1].toInt() and 0xFF,
                spotG = data[2].toInt() and 0xFF,
                spotB = data[3].toInt() and 0xFF,
                matrixR = data[4].toInt() and 0xFF,
                matrixG = data[5].toInt() and 0xFF,
                matrixB = data[6].toInt() and 0xFF,
            )
        }
}
