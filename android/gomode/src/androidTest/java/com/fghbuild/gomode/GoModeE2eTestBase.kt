// Shared instrumented helpers for Go Mode hosted WebView e2e coverage.
package com.fghbuild.gomode

import android.webkit.WebView
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextReplacement
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.UiDevice
import org.junit.Rule
import org.junit.rules.TestRule
import org.junit.runners.model.Statement
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.math.roundToInt

internal const val GOMODE_DEFAULT_TIMEOUT_MS = 30_000L
internal const val GOMODE_LOAD_TIMEOUT_MS = 60_000L

@Suppress("UnnecessaryAbstractClass")
abstract class GoModeE2eTestBase {
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

    protected val baseUrl: String by lazy {
        InstrumentationRegistry.getArguments().getString("baseUrl", DEFAULT_BASE_URL)
    }

    protected fun openWebShell() {
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            hasNodeWithTag("gomode-service-url") || hasNodeWithTag("gomode-web-shell")
        }
        if (hasNodeWithTag("gomode-service-url")) {
            composeRule.onNodeWithTag("gomode-service-url").performTextReplacement(baseUrl)
            composeRule.onNodeWithTag("gomode-save-service").performClick()
        }
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            hasNodeWithTag("gomode-web-shell")
        }
    }

    protected fun waitForHostedFrontend() {
        waitForWebView()
        waitForDom("frontend origin") { "location.origin === ${baseUrl.jsString()}" }
        waitForDom("Go Mode host query") { "new URL(location.href).searchParams.get('goModeHost') === '1'" }
        waitForDom("Go Mode host bridge") { "typeof window.goModeHost?.shellVersion === 'function'" }
        waitForDom("frontend document ready") { "document.readyState === 'complete'" }
        waitForDom("frontend body has content") { "document.body?.innerText.trim().length > 0" }
    }

    protected fun waitForHostedCaicFrontend() {
        waitForHostedFrontend()
        waitForDom("repository chips loaded", GOMODE_LOAD_TIMEOUT_MS) {
            "!!document.querySelector('[data-testid=\"repo-chips\"] [data-testid^=\"chip-label-\"]')"
        }
    }

    protected fun submitPromptThroughHostedUi(prompt: String) {
        navigateHostedHome()
        waitForDom("new task prompt is visible") { "isVisible('[data-testid=\"prompt-input\"]')" }
        fillContentEditableByTestId("prompt-input", prompt)
        clickByTestId("submit-task")
        waitForTestId("task-detail-form", GOMODE_LOAD_TIMEOUT_MS)
        waitForText(prompt, GOMODE_LOAD_TIMEOUT_MS)
    }

    protected fun openTaskCard(prompt: String) {
        navigateHostedHome()
        waitForDom("task card for '$prompt'", GOMODE_LOAD_TIMEOUT_MS) {
            "taskCard(${prompt.jsString()}) !== null"
        }
        executeDom("open task card '$prompt'") {
            "(() => { const card = taskCard(${prompt.jsString()}); card?.click(); return card !== null; })()"
        }
    }

    protected fun navigateHostedHome() {
        executeDom("navigate hosted frontend home") {
            """
            (() => {
              if (location.pathname !== "/") {
                history.pushState(null, "", "/");
                dispatchEvent(new PopStateEvent("popstate"));
              }
              return true;
            })()
            """.trimIndent()
        }
    }

    protected fun fillTaskDetailInput(text: String) {
        fillContentEditable("[data-testid=\"task-detail-form\"] [role=\"textbox\"]", text)
    }

    protected fun fillContentEditableByTestId(testId: String, text: String) {
        fillContentEditable(testIdSelector(testId), text)
    }

    protected fun clickByTestId(testId: String) {
        val selector = testIdSelector(testId)
        waitForDom("clickable $testId") { "isEnabled(${selector.jsString()})" }
        executeDom("click $testId") {
            """
            (() => {
              const el = document.querySelector(${selector.jsString()});
              el?.click();
              return el !== null;
            })()
            """.trimIndent()
        }
    }

    protected fun waitForTestId(testId: String, timeoutMs: Long = GOMODE_DEFAULT_TIMEOUT_MS) {
        val selector = testIdSelector(testId)
        waitForDom("test id $testId", timeoutMs) { "isVisible(${selector.jsString()})" }
    }

    protected fun waitForText(text: String, timeoutMs: Long = GOMODE_DEFAULT_TIMEOUT_MS) {
        waitForDom("text '$text'", timeoutMs) {
            "document.body?.innerText.includes(${text.jsString()}) === true"
        }
    }

    protected fun textOccurrenceCount(text: String): Int = js(
        "((document.body?.innerText ?? '').split(${text.jsString()}).length - 1)"
    ).toInt()

    protected fun waitForTextOccurrenceAtLeast(
        text: String,
        minCount: Int,
        timeoutMs: Long = GOMODE_DEFAULT_TIMEOUT_MS,
    ) {
        waitForDom("text '$text' occurrence count >= $minCount", timeoutMs) {
            "((document.body?.innerText ?? '').split(${text.jsString()}).length - 1) >= $minCount"
        }
    }

    protected fun waitForDom(
        description: String,
        timeoutMs: Long = GOMODE_DEFAULT_TIMEOUT_MS,
        script: () -> String,
    ) {
        val condition = script()
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(timeoutMs)
        while (System.nanoTime() < deadline) {
            if (js(wrapDomHelpers(condition)) == "true") return
            Thread.sleep(POLL_INTERVAL_MS)
        }
        val bodyText = js("document.body?.innerText.slice(0, 1000) ?? ''")
        error("Timed out waiting for DOM condition '$description': $condition\nBody: $bodyText")
    }

    protected fun executeDom(description: String, script: () -> String) {
        val result = js(wrapDomHelpers(script()))
        check(result == "true") { "DOM action failed '$description': $result" }
    }

    protected fun waitForWebView(timeoutMs: Long = GOMODE_DEFAULT_TIMEOUT_MS): WebView {
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(timeoutMs)
        while (System.nanoTime() < deadline) {
            webViewOrNull()?.let { return it }
            Thread.sleep(POLL_INTERVAL_MS)
        }
        error("WebView was not created")
    }

    protected fun pressActivityBack() {
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            composeRule.activity.onBackPressedDispatcher.onBackPressed()
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "Back dispatch timed out" }
    }

    protected fun tapDomElement(selector: String) {
        val view = waitForWebView()
        val widthScale = view.width / js("window.innerWidth").toFloat()
        val heightScale = view.height / js("window.innerHeight").toFloat()
        val xCss = js(
            """
            (() => {
              const r = document.querySelector(${selector.jsString()}).getBoundingClientRect();
              return r.left + r.width / 2;
            })()
            """.trimIndent(),
        ).toFloat()
        val yCss = js(
            """
            (() => {
              const r = document.querySelector(${selector.jsString()}).getBoundingClientRect();
              return r.top + r.height / 2;
            })()
            """.trimIndent(),
        ).toFloat()
        val location = webViewScreenLocation()
        UiDevice.getInstance(InstrumentationRegistry.getInstrumentation()).click(
            location[0] + (xCss * widthScale).roundToInt(),
            location[1] + (yCss * heightScale).roundToInt(),
        )
    }

    protected fun webViewScreenLocation(): IntArray {
        val view = waitForWebView()
        val location = IntArray(2)
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            view.getLocationOnScreen(location)
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "WebView bounds lookup timed out" }
        return location
    }

    protected fun isImeVisible(): Boolean {
        var visible = false
        val latch = CountDownLatch(1)
        composeRule.activity.runOnUiThread {
            val root = composeRule.activity.window.decorView.rootView
            visible = root.rootWindowInsets
                ?.getInsets(android.view.WindowInsets.Type.ime())
                ?.bottom
                ?.let { it > 0 }
                ?: false
            latch.countDown()
        }
        check(latch.await(JS_TIMEOUT_MS, TimeUnit.MILLISECONDS)) { "IME inset lookup timed out" }
        return visible
    }

    protected fun js(script: String): String {
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

    private fun hasNodeWithTag(tag: String): Boolean =
        composeRule.onAllNodesWithTag(tag).fetchSemanticsNodes().isNotEmpty()

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

    private fun fillContentEditable(selector: String, text: String) {
        waitForDom("editable $selector") { "isVisible(${selector.jsString()})" }
        executeDom("fill $selector") {
            """
            (() => {
              const el = document.querySelector(${selector.jsString()});
              if (!el) return false;
              el.focus();
              el.textContent = "";
              el.dispatchEvent(new InputEvent(
                "input",
                { bubbles: true, inputType: "deleteContentBackward" },
              ));
              el.textContent = ${text.jsString()};
              el.dispatchEvent(new InputEvent(
                "input",
                { bubbles: true, inputType: "insertText", data: ${text.jsString()} },
              ));
              return true;
            })()
            """.trimIndent()
        }
    }

    private fun testIdSelector(testId: String): String = "[data-testid=\"${testId.cssString()}\"]"

    private fun wrapDomHelpers(script: String): String =
        """
        (() => {
          const isVisible = (selector) => {
            const el = document.querySelector(selector);
            if (!el) return false;
            const rect = el.getBoundingClientRect();
            const style = getComputedStyle(el);
            return rect.width > 0 && rect.height > 0 &&
              style.visibility !== "hidden" && style.display !== "none";
          };
          const isEnabled = (selector) => {
            const el = document.querySelector(selector);
            return !!el && !el.disabled;
          };
          const taskCard = (prompt) => Array.from(document.querySelectorAll("[data-task-id]"))
            .find((el) => el.textContent?.includes(prompt)) ?? null;
          return Boolean($script);
        })()
        """.trimIndent()

    protected fun String.jsString(): String =
        "\"" + replace("\\", "\\\\").replace("\"", "\\\"").replace("\n", "\\n") + "\""

    private fun String.cssString(): String = replace("\\", "\\\\").replace("\"", "\\\"")

    companion object {
        private const val DEFAULT_BASE_URL = "http://localhost:8090"
        private const val JS_TIMEOUT_MS = 5_000L
        private const val POLL_INTERVAL_MS = 250L
    }
}
