// Display formatting utilities for tasks: tokens, cost, elapsed time, and tool detail.
package com.fghbuild.caic.util

import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive
import java.util.Locale

fun formatTokens(n: Int): String = when {
    n >= 1_000_000 -> "${n / 1_000_000}Mt"
    n >= 1_000 -> "${n / 1_000}kt"
    else -> "${n}t"
}

fun formatCost(usd: Double): String = when {
    usd < 0.01 -> "<$0.01"
    else -> "$${String.format(Locale.US, "%.2f", usd)}"
}

fun formatElapsed(seconds: Double): String {
    val s = seconds.toLong()
    return when {
        s >= 3600 -> if ((s % 3600) / 60 == 0L) "${s / 3600}h" else "${s / 3600}h ${(s % 3600) / 60}m"
        s >= 60 -> if (s % 60 == 0L) "${s / 60}m" else "${s / 60}m ${s % 60}s"
        else -> "${s}s"
    }
}

fun formatDuration(seconds: Double): String = when {
    seconds < 1 -> "${(seconds * 1000).toInt()}ms"
    else -> "${String.format(Locale.US, "%.1f", seconds)}s"
}

/** Extracts a brief detail string for a tool call. Truncation is handled by the
 * composable (maxLines=1 + TextOverflow.Ellipsis). */
fun toolCallDetail(name: String, input: JsonElement): String? {
    val obj = input as? JsonObject ?: return null
    return when (name.lowercase()) {
        "read", "write", "edit", "notebookedit" -> obj.stringField("file_path")?.substringAfterLast('/')
        "bash" -> obj.stringField("command")?.trimStart()
        "grep", "glob" -> obj.stringField("pattern")
        "task" -> obj.stringField("description")
        "webfetch" -> obj.stringField("url")
        "websearch" -> obj.stringField("query")
        else -> null
    }
}

private fun JsonObject.stringField(key: String): String? {
    val el = get(key) ?: return null
    return if (el is JsonPrimitive && el.isString) el.jsonPrimitive.content else null
}

/** Returns the currency symbol for a currency code. Unknown codes return "??". */
fun currencySign(currency: String): String = when (currency) {
    "CNY" -> "¥"
    "USD" -> "$"
    else -> "??"
}

/** Formats a monetary balance value. */
fun formatBalance(currency: String, total: Double): String =
    String.format(Locale.getDefault(), "%s%.2f", currencySign(currency), total)
