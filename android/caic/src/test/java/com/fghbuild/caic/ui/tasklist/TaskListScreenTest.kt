// Unit tests for TaskListScreen presentation helpers.
package com.fghbuild.caic.ui.tasklist

import org.junit.Assert.assertEquals
import org.junit.Test

class TaskListScreenTest {

    @Test
    fun titleUsesDisplayNameWithoutAppSuffix() {
        assertEquals("test-host", taskListTitle("test-host", "http://example.com"))
    }

    @Test
    fun titleFallsBackToURLHostWithoutAppSuffix() {
        assertEquals("example.com", taskListTitle("", "http://example.com:2242"))
    }

    @Test
    fun titleFallsBackToAppNameWhenServerIsMissing() {
        assertEquals("caic", taskListTitle(null, ""))
    }
}
