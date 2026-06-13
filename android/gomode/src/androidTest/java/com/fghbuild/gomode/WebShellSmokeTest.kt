// Instrumented smoke coverage for the Go Mode hosted WebView shell.
package com.fghbuild.gomode

import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.UiDevice
import org.junit.Assert.assertEquals
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
              window.history.pushState(null, "", "$BACK_TEST_PATH");
              window.dispatchEvent(new PopStateEvent("popstate"));
              return true;
            })()
            """.trimIndent()
        }
        waitForDom("pushed SPA route") { "location.pathname === '$BACK_TEST_PATH'" }

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
    fun webShellResizesAboveKeyboardForHostedTextInput() {
        openWebShell()
        loadHostedTestPage()
        waitForDom("test prompt loaded") { "document.getElementById('prompt') !== null" }

        try {
            tapDomElement("#prompt")

            composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
                val imeTop = imeTopOrNull() ?: return@waitUntil false
                webViewScreenBottom() <= imeTop + SCREEN_EDGE_TOLERANCE_PX
            }
        } finally {
            UiDevice.getInstance(InstrumentationRegistry.getInstrumentation()).pressBack()
        }
    }

    @Test
    fun gomodePackageNameIsAppSpecific() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        assertEquals("com.fghbuild.gomode", context.packageName)
    }

    private fun loadHostedTestPage() {
        val latch = CountDownLatch(1)
        val view = waitForWebView()
        composeRule.activity.runOnUiThread {
            view.loadDataWithBaseURL(baseUrl, IME_TEST_PAGE, "text/html", "UTF-8", null)
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "Test page load timed out" }
    }

    private fun imeTopOrNull(): Int? {
        var top: Int? = null
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            val root = composeRule.activity.window.decorView.rootView
            val imeBottom = root.rootWindowInsets
                ?.getInsets(android.view.WindowInsets.Type.ime())
                ?.bottom
                ?: 0
            if (imeBottom > 0) {
                top = root.height - imeBottom
            }
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "IME inset lookup timed out" }
        return top
    }

    private fun webViewScreenBottom(): Int {
        val view = waitForWebView()
        val location = webViewScreenLocation()
        return location[1] + view.height
    }

    companion object {
        private const val BACK_TEST_PATH = "/gomode-e2e-route"
        private const val SQL_JOKE_PREFIX = "A SQL query walks into a bar"
        private const val SCREEN_EDGE_TOLERANCE_PX = 8
        private const val JS_TIMEOUT_MS = 5_000L
        private val IME_TEST_PAGE = """
            <!doctype html>
            <html>
              <head>
                <meta name="viewport" content="width=device-width, initial-scale=1" />
                <style>
                  html, body { margin: 0; height: 100%; }
                  body { display: flex; flex-direction: column; font-family: sans-serif; }
                  main { flex: 1; }
                  #prompt { min-height: 44px; border: 1px solid #999; padding: 12px; }
                </style>
              </head>
              <body>
                <main>Hosted content</main>
                <div id="prompt" role="textbox" contenteditable="true">Prompt</div>
              </body>
            </html>
        """.trimIndent()
    }
}
