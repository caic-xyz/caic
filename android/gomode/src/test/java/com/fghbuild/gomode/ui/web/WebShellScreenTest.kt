// Unit tests for Go Mode WebView new-window routing helpers.
package com.fghbuild.gomode.ui.web

import android.webkit.WebView
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner

@RunWith(RobolectricTestRunner::class)
class WebShellScreenTest {
    @Test
    fun newWindowRequestUriAcceptsAnchorHitTestUrls() {
        val uri = newWindowRequestUriOrNull(
            WebView.HitTestResult.SRC_ANCHOR_TYPE,
            "https://example.com/docs",
        )

        assertEquals("https://example.com/docs", uri.toString())
    }

    @Test
    fun newWindowRequestUriAcceptsImageAnchorHitTestUrls() {
        val uri = newWindowRequestUriOrNull(
            WebView.HitTestResult.SRC_IMAGE_ANCHOR_TYPE,
            "mailto:support@example.com",
        )

        assertEquals("mailto:support@example.com", uri.toString())
    }

    @Test
    fun newWindowRequestUriIgnoresUnknownHitTestUrls() {
        val uri = newWindowRequestUriOrNull(
            WebView.HitTestResult.UNKNOWN_TYPE,
            "https://example.com/docs",
        )

        assertNull(uri)
    }

    @Test
    fun newWindowRequestUriIgnoresBlankAndRelativeUrls() {
        assertNull(newWindowRequestUriOrNull(WebView.HitTestResult.SRC_ANCHOR_TYPE, ""))
        assertNull(newWindowRequestUriOrNull(WebView.HitTestResult.SRC_ANCHOR_TYPE, "/docs"))
    }
}
