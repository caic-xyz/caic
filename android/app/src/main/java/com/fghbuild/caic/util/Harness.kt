// Shared Harness utilities: string-to-type conversion and effort level mapping.
package com.fghbuild.caic.util

import com.caic.sdk.v1.Harness

/** Converts a user-facing harness string (e.g. from a dropdown) to the typed [Harness]. */
fun String.toHarness(): Harness = when (this) {
    "claude" -> Harness.Claude
    "codex" -> Harness.Codex
    "gemini" -> Harness.Gemini
    "kilo" -> Harness.Kilo
    "opencode" -> Harness.OpenCode
    "pi" -> Harness.Pi
    else -> Harness.Other(this)
}

/** Returns valid effort levels for [harness], empty if unsupported. */
fun effortOptions(harness: Harness): List<String> = when (harness) {
    is Harness.Claude -> listOf("low", "medium", "high", "max")
    is Harness.Codex -> listOf("none", "minimal", "low", "medium", "high", "xhigh")
    is Harness.Pi -> listOf("off", "minimal", "low", "medium", "high", "xhigh")
    is Harness.Gemini, is Harness.Kilo, is Harness.OpenCode, is Harness.Other -> emptyList()
}
