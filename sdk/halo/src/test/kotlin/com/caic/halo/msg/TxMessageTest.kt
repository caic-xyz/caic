// Unit tests for TxMessage types: wire format packing.
package com.caic.halo.msg

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner

@RunWith(RobolectricTestRunner::class)
class TxMessageTest {

    // ---- TxPlainText ----

    @Test
    fun `plain text pack format`() {
        val msg = TxPlainText("Hi", x = 10, y = 20, paletteOffset = 3, spacing = 2)
        val packed = msg.pack()

        assertEquals(6 + 2, packed.size) // 6 header + 2 for "Hi"
        assertEquals(0x00, packed[0].toInt() and 0xFF) // x high
        assertEquals(0x0A, packed[1].toInt() and 0xFF) // x low = 10
        assertEquals(0x00, packed[2].toInt() and 0xFF) // y high
        assertEquals(0x14, packed[3].toInt() and 0xFF) // y low = 20
        assertEquals(0x03, packed[4].toInt() and 0xFF) // paletteOffset
        assertEquals(0x02, packed[5].toInt() and 0xFF) // spacing
        assertEquals('H'.code.toByte(), packed[6])
        assertEquals('i'.code.toByte(), packed[7])
    }

    @Test
    fun `plain text rejects invalid palette offset`() {
        try {
            TxPlainText("x", paletteOffset = 0)
            fail()
        } catch (e: IllegalArgumentException) {
            assertTrue(e.message!!.contains("1–15"))
        }
        try {
            TxPlainText("x", paletteOffset = 16)
            fail()
        } catch (e: IllegalArgumentException) {
            assertTrue(e.message!!.contains("1–15"))
        }
    }

    @Test
    fun `plain text rejects out-of-bounds coordinates`() {
        try { TxPlainText("x", x = 0); fail() } catch (_: IllegalArgumentException) { }
        try { TxPlainText("x", x = 257); fail() } catch (_: IllegalArgumentException) { }
        try { TxPlainText("x", y = 0); fail() } catch (_: IllegalArgumentException) { }
        try { TxPlainText("x", y = 257); fail() } catch (_: IllegalArgumentException) { }
    }

    // ---- TxCode ----

    @Test
    fun `code pack is single byte`() {
        assertEquals(1, TxCode(0x42.toByte()).pack().size)
        assertEquals(0x42.toByte(), TxCode(0x42.toByte()).pack()[0])
    }

    // ---- TxSprite ----

    @Test
    fun `sprite pack format`() {
        val palette = byteArrayOf(0, 0, 0, -1, -1, -1) // black, white
        val pixels = byteArrayOf(0x55.toByte()) // packed 2bpp: 01 01 01 01
        val sprite = TxSprite(4, 4, 2, 2, palette, pixels)

        val packed = sprite.pack()
        // header(7) + palette(6) + pixels(1) = 14
        assertEquals(14, packed.size)
        assertEquals(0x00, packed[0].toInt() and 0xFF) // width high
        assertEquals(0x04, packed[1].toInt() and 0xFF) // width low = 4
        assertEquals(0x00, packed[2].toInt() and 0xFF) // height high
        assertEquals(0x04, packed[3].toInt() and 0xFF) // height low = 4
        assertEquals(0x00, packed[4].toInt() and 0xFF) // compressed = 0
        assertEquals(0x02, packed[5].toInt() and 0xFF) // bpp = 2
        assertEquals(0x02, packed[6].toInt() and 0xFF) // numColors = 2
        // palette follows
        assertArrayEquals(palette, packed.copyOfRange(7, 13))
        // pixel data
        assertEquals(0x55.toByte(), packed[13])
    }

    @Test
    fun `packBits 1bpp`() {
        // 8 pixels: 0,1,0,1,0,1,0,1 → 01010101 = 0x55
        val result = TxSprite.packBits(intArrayOf(0, 1, 0, 1, 0, 1, 0, 1), 1)
        assertEquals(1, result.size)
        assertEquals(0x55.toByte(), result[0])
    }

    @Test
    fun `packBits 2bpp`() {
        // 4 pixels: 1,2,3,0 → 01 10 11 00 = 0x6C
        val result = TxSprite.packBits(intArrayOf(1, 2, 3, 0), 2)
        assertEquals(1, result.size)
        assertEquals(0x6C.toByte(), result[0])
    }

    @Test
    fun `packBits 4bpp`() {
        // 2 pixels: 5,10 → 0101 1010 = 0x5A
        val result = TxSprite.packBits(intArrayOf(5, 10), 4)
        assertEquals(1, result.size)
        assertEquals(0x5A.toByte(), result[0])
    }
}
