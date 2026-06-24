// Instrumented smoke coverage for the Go Mode WebView shell and native service monitoring.
package com.fghbuild.gomode

import android.app.Instrumentation.ActivityResult
import android.content.Intent
import android.content.IntentFilter
import androidx.compose.ui.semantics.SemanticsProperties
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

@RunWith(AndroidJUnit4::class)
class WebShellSmokeTest : GoModeE2eTestBase() {
    @Test
    fun webShellLoadsHostedFrontendAndHandlesSpaBack() {
        openWebShell()
        waitForHostedFrontend()
        waitForDom("frontend fills viewport") {
            "document.querySelector('#app > div')?.getBoundingClientRect().height > window.innerHeight * 0.5"
        }

        executeDom("push SPA route") {
            """
            (() => {
              window.history.pushState(null, "", "$SPA_BACK_TEST_PATH");
              window.dispatchEvent(new PopStateEvent("popstate"));
              return true;
            })()
            """.trimIndent()
        }
        waitForDom("pushed SPA route") { "location.pathname === '$SPA_BACK_TEST_PATH'" }

        pressActivityBack()
        waitForDom("SPA back returned home") { "location.pathname === '/'" }
    }

    @Test
    fun webShellSettingsButtonOpensNativeSettingsAndBackReturnsToWebShell() {
        openWebShell()

        composeRule.onNodeWithTag("gomode-web-open-settings").performClick()
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }

        pressActivityBack()

        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-web-shell").fetchSemanticsNodes().isNotEmpty()
        }
    }

    @Test
    fun nativeServiceAttentionFollowsMcpResourceUpdates() {
        openWebShell()
        waitForHostedCaicFrontend()

        val initialAttentionText = serviceAttentionText()
        submitAttentionUpdateTask(promptSuffix = "alpha")
        submitAttentionUpdateTask(promptSuffix = "beta")

        composeRule.waitUntil(GOMODE_LOAD_TIMEOUT_MS) {
            serviceAttentionShowsCountChangedFrom(initialAttentionText)
        }
        assertTrue(serviceAttentionShowsCountChangedFrom(initialAttentionText))
    }

    @Test
    fun hostedFrontendPlanAskAndMultiTurnFlowsWorkThroughWebViewDom() {
        openWebShell()
        waitForHostedCaicFrontend()

        val planPrompt = "FAKE_PLAN gomode e2e ${System.currentTimeMillis()}"
        submitPromptThroughHostedUi(planPrompt)
        waitForTestId("clear-and-execute-plan", GOMODE_LOAD_TIMEOUT_MS)
        waitForTestId("plan-content")
        fillTaskDetailInput("execute now")
        clickByTestId("clear-and-execute-plan")
        waitForText("Why do programmers prefer dark mode?", GOMODE_LOAD_TIMEOUT_MS)

        val askPrompt = "FAKE_ASK gomode e2e ${System.currentTimeMillis()}"
        submitPromptThroughHostedUi(askPrompt)
        waitForText("Which approach should I use?", GOMODE_LOAD_TIMEOUT_MS)
        clickByTestId("ask-option-In-memory (sync.Map)")
        val askAnswerCount = textOccurrenceCount(SQL_JOKE_PREFIX)
        clickByTestId("ask-submit")
        waitForTextOccurrenceAtLeast(SQL_JOKE_PREFIX, askAnswerCount + 1, GOMODE_LOAD_TIMEOUT_MS)

        val multiTurnPrompt = "multi-turn gomode e2e ${System.currentTimeMillis()}"
        submitPromptThroughHostedUi(multiTurnPrompt)
        waitForText("Why do programmers prefer dark mode?", GOMODE_LOAD_TIMEOUT_MS)
        fillTaskDetailInput("tell me another")
        val multiTurnAnswerCount = textOccurrenceCount(SQL_JOKE_PREFIX)
        clickByTestId("send-input")
        waitForTextOccurrenceAtLeast(SQL_JOKE_PREFIX, multiTurnAnswerCount + 1, GOMODE_LOAD_TIMEOUT_MS)
    }

    @Test
    fun targetBlankLinksOpenInDefaultBrowser() {
        openWebShell()
        loadHostedExternalLinkTestPage()
        waitForDom("external link loaded") { "document.getElementById('external-link') !== null" }

        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val filter = IntentFilter(Intent.ACTION_VIEW).apply {
            addCategory(Intent.CATEGORY_BROWSABLE)
            addDataScheme("https")
        }
        val monitor = instrumentation.addMonitor(filter, ActivityResult(0, null), true)
        try {
            tapDomElement("#external-link")
            composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) { monitor.hits > 0 }
            assertEquals(1, monitor.hits)
        } finally {
            instrumentation.removeMonitor(monitor)
        }
    }

    @Test
    fun gomodePackageNameIsAppSpecific() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        assertEquals("com.fghbuild.gomode", context.packageName)
    }

    private fun submitAttentionUpdateTask(promptSuffix: String) {
        val prompt = "FAKE_ATTENTION_UPDATE gomode e2e $promptSuffix"
        submitPromptThroughHostedUi(prompt)
        waitForText(ATTENTION_RUNNING_TEXT, GOMODE_LOAD_TIMEOUT_MS)
        waitForText(ATTENTION_RESULT_TEXT, GOMODE_LOAD_TIMEOUT_MS)
    }

    private fun serviceAttentionShowsCountChangedFrom(initialText: String?): Boolean {
        val text = serviceAttentionText()
        return !text.isNullOrBlank() && text != initialText && ATTENTION_COUNT_NUMBER_PATTERN.containsMatchIn(text)
    }

    private fun serviceAttentionText(): String? = composeRule.onAllNodesWithTag("gomode-service-attention")
        .fetchSemanticsNodes()
        .firstOrNull()
        ?.config
        ?.get(SemanticsProperties.Text)
        ?.joinToString(separator = "") { it.text }

    private fun loadHostedExternalLinkTestPage() {
        loadHostedHtml(EXTERNAL_LINK_TEST_PAGE)
    }

    private fun loadHostedHtml(html: String) {
        val latch = CountDownLatch(1)
        val view = waitForWebView()
        composeRule.activity.runOnUiThread {
            view.loadDataWithBaseURL(baseUrl, html, "text/html", "UTF-8", null)
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "Test page load timed out" }
    }

    companion object {
        private const val SPA_BACK_TEST_PATH = "/gomode-e2e-route"
        private const val SQL_JOKE_PREFIX = "A SQL query walks into a bar"
        private const val ATTENTION_RUNNING_TEXT = "Monitoring update is running before attention is required."
        private const val ATTENTION_RESULT_TEXT = "Monitoring update requires attention now"
        private const val JS_TIMEOUT_MS = 5_000L
        private val ATTENTION_COUNT_NUMBER_PATTERN = Regex("""\d+""")
        private const val EXTERNAL_LINK_URL = "https://example.com/gomode-external-link"
        private val EXTERNAL_LINK_TEST_PAGE = """
            <!doctype html>
            <html>
              <body>
                <a id="external-link" href="$EXTERNAL_LINK_URL" target="_blank" rel="noopener">External docs</a>
              </body>
            </html>
        """.trimIndent()
    }
}
