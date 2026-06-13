// Generates documentation screenshots for Go Mode native shell and hosted caic task surfaces.
package com.fghbuild.gomode

import android.os.Environment
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.UiDevice
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
        screenshotDir.listFiles()?.forEach { it.delete() }

        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }
        takeScreenshot("gomode-settings")

        openWebShell()
        waitForHostedFrontend()
        takeScreenshot("gomode-web-shell")

        composeRule.onNodeWithTag("gomode-web-open-settings").performClick()
        composeRule.waitUntil(GOMODE_DEFAULT_TIMEOUT_MS) {
            composeRule.onAllNodesWithTag("gomode-settings").fetchSemanticsNodes().isNotEmpty()
        }
        takeScreenshot("gomode-settings-from-web")

        openWebShell()
        waitForHostedCaicFrontend()
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
        takeScreenshot("gomode-task-detail")
        navigateHostedHome()

        val planPrompt = "Plan the rate limiting implementation for API endpoints"
        submitPromptThroughHostedUi(planPrompt)
        waitForTestId("clear-and-execute-plan", GOMODE_LOAD_TIMEOUT_MS)
        waitForTestId("plan-content")
        takeScreenshot("gomode-task-plan")
        navigateHostedHome()

        val askPrompt = "Which storage backend should we use for session data?"
        submitPromptThroughHostedUi(askPrompt)
        waitForText("Which approach should I use?", GOMODE_LOAD_TIMEOUT_MS)
        waitForTestId("ask-option-In-memory (sync.Map)")
        takeScreenshot("gomode-task-ask")
        navigateHostedHome()

        val listPrompt = "Update CI pipeline to run tests in parallel"
        submitPromptThroughHostedUi(listPrompt)
        navigateHostedHome()

        for (prompt in listOf(detailPrompt, planPrompt, askPrompt, listPrompt)) {
            waitForDom("task card for screenshot prompt '$prompt'", GOMODE_LOAD_TIMEOUT_MS) {
                "taskCard(${prompt.jsString()}) !== null"
            }
        }
        takeScreenshot("gomode-task-list")
    }

    private fun takeScreenshot(name: String) {
        composeRule.waitForIdle()
        Thread.sleep(SETTLE_DELAY_MS)
        check(device.takeScreenshot(File(screenshotDir, "$name.png"))) { "Could not save screenshot $name" }
    }

    companion object {
        private const val SETTLE_DELAY_MS = 1_000L
        private val SCREENSHOT_NAMES = listOf(
            "gomode-settings",
            "gomode-web-shell",
            "gomode-settings-from-web",
            "gomode-task-list",
            "gomode-task-detail",
            "gomode-task-plan",
            "gomode-task-ask",
        )
    }
}
