// Unit tests for JSON argument parsing helpers used by FunctionHandlers.
package com.fghbuild.caic.voice

import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class FunctionHandlerHelpersTest {

    @Test
    fun `requireString extracts string value`() {
        val obj = JsonObject(mapOf("key" to JsonPrimitive("value")))
        assertEquals("value", obj.requireString("key"))
    }

    @Test(expected = IllegalArgumentException::class)
    fun `requireString throws on missing key`() {
        val obj = JsonObject(emptyMap())
        obj.requireString("missing")
    }

    @Test(expected = IllegalArgumentException::class)
    fun `requireString throws on json array`() {
        val obj = JsonObject(mapOf("key" to JsonArray(emptyList())))
        obj.requireString("key")
    }

    @Test
    fun `requireString extracts integer as string`() {
        val obj = JsonObject(mapOf("count" to JsonPrimitive(7)))
        assertEquals("7", obj.requireString("count"))
    }

    @Test
    fun `requireInt extracts integer value`() {
        val obj = JsonObject(mapOf("count" to JsonPrimitive(7)))
        assertEquals(7, obj.requireInt("count"))
    }

    @Test(expected = IllegalArgumentException::class)
    fun `requireInt throws on missing key`() {
        val obj = JsonObject(emptyMap())
        obj.requireInt("missing")
    }

    @Test(expected = IllegalArgumentException::class)
    fun `requireInt throws on string value`() {
        val obj = JsonObject(mapOf("count" to JsonPrimitive("not_an_int")))
        obj.requireInt("count")
    }

    @Test
    fun `optString returns string when present`() {
        val obj = JsonObject(mapOf("key" to JsonPrimitive("hello")))
        assertEquals("hello", obj.optString("key"))
    }

    @Test
    fun `optString returns null when key missing`() {
        val obj = JsonObject(emptyMap())
        assertNull(obj.optString("missing"))
    }

    @Test
    fun `textResult wraps message in result object`() {
        val result = textResult("done")
        val obj = result as JsonObject
        assertEquals("done", obj["result"]!!.jsonPrimitive.content)
        assertEquals(1, obj.size)
    }

    @Test
    fun `errorResult wraps message in error object`() {
        val result = errorResult("something went wrong")
        val obj = result as JsonObject
        assertEquals("something went wrong", obj["error"]!!.jsonPrimitive.content)
        assertEquals(1, obj.size)
    }

    @Test
    fun `textResult handles empty string`() {
        val result = textResult("")
        val obj = result as JsonObject
        assertEquals("", obj["result"]!!.jsonPrimitive.content)
    }

    @Test
    fun `errorResult handles multiline message`() {
        val result = errorResult("line1\nline2")
        val obj = result as JsonObject
        assertEquals("line1\nline2", obj["error"]!!.jsonPrimitive.content)
    }

    @Test
    fun `error message includes function name`() {
        val result = errorResult("Unknown function: bogus")
        assertTrue((result as JsonObject)["error"]!!.jsonPrimitive.content.contains("Unknown function"))
    }
}
