// RxTap: group Frame tap packets into multi-tap counts from dataResponse.
package com.caic.halo.msg

import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.launch

class RxTap(
    private val msgCode: Int = HaloMessageCodes.RX_TAP,
    private val thresholdMs: Long = 300,
    private val debounceMs: Long = 40,
    private val nowMs: () -> Long = { System.currentTimeMillis() },
) {
    fun attach(dataResponse: Flow<ByteArray>): Flow<Int> = callbackFlow {
        var lastTapTimeMs = Long.MIN_VALUE
        var taps = 0
        var pendingEmit = launch { }

        val collector = launch {
            dataResponse
                .filter { it.isNotEmpty() && it[0].toInt() and 0xFF == msgCode }
                .collect {
                    val now = nowMs()
                    if (lastTapTimeMs != Long.MIN_VALUE && now - lastTapTimeMs < debounceMs) {
                        lastTapTimeMs = now
                        return@collect
                    }

                    lastTapTimeMs = now
                    taps += 1
                    pendingEmit.cancel()
                    pendingEmit = launch {
                        delay(thresholdMs)
                        trySend(taps)
                        taps = 0
                    }
                }
        }

        awaitClose {
            collector.cancel()
            pendingEmit.cancel()
        }
    }
}
