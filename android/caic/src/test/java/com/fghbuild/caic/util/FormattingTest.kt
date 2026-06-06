// Unit tests for formatting utilities.
package com.fghbuild.caic.util

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import java.util.Locale

class FormattingTest {
    private val t = object {
        fun run(name: String, block: () -> Unit) {
            try {
                block()
            } catch (e: AssertionError) {
                throw AssertionError("Subtest '$name' failed: ${e.message}", e)
            }
        }
    }

    private fun withDefaultLocale(locale: Locale, block: () -> Unit) {
        val previousLocale = Locale.getDefault()
        try {
            Locale.setDefault(locale)
            block()
        } finally {
            Locale.setDefault(previousLocale)
        }
    }

    @Test
    fun testToolCallDetail() {
        t.run("Read extracts filename") {
            val input = JsonObject(mapOf("file_path" to JsonPrimitive("/home/user/foo/bar.kt")))
            assertEquals("bar.kt", toolCallDetail("Read", input))
        }

        t.run("Bash returns full command (truncation is handled by composable)") {
            val longCmd = "a".repeat(70)
            val input = JsonObject(mapOf("command" to JsonPrimitive(longCmd)))
            assertEquals(longCmd, toolCallDetail("Bash", input))
        }

        t.run("Bash keeps short commands") {
            val input = JsonObject(mapOf("command" to JsonPrimitive("ls -la")))
            assertEquals("ls -la", toolCallDetail("Bash", input))
        }

        t.run("Grep extracts pattern") {
            val input = JsonObject(mapOf("pattern" to JsonPrimitive("foo.*bar")))
            assertEquals("foo.*bar", toolCallDetail("Grep", input))
        }

        t.run("WebSearch extracts query") {
            val input = JsonObject(mapOf("query" to JsonPrimitive("kotlin coroutines")))
            assertEquals("kotlin coroutines", toolCallDetail("WebSearch", input))
        }

        t.run("unknown tool returns null") {
            val input = JsonObject(mapOf("x" to JsonPrimitive("y")))
            assertNull(toolCallDetail("Unknown", input))
        }
    }

    @Test
    fun testFormatTokens() {
        t.run("millions") { assertEquals("1Mt", formatTokens(1_000_000)) }
        t.run("thousands") { assertEquals("5kt", formatTokens(5_000)) }
        t.run("small") { assertEquals("42t", formatTokens(42)) }
    }

    @Test
    fun testFormatDuration() {
        t.run("milliseconds") { assertEquals("500ms", formatDuration(0.5)) }
        t.run("seconds") { assertEquals("1.5s", formatDuration(1.5)) }
    }

    @Test
    fun testFormatCost() {
        t.run("sub-cent shows <$0.01") { assertEquals("<$0.01", formatCost(0.005)) }
        t.run("exactly one cent") { assertEquals("$0.01", formatCost(0.01)) }
        t.run("cents in range") { assertEquals("$0.50", formatCost(0.50)) }
        t.run("whole dollars") { assertEquals("$1.00", formatCost(1.0)) }
        t.run("large amounts") { assertEquals("$1500.00", formatCost(1500.0)) }
        t.run("just below one cent") { assertEquals("<$0.01", formatCost(0.009)) }
        t.run("rounded down to one cent") { assertEquals("$0.01", formatCost(0.014)) }
    }

    @Test
    fun testFormatElapsed() {
        t.run("seconds only") { assertEquals("42s", formatElapsed(42.0)) }
        t.run("exactly one minute") { assertEquals("1m", formatElapsed(60.0)) }
        t.run("minutes and seconds") { assertEquals("2m 30s", formatElapsed(150.0)) }
        t.run("exactly one hour") { assertEquals("1h", formatElapsed(3600.0)) }
        t.run("hours and minutes") { assertEquals("1h 30m", formatElapsed(5400.0)) }
        t.run("hours without fractional minutes") { assertEquals("2h", formatElapsed(7200.0)) }
        t.run("zero seconds") { assertEquals("0s", formatElapsed(0.0)) }
        t.run("large hours") { assertEquals("48h", formatElapsed(172800.0)) }
    }

    @Test
    fun testCurrencySign() {
        t.run("USD returns $") { assertEquals("$", currencySign("USD")) }
        t.run("CNY returns ¥") { assertEquals("¥", currencySign("CNY")) }
        t.run("unknown returns ??") { assertEquals("??", currencySign("EUR")) }
        t.run("empty returns ??") { assertEquals("??", currencySign("")) }
    }

    @Test
    fun testFormatBalance() {
        withDefaultLocale(Locale.US) {
            t.run("USD format") { assertEquals("$1.50", formatBalance("USD", 1.5)) }
            t.run("CNY format") { assertEquals("¥100.00", formatBalance("CNY", 100.0)) }
            t.run("unknown currency uses ??") { assertEquals("??0.00", formatBalance("EUR", 0.0)) }
            t.run("large values") { assertEquals("$1234567.89", formatBalance("USD", 1234567.89)) }
        }
    }

    @Test
    fun testFormatBalanceUsesDefaultLocale() {
        withDefaultLocale(Locale.CANADA_FRENCH) {
            assertEquals("$1,50", formatBalance("USD", 1.5))
        }
    }
}
