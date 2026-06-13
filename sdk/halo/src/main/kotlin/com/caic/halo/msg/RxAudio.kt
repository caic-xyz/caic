// RxAudio: reassemble or stream audio chunks from dataResponse and emit WAV bytes for PCM clips.
package com.caic.halo.msg

import com.caic.halo.ble.HaloDevice
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.launch

class RxAudio(
    private val nonFinalFlag: Int = HaloMessageCodes.RX_AUDIO_NON_FINAL,
    private val finalFlag: Int = HaloMessageCodes.RX_AUDIO_FINAL,
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

    fun attachStreaming(dataResponse: Flow<ByteArray>): Flow<ByteArray> = callbackFlow {
        val collector = launch {
            dataResponse
                .filter { it.isNotEmpty() && (it[0].toInt() and 0xFF == nonFinalFlag || it[0].toInt() and 0xFF == finalFlag) }
                .collect { data ->
                    if (data.size > 1) trySend(data.copyOfRange(1, data.size))
                    if (data[0].toInt() and 0xFF == finalFlag) close()
                }
        }

        awaitClose { collector.cancel() }
    }

    companion object {
        fun toWavBytes(
            pcmData: ByteArray,
            sampleRate: Int = 8000,
            bitsPerSample: Int = 16,
            channels: Int = 1,
        ): ByteArray {
            val byteRate = sampleRate * channels * bitsPerSample / 8
            val blockAlign = channels * bitsPerSample / 8
            val fileSize = 36 + pcmData.size
            return byteArrayOf(
                0x52, 0x49, 0x46, 0x46,
                fileSize.toByte(), (fileSize shr 8).toByte(), (fileSize shr 16).toByte(), (fileSize shr 24).toByte(),
                0x57, 0x41, 0x56, 0x45,
                0x66, 0x6D, 0x74, 0x20,
                0x10, 0x00, 0x00, 0x00,
                0x01, 0x00,
                channels.toByte(), 0x00,
                sampleRate.toByte(), (sampleRate shr 8).toByte(), (sampleRate shr 16).toByte(), (sampleRate shr 24).toByte(),
                byteRate.toByte(), (byteRate shr 8).toByte(), (byteRate shr 16).toByte(), (byteRate shr 24).toByte(),
                blockAlign.toByte(), 0x00,
                bitsPerSample.toByte(), 0x00,
                0x64, 0x61, 0x74, 0x61,
                pcmData.size.toByte(), (pcmData.size shr 8).toByte(), (pcmData.size shr 16).toByte(), (pcmData.size shr 24).toByte(),
            ) + pcmData
        }
    }
}
