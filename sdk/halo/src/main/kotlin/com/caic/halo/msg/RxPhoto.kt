// RxPhoto: reassemble JPEG photo chunks from dataResponse. Emits a single-element Flow.
package com.caic.halo.msg

import com.caic.halo.ble.HaloDevice
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.launch

class RxPhoto(
    private val nonFinalFlag: Int = HaloMessageCodes.RX_PHOTO_NON_FINAL,
    private val finalFlag: Int = HaloMessageCodes.RX_PHOTO_FINAL,
) {
    fun attach(dataResponse: Flow<ByteArray>): Flow<ByteArray> = callbackFlow {
        val buffer = mutableListOf<Byte>()

        val collector = launch {
            dataResponse
                .filter { it.isNotEmpty() && (it[0].toInt() and 0xFF == nonFinalFlag || it[0].toInt() and 0xFF == finalFlag) }
                .collect { data ->
                    buffer.addAll(data.copyOfRange(1, data.size).toList())
                    if (data[0].toInt() and 0xFF == finalFlag) {
                        trySend(buffer.toByteArray())
                        close()
                    }
                }
        }

        awaitClose { collector.cancel() }
    }
}
