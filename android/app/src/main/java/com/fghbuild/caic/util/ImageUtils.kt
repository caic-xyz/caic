// Utilities for converting content URIs to base64 ImageData for the API.
package com.fghbuild.caic.util

import android.content.ContentResolver
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.util.Base64
import androidx.core.graphics.scale
import com.caic.sdk.ImageData
import java.io.ByteArrayOutputStream

private const val MAX_DIMENSION = 1568
private const val JPEG_QUALITY = 85

/**
 * Reads an image from [uri], down-scales it if larger than [MAX_DIMENSION],
 * and returns an [ImageData] with base64-encoded data.
 * Returns null if the URI cannot be read or decoded.
 */
fun uriToImageData(contentResolver: ContentResolver, uri: Uri): ImageData? {
    val bytes = contentResolver.openInputStream(uri)?.use { it.readBytes() } ?: return null
    return bytesToImageData(bytes)
}

private fun bytesToImageData(bytes: ByteArray): ImageData? {
    val opts = BitmapFactory.Options().apply { inJustDecodeBounds = true }
    BitmapFactory.decodeByteArray(bytes, 0, bytes.size, opts)
    val origW = opts.outWidth
    val origH = opts.outHeight
    if (origW <= 0 || origH <= 0) return null

    val mime = opts.outMimeType ?: "image/jpeg"
    // If small enough, send the raw bytes without re-encoding.
    if (origW <= MAX_DIMENSION && origH <= MAX_DIMENSION) {
        val mediaType = when {
            mime.contains("png") -> "image/png"
            mime.contains("webp") -> "image/webp"
            mime.contains("gif") -> "image/gif"
            else -> "image/jpeg"
        }
        return ImageData(
            mediaType = mediaType,
            data = Base64.encodeToString(bytes, Base64.NO_WRAP),
        )
    }
    // Down-scale preserving aspect ratio and re-encode as JPEG.
    return downscaleToImageData(bytes, origW, origH)
}

private fun downscaleToImageData(bytes: ByteArray, origW: Int, origH: Int): ImageData? {
    val scale = MAX_DIMENSION.toFloat() / maxOf(origW, origH)
    val newW = (origW * scale).toInt()
    val newH = (origH * scale).toInt()
    val full = BitmapFactory.decodeByteArray(bytes, 0, bytes.size) ?: return null
    val scaled = full.scale(newW, newH, filter = true)
    if (scaled !== full) full.recycle()
    val out = ByteArrayOutputStream()
    scaled.compress(Bitmap.CompressFormat.JPEG, JPEG_QUALITY, out)
    scaled.recycle()
    return ImageData(
        mediaType = "image/jpeg",
        data = Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP),
    )
}

/**
 * Decodes a base64 [ImageData] into an Android [Bitmap], or null on failure.
 */
fun imageDataToBitmap(img: ImageData): Bitmap? {
    val bytes = Base64.decode(img.data, Base64.DEFAULT)
    return BitmapFactory.decodeByteArray(bytes, 0, bytes.size)
}
