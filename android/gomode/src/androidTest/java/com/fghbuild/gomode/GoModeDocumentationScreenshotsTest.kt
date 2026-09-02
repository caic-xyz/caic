// Generates documentation screenshots for Go Mode native shell and hosted caic task surfaces.
package com.fghbuild.gomode

import android.os.Environment
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.UiDevice
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File

@RunWith(AndroidJUnit4::class)
class GoModeDocumentationScreenshotsTest : GoModeE2eTestBase() {
    private val device: UiDevice by lazy {
        UiDevice.getInstance(InstrumentationRegistry.getInstrumentation())
    }

    private val screenshotDir: File by lazy {
        val dir = File(
            Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_PICTURES),
            "gomode-screenshots",
        )
        dir.mkdirs()
        dir
    }

    @Test
    fun generateDocumentationScreenshotsForNativeShellAndHostedTasks() {
        assumeTrue(
            "documentation screenshots run only through the explicit visual workflow",
            InstrumentationRegistry.getArguments().getString("caicVisualScreenshots") == "true",
        )
        screenshotDir.listFiles()?.forEach { it.delete() }

        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }
        takeScreenshot("gomode-settings")

        openWebShell()
        waitForHostedCaicFrontend()
        waitForText("No tasks yet.", GOMODE_LOAD_TIMEOUT_MS)
        prepareHostedVisuals()
        takeHostedScreenshot("gomode-web-shell", waitForTaskMetadata = false)

        composeRule.onNodeWithTag("gomode-web-open-settings").performClick()
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }
        takeScreenshot("gomode-settings-from-web")

        openWebShell()
        waitForHostedCaicFrontend()
        prepareHostedVisuals()
        generateHostedTaskScreenshots()

        for (name in SCREENSHOT_NAMES) {
            val file = File(screenshotDir, "$name.png")
            check(file.exists()) { "Screenshot $name.png was not created" }
            check(file.length() > 0L) { "Screenshot $name.png is empty" }
        }
    }

    private fun generateHostedTaskScreenshots() {
        val detailPrompt = "Fix token expiry bug in auth middleware"
        submitPromptThroughHostedUi(detailPrompt)
        waitForText("Fixed the token validation bug", GOMODE_LOAD_TIMEOUT_MS)
        waitForAttentionText("$detailPrompt needs attention")
        takeHostedScreenshot("gomode-task-detail")
        navigateHostedHome()

        val planPrompt = "Plan the rate limiting implementation for API endpoints"
        submitPromptThroughHostedUi(planPrompt)
        waitForTestId("clear-and-execute-plan", GOMODE_LOAD_TIMEOUT_MS)
        waitForTestId("plan-content")
        waitForAttentionText("2 items need attention")
        takeHostedScreenshot("gomode-task-plan")
        navigateHostedHome()

        val askPrompt = "Which storage backend should we use for session data?"
        submitPromptThroughHostedUi(askPrompt)
        waitForText("Which approach should I use?", GOMODE_LOAD_TIMEOUT_MS)
        waitForTestId("ask-option-In-memory (sync.Map)")
        waitForAttentionText("3 items need attention")
        takeHostedScreenshot("gomode-task-ask")
        tapDomElement(TASK_DETAIL_PROMPT_SELECTOR)
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) { isImeVisible() }
        takeHostedScreenshot("gomode-task-detail-prompt-focused")
        device.pressBack()
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) { !isImeVisible() }
        navigateHostedHome()

        val listPrompt = "Update CI pipeline to run tests in parallel"
        submitPromptThroughHostedUi(listPrompt)
        navigateHostedHome()

        for (prompt in listOf(detailPrompt, planPrompt, askPrompt, listPrompt)) {
            waitForDom("task card for screenshot prompt '$prompt'", GOMODE_LOAD_TIMEOUT_MS) {
                "taskCard(${prompt.jsString()}) !== null"
            }
            waitForDom("task metadata for screenshot prompt '$prompt'", GOMODE_LOAD_TIMEOUT_MS) {
                "taskCard(${prompt.jsString()})?.textContent?.includes('fake-model') === true"
            }
            waitForDom("successful CI for screenshot prompt '$prompt'", GOMODE_LOAD_TIMEOUT_MS) {
                "taskCard(${prompt.jsString()})?.querySelector('[data-testid=\"ci-status\"]')?.dataset.status === 'success'"
            }
        }
        takeHostedScreenshot("gomode-task-list", waitForTaskMetadata = false)
    }

    private fun waitForAttentionText(text: String) {
        composeRule.waitUntil(GOMODE_LOAD_TIMEOUT_MS) {
            composeRule.onAllNodesWithText(text).fetchSemanticsNodes().isNotEmpty()
        }
    }

    private fun takeHostedScreenshot(name: String, waitForTaskMetadata: Boolean = true) {
        if (waitForTaskMetadata) {
            waitForText("PR #1", GOMODE_LOAD_TIMEOUT_MS)
            waitForText("CI: passed", GOMODE_LOAD_TIMEOUT_MS)
        }
        dismissHostedToasts()
        stabilizeHostedVisuals()
        takeScreenshot(name)
    }

    private fun prepareHostedVisuals() {
        executeDom("install deterministic hosted visual state") {
            """
            (() => {
              Date.now = () => 1788350400000;
              let style = document.getElementById("caic-visual-test-style");
              if (!style) {
                style = document.createElement("style");
                style.id = "caic-visual-test-style";
                style.textContent = `
                  *, *::before, *::after {
                    animation: none !important;
                    caret-color: transparent !important;
                    transition: none !important;
                  }
                  [data-testid="timing-duration"] > span {
                    display: none !important;
                  }
                  [data-testid="timing-duration"]::after {
                    content: "150ms";
                    font-size: 0.72rem;
                    font-variant-numeric: tabular-nums;
                    opacity: 0.72;
                    white-space: nowrap;
                  }
                  [data-testid="task-setup"] [data-testid="timing-duration"]::after {
                    content: "42ms";
                  }
                `;
                document.head.append(style);
              }
              return true;
            })()
            """.trimIndent()
        }
        stabilizeHostedVisuals()
    }

    private fun stabilizeHostedVisuals() {
        waitForDom("hosted fonts") { "document.fonts.status === 'loaded'" }
        executeDom("schedule hosted layout stabilization") {
            """
            (() => {
              document.documentElement.dataset.caicVisualReady = "false";
              requestAnimationFrame(() => requestAnimationFrame(() => {
                document.documentElement.dataset.caicVisualReady = "true";
              }));
              return true;
            })()
            """.trimIndent()
        }
        waitForDom("hosted layout stabilization") {
            "document.documentElement.dataset.caicVisualReady === 'true'"
        }
    }

    private fun dismissHostedToasts() {
        executeDom("dismiss hosted toasts") {
            """
            (() => {
              for (const button of document.querySelectorAll("button")) {
                if (button.textContent?.trim() === "×") button.click();
              }
              return true;
            })()
            """.trimIndent()
        }
    }

    private fun takeScreenshot(name: String) {
        composeRule.waitForIdle()
        device.waitForIdle()
        check(device.takeScreenshot(File(screenshotDir, "$name.png"))) { "Could not save screenshot $name" }
    }

    companion object {
        private const val TASK_DETAIL_PROMPT_SELECTOR = "[data-testid=\"task-detail-form\"] [role=\"textbox\"]"
        private val SCREENSHOT_NAMES = listOf(
            "gomode-settings",
            "gomode-web-shell",
            "gomode-settings-from-web",
            "gomode-task-list",
            "gomode-task-detail",
            "gomode-task-detail-prompt-focused",
            "gomode-task-plan",
            "gomode-task-ask",
        )
    }
}
