// Usage badges: per-provider grouped pills with color-coded thresholds.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.foundation.Image
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Text
import androidx.compose.material3.TooltipAnchorPosition
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.painter.BitmapPainter
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ProviderQuota
import com.caic.sdk.v1.QuotaBalance
import com.caic.sdk.v1.QuotaExtraUsage
import com.caic.sdk.v1.QuotaRateLimit
import com.caic.sdk.v1.UsageResp
import com.caverock.androidsvg.SVG
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.util.currencySign
import com.fghbuild.caic.util.formatBalance
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

private data class BadgeColors(val bg: Color, val fg: Color)

@Composable
private fun pctColor(pct: Double): BadgeColors {
    val c = MaterialTheme.appColors
    return when {
        pct >= 90.0 -> BadgeColors(c.dangerBg, c.dangerText)
        pct >= 80.0 -> BadgeColors(c.warningBg, c.warningText)
        else -> BadgeColors(c.successBg, c.successText)
    }
}

@Composable
fun UsageBadges(usage: UsageResp, serverURL: String = "", modifier: Modifier = Modifier) {
    FlowRow(
        modifier = modifier,
        horizontalArrangement = Arrangement.spacedBy(4.dp),
        verticalArrangement = Arrangement.spacedBy(2.dp),
    ) {
        ProviderPills(usage, serverURL)
    }
}

@Composable
fun ProviderPills(usage: UsageResp, serverURL: String) {
    usage.providers.forEach { pq -> ProviderPill(pq, serverURL) }
}

@Composable
private fun SvgUrlImage(url: String, contentDescription: String?, modifier: Modifier = Modifier) {
    var bitmap by remember { mutableStateOf<android.graphics.Bitmap?>(null) }
    val density = LocalDensity.current
    val sizePx = with(density) { 12.dp.toPx().toInt() }
    LaunchedEffect(url) {
        try {
            val svgText = withContext(Dispatchers.IO) { java.net.URL(url).readText() }
            val svg = SVG.getFromString(svgText)
            val bm: android.graphics.Bitmap = android.graphics.Bitmap.createBitmap(
                sizePx, sizePx, android.graphics.Bitmap.Config.ARGB_8888,
            )
            val canvas = android.graphics.Canvas(bm)
            svg.renderToCanvas(canvas, android.graphics.RectF(0f, 0f, sizePx.toFloat(), sizePx.toFloat()))
            bitmap = bm
        } catch (_: Exception) {
        }
    }
    bitmap?.let {
        Image(
            painter = BitmapPainter(it.asImageBitmap()),
            contentDescription = contentDescription,
            modifier = modifier,
            contentScale = ContentScale.Fit,
        )
    }
}

@Composable
private fun ProviderPill(pq: ProviderQuota, serverURL: String) {
    val pillBg = MaterialTheme.colorScheme.surfaceVariant
    val pillBorder = Color(0xFFDDDDDD)
    Row(
        modifier = Modifier
            .background(pillBg, RoundedCornerShape(4.dp))
            .border(0.5.dp, pillBorder, RoundedCornerShape(4.dp))
            .padding(horizontal = 5.dp, vertical = 2.dp),
        horizontalArrangement = Arrangement.spacedBy(3.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (pq.logoUrl.isNotBlank() && serverURL.isNotBlank()) {
            SvgUrlImage(
                url = serverURL.trimEnd('/') + "/" + pq.logoUrl.trimStart('/'),
                contentDescription = pq.label,
                modifier = Modifier.size(12.dp),
            )
        }
        pq.rateLimits?.forEach { rl -> RateLimitBadge(pq.label, rl) }
        pq.balance?.let { BalanceBadge(pq.label, it) }
        pq.extraUsage?.let { ExtraUsageBadge(pq.label, it) }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun RateLimitBadge(label: String, rl: QuotaRateLimit) {
    val pct = rl.usedPct.coerceIn(0.0, 100.0)
    val colors = pctColor(pct)
    val tip = rememberTooltipState()
    TooltipBox(
        positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
        tooltip = { PlainTooltip { Text("$label ${rl.window}: ${pct.toInt()}%") } },
        state = tip,
    ) {
        BadgeText(text = "${rl.window} ${pct.toInt()}%", colors = colors)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun BalanceBadge(label: String, bal: QuotaBalance) {
    val total = formatBalance(bal.currency, bal.total)
    val colors = if (bal.total <= 0.0) {
        val c = MaterialTheme.appColors
        BadgeColors(c.dangerBg, c.dangerText)
    } else {
        val appColors = MaterialTheme.appColors
        BadgeColors(appColors.successBg, appColors.successText)
    }
    val tip = rememberTooltipState()
    TooltipBox(
        positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
        tooltip = { PlainTooltip { Text("$label: $total") } },
        state = tip,
    ) {
        BadgeText(text = total, colors = colors)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ExtraUsageBadge(label: String, extra: QuotaExtraUsage) {
    val sign = currencySign(extra.currency)
    val used = extra.usedCredits
    val limit = extra.monthlyLimit
    if (used == 0.0 && limit == 0.0) return
    val colors = if (!extra.isEnabled) {
        BadgeColors(MaterialTheme.appColors.badgeDisabledBg, MaterialTheme.colorScheme.secondary)
    } else {
        pctColor(extra.usedPct)
    }
    val tip = rememberTooltipState()
    TooltipBox(
        positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
        tooltip = { PlainTooltip { Text("$label: extra %s%d/%s%d".format(sign, used.toInt(), sign, limit.toInt())) } },
        state = tip,
    ) {
        BadgeText(
            text = "extra %s%d/%s%d".format(sign, used.toInt(), sign, limit.toInt()),
            colors = colors,
            strikethrough = !extra.isEnabled,
        )
    }
}

@Composable
private fun BadgeText(
    text: String,
    colors: BadgeColors,
    strikethrough: Boolean = false,
) {
    val style = if (strikethrough) {
        MaterialTheme.typography.labelSmall.merge(
            TextStyle(textDecoration = TextDecoration.LineThrough)
        )
    } else {
        MaterialTheme.typography.labelSmall
    }
    Text(
        text = text,
        style = style,
        color = colors.fg,
        fontWeight = FontWeight.Medium,
        modifier = Modifier
            .background(colors.bg, RoundedCornerShape(3.dp))
            .padding(horizontal = 4.dp, vertical = 1.dp),
    )
}
