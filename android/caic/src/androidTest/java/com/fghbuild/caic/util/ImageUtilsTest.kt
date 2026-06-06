// Instrumented tests for screenshot capture image utility functions.
package com.fghbuild.caic.util

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.util.Base64
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ImageUtilsTest {

    @Test
    fun bitmapToImageData_smallBitmap_returnsJpegWithCorrectMediaType() {
        val bmp = Bitmap.createBitmap(100, 100, Bitmap.Config.ARGB_8888)
        val result = bitmapToImageData(bmp)
        bmp.recycle()

        assertEquals("image/jpeg", result.mediaType)
        assertTrue("data must be non-empty", result.data.isNotEmpty())
    }

    @Test
    fun bitmapToImageData_smallBitmap_producesDecodableJpeg() {
        val bmp = Bitmap.createBitmap(200, 150, Bitmap.Config.ARGB_8888)
        val result = bitmapToImageData(bmp)
        bmp.recycle()

        val bytes = Base64.decode(result.data, Base64.DEFAULT)
        val decoded = BitmapFactory.decodeByteArray(bytes, 0, bytes.size)
        assertNotNull("decoded bitmap must not be null", decoded)
        decoded?.recycle()
    }

    @Test
    fun bitmapToImageData_largeBitmap_downscalesToMaxDimension() {
        val largeWidth = 3000
        val largeHeight = 2000
        val bmp = Bitmap.createBitmap(largeWidth, largeHeight, Bitmap.Config.ARGB_8888)
        val result = bitmapToImageData(bmp)
        bmp.recycle()

        val bytes = Base64.decode(result.data, Base64.DEFAULT)
        val decoded = BitmapFactory.decodeByteArray(bytes, 0, bytes.size)
        assertNotNull("decoded bitmap must not be null", decoded)
        val maxDim = maxOf(decoded!!.width, decoded.height)
        assertTrue("max dimension $maxDim should be <= 1568", maxDim <= 1568)
        decoded.recycle()
    }

    @Test
    fun bitmapToImageData_squareLargeBitmap_preservesAspectRatio() {
        val bmp = Bitmap.createBitmap(4000, 2000, Bitmap.Config.ARGB_8888)
        val result = bitmapToImageData(bmp)
        bmp.recycle()

        val bytes = Base64.decode(result.data, Base64.DEFAULT)
        val decoded = BitmapFactory.decodeByteArray(bytes, 0, bytes.size)
        assertNotNull(decoded)
        // Original ratio is 2:1; scaled width should be ~2x height.
        val ratio = decoded!!.width.toDouble() / decoded.height.toDouble()
        assertTrue("aspect ratio $ratio should be close to 2.0", ratio in 1.9..2.1)
        decoded.recycle()
    }

    @Test
    fun bitmapToImageData_doesNotRecycleBitmap() {
        val bmp = Bitmap.createBitmap(100, 100, Bitmap.Config.ARGB_8888)
        bitmapToImageData(bmp)
        // Bitmap must still be usable after the call.
        assertTrue("bitmap must not be recycled by bitmapToImageData", !bmp.isRecycled)
        bmp.recycle()
    }
}
