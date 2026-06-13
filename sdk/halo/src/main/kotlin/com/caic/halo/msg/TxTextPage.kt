// TxTextPage: layout and rasterize text pages for rectangular Frame and circular Halo displays.
package com.caic.halo.msg

import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.Paint
import android.graphics.Typeface
import kotlin.math.floor
import kotlin.math.max
import kotlin.math.min
import kotlin.math.sqrt

abstract class TextLayout(
    val width: Int,
    val height: Int,
    val fontSizePx: Int,
    val typeface: Typeface = Typeface.DEFAULT,
    val textAlign: Paint.Align = Paint.Align.LEFT,
) {
    init {
        require(width > 0) { "width must be positive" }
        require(height > 0) { "height must be positive" }
        require(fontSizePx > 0) { "fontSizePx must be positive" }
    }

    open val startY: Float = 0f

    abstract fun lineLayout(lineY: Float, lineHeight: Float): LineLayout?
}

data class LineLayout(val width: Int, val xOffset: Int)

class RectangularTextLayout(
    width: Int,
    height: Int,
    fontSizePx: Int,
    typeface: Typeface = Typeface.DEFAULT,
    textAlign: Paint.Align = Paint.Align.LEFT,
) : TextLayout(width, height, fontSizePx, typeface, textAlign) {
    override fun lineLayout(lineY: Float, lineHeight: Float): LineLayout? {
        if (lineY < 0f || lineY + lineHeight > height) return null
        return LineLayout(width = width, xOffset = 0)
    }
}

class CircularTextLayout(
    width: Int,
    height: Int,
    fontSizePx: Int,
    val circleMarginPx: Float = 15f,
    typeface: Typeface = Typeface.DEFAULT,
    textAlign: Paint.Align = Paint.Align.CENTER,
) : TextLayout(width, height, fontSizePx, typeface, textAlign) {
    private val radius = min(width, height) / 2f - circleMarginPx
    private val centerX = width / 2f
    private val centerY = height / 2f

    init {
        require(radius > 0f) { "circleMarginPx leaves no drawable area" }
    }

    override val startY: Float = centerY - radius

    override fun lineLayout(lineY: Float, lineHeight: Float): LineLayout? {
        val distanceFromCenter = lineY + lineHeight / 2f - centerY
        if (kotlin.math.abs(distanceFromCenter) > radius) return null
        val halfWidth = sqrt(radius * radius - distanceFromCenter * distanceFromCenter)
        val lineWidth = floor(halfWidth * 2f).toInt()
        if (lineWidth < fontSizePx) return null
        return LineLayout(width = lineWidth, xOffset = floor(centerX - halfWidth).toInt())
    }
}

class TxTextPage(
    private val layout: TextLayout,
    text: String,
) {
    private var remainingText = text.trim()

    val hasMoreText: Boolean get() = remainingText.isNotEmpty()

    fun remainingText(): String = remainingText

    fun measureNextPage(): TextPageData? {
        if (remainingText.isEmpty()) return null

        val paint = textPaint(layout)
        val lineHeight = max(layout.fontSizePx.toFloat(), paint.fontMetrics.let { it.descent - it.ascent })
        val lines = mutableListOf<TextLineData>()
        var pageRemainder = remainingText
        var y = layout.startY

        while (pageRemainder.isNotEmpty() && y + lineHeight <= layout.height) {
            val lineLayout = layout.lineLayout(y, lineHeight) ?: break
            val breakIndex = breakText(pageRemainder, paint, lineLayout.width.toFloat())
            if (breakIndex <= 0) break

            val lineText = pageRemainder.substring(0, breakIndex).trim()
            if (lineText.isNotEmpty()) {
                lines += TextLineData(
                    text = lineText,
                    width = lineLayout.width,
                    xOffset = lineLayout.xOffset,
                    yOffset = y.toInt(),
                    lineHeight = lineHeight.toInt(),
                )
            }
            pageRemainder = pageRemainder.substring(breakIndex).trimStart()
            y += lineHeight
        }

        if (lines.isEmpty()) return null
        remainingText = pageRemainder
        return TextPageData(layout, lines)
    }

    fun rasterizeNextPage(): TextPageData? = measureNextPage()?.also { it.rasterize() }

    private fun breakText(text: String, paint: Paint, maxWidth: Float): Int {
        val measured = paint.breakText(text, true, maxWidth, null)
        if (measured >= text.length) return text.length
        val lastWhitespace = text.substring(0, measured).indexOfLast { it.isWhitespace() }
        return if (lastWhitespace > 0) lastWhitespace + 1 else measured
    }
}

class TextPageData internal constructor(
    private val layout: TextLayout,
    private val lines: List<TextLineData>,
) : TxMessage {
    private val sprites = mutableListOf<TxSprite>()

    val lineTexts: List<String> get() = lines.map { it.text }
    val rasterizedSprites: List<TxSprite> get() = sprites
    val isRasterized: Boolean get() = sprites.isNotEmpty()

    fun rasterize() {
        if (isRasterized) return
        val paint = textPaint(layout)
        for (line in lines) {
            val textWidth = max(1, paint.measureText(line.text).toInt() + 1)
            val bitmap = Bitmap.createBitmap(textWidth, line.lineHeight, Bitmap.Config.ARGB_8888)
            val canvas = Canvas(bitmap)
            canvas.drawColor(Color.BLACK)
            val x = when (layout.textAlign) {
                Paint.Align.CENTER -> textWidth / 2f
                Paint.Align.RIGHT -> textWidth.toFloat()
                else -> 0f
            }
            canvas.drawText(line.text, x, -paint.fontMetrics.ascent, paint)
            sprites += bitmap.toMonochromeSprite()
            line.width = textWidth
        }
    }

    override fun pack(): ByteArray {
        require(isRasterized) { "Page must be rasterized before packing" }
        val header = ByteArray(6 + lines.size * 4)
        header[0] = 0xFF.toByte()
        header[1] = (layout.width shr 8).toByte()
        header[2] = (layout.width and 0xFF).toByte()
        header[3] = (layout.height shr 8).toByte()
        header[4] = (layout.height and 0xFF).toByte()
        header[5] = sprites.size.toByte()
        lines.forEachIndexed { index, line ->
            val offset = 6 + index * 4
            header[offset] = (line.xOffset shr 8).toByte()
            header[offset + 1] = (line.xOffset and 0xFF).toByte()
            header[offset + 2] = (line.yOffset shr 8).toByte()
            header[offset + 3] = (line.yOffset and 0xFF).toByte()
        }
        return header
    }
}

internal data class TextLineData(
    val text: String,
    var width: Int,
    val xOffset: Int,
    val yOffset: Int,
    val lineHeight: Int,
)

private fun textPaint(layout: TextLayout): Paint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
    color = Color.WHITE
    textSize = layout.fontSizePx.toFloat()
    typeface = layout.typeface
    textAlign = layout.textAlign
}

private fun Bitmap.toMonochromeSprite(): TxSprite {
    val indices = IntArray(width * height)
    for (y in 0 until height) {
        for (x in 0 until width) {
            val pixel = getPixel(x, y)
            val luminance = (Color.red(pixel) + Color.green(pixel) + Color.blue(pixel)) / 3
            indices[y * width + x] = if (luminance >= 128) 1 else 0
        }
    }
    return TxSprite(
        width = width,
        height = height,
        bpp = 1,
        numColors = 2,
        palette = byteArrayOf(0, 0, 0, -1, -1, -1),
        pixels = TxSprite.packBits(indices, 1),
    )
}
