// Unit tests for HaloDeviceType and HaloException — pure Kotlin, no Android dependencies.
package com.caic.halo.ble

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class HaloDataTypesTest {

    @Test
    fun `enum values are distinct`() {
        assertEquals(3, HaloDeviceType.entries.size)
        assertEquals(HaloDeviceType.FRAME, HaloDeviceType.valueOf("FRAME"))
        assertEquals(HaloDeviceType.HALO, HaloDeviceType.valueOf("HALO"))
        assertEquals(HaloDeviceType.UNKNOWN, HaloDeviceType.valueOf("UNKNOWN"))
    }

    @Test
    fun `HaloException stores message`() {
        val ex = HaloException("something went wrong")
        assertEquals("something went wrong", ex.message)
        assertNull(ex.cause)
    }

    @Test
    fun `HaloException stores message and cause`() {
        val cause = RuntimeException("root")
        val ex = HaloException("wrapped", cause)
        assertEquals("wrapped", ex.message)
        assertEquals(cause, ex.cause)
    }
}
