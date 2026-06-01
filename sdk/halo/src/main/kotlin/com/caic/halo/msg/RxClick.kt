// RxClick: parse Halo button click events from dataResponse.
// msgCode 0x0B, payload: [type] where 1=single, 2=double, 3=long.
package com.caic.halo.msg

import com.caic.halo.ble.HaloDevice
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.map

enum class ClickType { SINGLE, DOUBLE, LONG }

class RxClick(private val msgCode: Int = 0x0B) {

    /** Attach to [HaloDevice.dataResponse] and emit [ClickType] events. */
    fun attach(dataResponse: Flow<ByteArray>): Flow<ClickType> = dataResponse
        .filter { it.isNotEmpty() && it[0].toInt() and 0xFF == msgCode }
        .map { data ->
            when (data.getOrNull(1)?.toInt()) {
                1 -> ClickType.SINGLE
                2 -> ClickType.DOUBLE
                3 -> ClickType.LONG
                else -> throw IllegalArgumentException("Unknown click type: ${data.getOrNull(1)}")
            }
        }
}
