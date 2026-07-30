// Unit tests for Go Mode WebView recovery and new-window routing helpers.
package com.fghbuild.gomode.ui.web

import android.webkit.WebView
import android.webkit.WebViewClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner

@RunWith(RobolectricTestRunner::class)
class WebShellScreenTest {
    @Test
    fun firstTimeoutIsRetriedSilently() {
        assertEquals(true, shouldAutomaticallyRetryWebLoadError(WebViewClient.ERROR_TIMEOUT, retryAttempts = 0))
        assertEquals(false, shouldAutomaticallyRetryWebLoadError(WebViewClient.ERROR_TIMEOUT, retryAttempts = 1))
    }

    @Test
    fun nonTimeoutFailuresAreNotRetriedAutomatically() {
        assertEquals(false, shouldAutomaticallyRetryWebLoadError(WebViewClient.ERROR_CONNECT, retryAttempts = 0))
    }

    @Test
    fun newNavigationCancelsPendingAutomaticRetryAndResetsItsBudget() {
        val state = AutomaticTimeoutRetryState(attempts = 1, pending = true)

        assertEquals(AutomaticTimeoutRetryState(), automaticTimeoutRetryStateOnPageStarted(state))
    }

    @Test
    fun automaticRetryNavigationPreservesItsBudget() {
        val state = AutomaticTimeoutRetryState(attempts = 1, inProgress = true)

        assertEquals(
            AutomaticTimeoutRetryState(attempts = 1),
            automaticTimeoutRetryStateOnPageStarted(state),
        )
    }

    @Test
    fun timeoutMessageExplainsRecoveryAfterAutomaticRetryFails() {
        assertEquals(
            "The service took too long to respond. Check your network connection, then retry.",
            webLoadErrorMessage(WebViewClient.ERROR_TIMEOUT, "net::ERR_TIMED_OUT"),
        )
    }

    @Test
    fun nonTimeoutMessagePreservesWebViewDescription() {
        assertEquals(
            "net::ERR_CONNECTION_REFUSED",
            webLoadErrorMessage(WebViewClient.ERROR_CONNECT, "net::ERR_CONNECTION_REFUSED"),
        )
    }

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
