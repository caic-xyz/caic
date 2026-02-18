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

fun formatElapsed(ms: Long): String {
    val seconds = ms / 1000
    return when {
        seconds >= 3600 -> "${seconds / 3600}h ${(seconds % 3600) / 60}m"
        seconds >= 60 -> "${seconds / 60}m ${seconds % 60}s"
        else -> "${seconds}s"
    }
}

fun formatDuration(ms: Long): String = when {
    ms < 1000 -> "${ms}ms"
    else -> "${String.format(Locale.US, "%.1f", ms / 1000.0)}s"
}

private const val MAX_BASH_DETAIL = 60
private const val BASH_TRUNCATE_AT = 57

/** Extracts a brief detail string for a tool call, matching the web frontend. */
fun toolCallDetail(name: String, input: JsonElement): String? {
    val obj = input as? JsonObject ?: return null
    return when (name.lowercase()) {
        "read", "write", "edit", "notebookedit" -> obj.stringField("file_path")?.substringAfterLast('/')
        "bash" -> {
            val cmd = obj.stringField("command")?.trimStart() ?: return null
            if (cmd.length > MAX_BASH_DETAIL) cmd.take(BASH_TRUNCATE_AT) + "..." else cmd
        }
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
