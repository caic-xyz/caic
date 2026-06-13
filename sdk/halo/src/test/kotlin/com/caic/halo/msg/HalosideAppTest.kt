// Unit tests for HalosideApp bundled Lua assets.
package com.caic.halo.msg

import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment

@RunWith(RobolectricTestRunner::class)
class HalosideAppTest {
    @Test
    fun `default Lua libraries are bundled as assets`() {
        val assets = RuntimeEnvironment.getApplication().assets
        for (lib in HalosideApp.DEFAULT_LIBS) {
            assets.open("halo/$lib").use { stream ->
                assertTrue("$lib should not be empty", stream.available() > 0)
            }
        }
    }
}
