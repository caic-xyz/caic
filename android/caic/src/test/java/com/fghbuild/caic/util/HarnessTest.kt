// Unit tests for harness conversion and effort option utilities.
package com.fghbuild.caic.util

import com.caic.sdk.v1.Harness
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class HarnessTest {
    @Test
    fun `toHarness converts known values`() {
        assertEquals(Harness.Claude, "claude".toHarness())
        assertEquals(Harness.Codex, "codex".toHarness())
        assertEquals(Harness.Kilo, "kilo".toHarness())
        assertEquals(Harness.OpenCode, "opencode".toHarness())
        assertEquals(Harness.Pi, "pi".toHarness())
    }

    @Test
    fun `toHarness uses Other for unknown values`() {
        val h = "custom-agent".toHarness()
        assertTrue(h is Harness.Other)
        assertEquals("custom-agent", (h as Harness.Other).value)
    }

    @Test
    fun `toHarness uses Other for empty string`() {
        val h = "".toHarness()
        assertTrue(h is Harness.Other)
        assertEquals("", (h as Harness.Other).value)
    }

    @Test
    fun `effortOptions returns levels for Claude`() {
        val opts = effortOptions(Harness.Claude)
        assertEquals(listOf("low", "medium", "high", "max"), opts)
    }

    @Test
    fun `effortOptions returns levels for Codex`() {
        val opts = effortOptions(Harness.Codex)
        assertEquals(listOf("none", "minimal", "low", "medium", "high", "xhigh"), opts)
    }

    @Test
    fun `effortOptions returns levels for Pi`() {
        val opts = effortOptions(Harness.Pi)
        assertEquals(listOf("off", "minimal", "low", "medium", "high", "xhigh"), opts)
    }

    @Test
    fun `effortOptions returns empty for Kilo`() {
        assertTrue(effortOptions(Harness.Kilo).isEmpty())
    }

    @Test
    fun `effortOptions returns empty for OpenCode`() {
        assertTrue(effortOptions(Harness.OpenCode).isEmpty())
    }

    @Test
    fun `effortOptions returns empty for Other`() {
        assertTrue(effortOptions(Harness.Other("custom")).isEmpty())
    }
}
