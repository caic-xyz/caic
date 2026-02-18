// Unit tests for formatting utilities.
package com.fghbuild.caic.util

import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

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

    @Test
    fun testToolCallDetail() {
        t.run("Read extracts filename") {
            val input = JsonObject(mapOf("file_path" to JsonPrimitive("/home/user/foo/bar.kt")))
            assertEquals("bar.kt", toolCallDetail("Read", input))
        }

        t.run("Bash truncates long commands") {
            val longCmd = "a".repeat(70)
            val input = JsonObject(mapOf("command" to JsonPrimitive(longCmd)))
            val detail = toolCallDetail("Bash", input)!!
            assertEquals(60, detail.length)
            assertTrue(detail.endsWith("..."))
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
        t.run("milliseconds") { assertEquals("500ms", formatDuration(500)) }
        t.run("seconds") { assertEquals("1.5s", formatDuration(1500)) }
    }

    private fun assertTrue(condition: Boolean) {
        org.junit.Assert.assertTrue(condition)
    }
}
