// Core message types for the Halo messaging protocol (haloside applications).
// Tx (host→device): typed messages sent via HaloDevice.sendMessage().
// Rx (device→host): parsed from HaloDevice.dataResponse Flow.
package com.caic.halo.msg

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import java.io.ByteArrayOutputStream

// =============================================================================
// TX: Host → Device messages
// =============================================================================

/** Base type for messages the host sends to the device via [HaloDevice.sendMessage]. */
interface TxMessage {
    /** Serialize to the byte payload for sendMessage(). */
    fun pack(): ByteArray
}

/**
 * An indexed-color sprite sent to the device for display.
 *
 * Wire format: width(2) + height(2) + compressed(1) + bpp(1) + numColors(1) +
 *              palette(3 × numColors) + packedPixelData.
 *
 * bpp: 1, 2, or 4 (derived from numColors ≤2, ≤4, ≤16).
 */
data class TxSprite(
    val width: Int,
    val height: Int,
    val bpp: Int,
    val numColors: Int,
    val palette: ByteArray,   // RGB triplets, numColors × 3
    val pixels: ByteArray,    // packed pixel data
) : TxMessage {

    companion object {
        /** 16-color Halo palette (indices 0–15), RGB. */
        val HALO_PALETTE = byteArrayOf(
            0, 0, 0,          //  0 VOID
            -1, -1, -1,        //  1 WHITE
            -99, -99, -99,     //  2 GREY
            -66, 38, 51,       //  3 RED
            -32, 111, -117,    //  4 PINK
            73, 60, 43,        //  5 DARKBROWN
            -92, 100, 34,      //  6 BROWN
            -21, -119, 49,    //  7 ORANGE
            -9, -30, 107,     //  8 YELLOW
            47, 72, 78,        //  9 DARKGREEN
            68, -119, 26,      // 10 GREEN
            -93, -50, 39,     // 11 LIGHTGREEN
            27, 38, 50,        // 12 NIGHTBLUE
            0, 87, -124,       // 13 SEABLUE
            49, -94, -14,      // 14 SKYBLUE
            -78, -36, -17,    // 15 CLOUDBLUE
        )

        /**
         * Create a TxSprite from a PNG byte array. The PNG must be indexed-color
         * with ≤16 colors. Non-indexed PNGs are quantized.
         */
        fun fromPng(pngBytes: ByteArray): TxSprite {
            val opts = BitmapFactory.Options().apply { inMutable = false }
            val bitmap = BitmapFactory.decodeByteArray(pngBytes, 0, pngBytes.size, opts)
                ?: throw IllegalArgumentException("Failed to decode PNG")

            // Quantize to ≤16 colors if needed.
            val (quantized, palette, pixels) = quantize(bitmap)
            val bpp = when {
                palette.size / 3 <= 2 -> 1
                palette.size / 3 <= 4 -> 2
                else -> 4
            }

            return TxSprite(
                width = quantized.width,
                height = quantized.height,
                bpp = bpp,
                numColors = palette.size / 3,
                palette = palette,
                pixels = packPixels(quantized, palette, bpp),
            )
        }

        private fun quantize(bitmap: Bitmap): Triple<Bitmap, ByteArray, IntArray> {
            // Simple median-cut-like quantization to ≤16 colors.
            val width = bitmap.width
            val height = bitmap.height
            val maxDim = 640.coerceAtMost(if (width > height) width else height)
            val scaled = if (width > maxDim || height > maxDim) {
                val scale = maxDim.toFloat() / maxOf(width, height)
                Bitmap.createScaledBitmap(bitmap, (width * scale).toInt(), (height * scale).toInt(), true)
            } else bitmap

            // Collect all unique colors.
            val colorMap = LinkedHashMap<Int, Int>() // ARGB → index
            val pixelIndices = IntArray(scaled.width * scaled.height)
            for (y in 0 until scaled.height) {
                for (x in 0 until scaled.width) {
                    val argb = scaled.getPixel(x, y)
                    val rgb = argb and 0x00FFFFFF
                    val idx = colorMap.getOrPut(rgb) { colorMap.size }
                    pixelIndices[y * scaled.width + x] = idx
                }
            }

            val numColors = colorMap.size.coerceAtMost(16)
            val paletteBytes = ByteArray(numColors * 3)
            val entries = colorMap.entries.take(numColors)
            entries.forEachIndexed { i, (rgb, _) ->
                paletteBytes[i * 3] = ((rgb shr 16) and 0xFF).toByte()
                paletteBytes[i * 3 + 1] = ((rgb shr 8) and 0xFF).toByte()
                paletteBytes[i * 3 + 2] = (rgb and 0xFF).toByte()
            }

            // Remap pixel indices if we truncated colors.
            if (numColors < colorMap.size) {
                for (i in pixelIndices.indices) {
                    pixelIndices[i] = pixelIndices[i].coerceAtMost(numColors - 1)
                }
            }

            return Triple(scaled, paletteBytes, pixelIndices)
        }

        private fun packPixels(bitmap: Bitmap, palette: ByteArray, bpp: Int): ByteArray {
            val pixels = IntArray(bitmap.width * bitmap.height)
            bitmap.getPixels(pixels, 0, bitmap.width, 0, 0, bitmap.width, bitmap.height)

            // Map each pixel to the closest palette index.
            val numColors = palette.size / 3
            for (i in pixels.indices) {
                val rgb = pixels[i] and 0x00FFFFFF
                var bestIdx = 0
                var bestDist = Int.MAX_VALUE
                for (j in 0 until numColors) {
                    val pr = palette[j * 3].toInt() and 0xFF
                    val pg = palette[j * 3 + 1].toInt() and 0xFF
                    val pb = palette[j * 3 + 2].toInt() and 0xFF
                    val dr = ((rgb shr 16) and 0xFF) - pr
                    val dg = ((rgb shr 8) and 0xFF) - pg
                    val db = (rgb and 0xFF) - pb
                    val dist = dr * dr + dg * dg + db * db
                    if (dist < bestDist) { bestDist = dist; bestIdx = j }
                }
                pixels[i] = bestIdx
            }

            return packBits(pixels, bpp)
        }

        internal fun packBits(indices: IntArray, bpp: Int): ByteArray {
            return when (bpp) {
                1 -> {
                    ByteArray((indices.size + 7) / 8).also { out ->
                        for (i in indices.indices) {
                            out[i / 8] = (out[i / 8].toInt() or ((indices[i] and 0x01) shl (7 - i % 8))).toByte()
                        }
                    }
                }
                2 -> {
                    ByteArray((indices.size + 3) / 4).also { out ->
                        for (i in indices.indices) {
                            val shift = (3 - i % 4) * 2
                            out[i / 4] = (out[i / 4].toInt() or ((indices[i] and 0x03) shl shift)).toByte()
                        }
                    }
                }
                4 -> {
                    ByteArray((indices.size + 1) / 2).also { out ->
                        for (i in indices.indices) {
                            val shift = (1 - i % 2) * 4
                            out[i / 2] = (out[i / 2].toInt() or ((indices[i] and 0x0F) shl shift)).toByte()
                        }
                    }
                }
                else -> throw IllegalArgumentException("Unsupported bpp: $bpp")
            }
        }
    }

    override fun pack(): ByteArray {
        val header = ByteArray(7)
        header[0] = (width shr 8).toByte()
        header[1] = (width and 0xFF).toByte()
        header[2] = (height shr 8).toByte()
        header[3] = (height and 0xFF).toByte()
        header[4] = 0 // compressed flag
        header[5] = bpp.toByte()
        header[6] = numColors.toByte()

        return header + palette + pixels
    }
}
