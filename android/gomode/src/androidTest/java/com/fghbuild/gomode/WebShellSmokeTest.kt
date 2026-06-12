// Instrumented smoke coverage for the Go Mode hosted WebView shell.
package com.fghbuild.gomode

import android.webkit.WebView
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextReplacement
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TestRule
import org.junit.runner.RunWith
import org.junit.runners.model.Statement
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

@RunWith(AndroidJUnit4::class)
class WebShellSmokeTest {
    @get:Rule(order = 0)
    val clearSettingsRule = TestRule { base, _ ->
        object : Statement() {
            override fun evaluate() {
                val context = InstrumentationRegistry.getInstrumentation().targetContext
                context.filesDir.resolve("datastore/gomode_settings.preferences_pb").delete()
                base.evaluate()
            }
        }
    }

    @get:Rule(order = 1)
    val composeRule = createAndroidComposeRule<MainActivity>()

    private val baseUrl: String by lazy {
        InstrumentationRegistry.getArguments().getString("baseUrl", DEFAULT_BASE_URL)
    }

    @Test
    fun webShellLoadsHostedFrontendAndHandlesSpaBack() {
        openWebShell()
        waitForWebView()

        waitForDom("location.origin === ${baseUrl.jsString()}")
        waitForDom("document.readyState === 'complete'")
        waitForDom("document.body?.innerText.trim().length > 0")
        waitForDom(
            "document.querySelector('#app > div')?.getBoundingClientRect().height > window.innerHeight * 0.5"
        )

        assertJsTrue(
            """
            window.history.pushState(null, "", "$BACK_TEST_PATH");
            window.dispatchEvent(new PopStateEvent("popstate"));
            true;
            """.trimIndent(),
        )
        waitForDom("location.pathname === '$BACK_TEST_PATH'")

        pressActivityBack()
        waitForDom("location.pathname === '/'")
    }

    @Test
    fun webShellHeaderOpensNativeSettingsAndBackReturnsToWebShell() {
        openWebShell()

        composeRule.onNodeWithTag("gomode-web-open-settings").performClick()
        composeRule.waitUntil(DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }

        pressActivityBack()

        composeRule.waitUntil(DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-web-shell").fetchSemanticsNodes().isNotEmpty()
        }
    }

    @Test
    fun gomodePackageNameIsAppSpecific() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        assertEquals("com.fghbuild.gomode", context.packageName)
    }

    private fun openWebShell() {
        composeRule.waitUntil(DEFAULT_TIMEOUT_MS) {
            hasNodeWithTag("gomode-service-url") || hasNodeWithTag("gomode-web-header")
        }
        if (hasNodeWithTag("gomode-service-url")) {
            composeRule.onNodeWithTag("gomode-service-url").performTextReplacement(baseUrl)
            composeRule.onNodeWithTag("gomode-save-service").performClick()
        }
        composeRule.waitUntil(DEFAULT_TIMEOUT_MS) {
            hasNodeWithTag("gomode-web-header")
        }
    }

    private fun hasNodeWithTag(tag: String): Boolean =
        composeRule.onAllNodesWithTag(tag).fetchSemanticsNodes().isNotEmpty()

    private fun waitForDom(script: String, timeoutMs: Long = DEFAULT_TIMEOUT_MS) {
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(timeoutMs)
        while (System.nanoTime() < deadline) {
            if (js(script) == "true") return
            Thread.sleep(POLL_INTERVAL_MS)
        }
        error("Timed out waiting for DOM condition: $script")
    }

    private fun assertJsTrue(script: String) {
        assertEquals("true", js(script))
    }

    private fun js(script: String): String {
        var result: String? = null
        val latch = CountDownLatch(1)
        val view = waitForWebView()
        composeRule.activity.runOnUiThread {
            view.evaluateJavascript(script) {
                result = it
                latch.countDown()
            }
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "JavaScript evaluation timed out" }
        return result ?: "null"
    }

    private fun waitForWebView(timeoutMs: Long = DEFAULT_TIMEOUT_MS): WebView {
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(timeoutMs)
        while (System.nanoTime() < deadline) {
            webViewOrNull()?.let { return it }
            Thread.sleep(POLL_INTERVAL_MS)
        }
        error("WebView was not created")
    }

    private fun webViewOrNull(): WebView? {
        var view: WebView? = null
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            view = composeRule.activity.findViewById(R.id.web_shell)
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "WebView lookup timed out" }
        return view
    }

    private fun pressActivityBack() {
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            composeRule.activity.onBackPressedDispatcher.onBackPressed()
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "Back dispatch timed out" }
    }

    private fun String.jsString(): String {
        return "\"" + replace("\\", "\\\\").replace("\"", "\\\"") + "\""
    }

    companion object {
        private const val DEFAULT_BASE_URL = "http://localhost:8090"
        private const val DEFAULT_TIMEOUT_MS = 30_000L
        private const val JS_TIMEOUT_MS = 5_000L
        private const val POLL_INTERVAL_MS = 250L
        private const val BACK_TEST_PATH = "/gomode-e2e-route"
    }
}
