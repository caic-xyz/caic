// Generate documentation screenshots using the fake backend. Replaces scripts/gen-android-screenshots.sh.
//
// Run with: make android-e2e
// Output: screenshots saved to device /sdcard/Pictures/caic-screenshots/, then pulled and converted.
package com.fghbuild.caic.e2e

import android.os.Environment
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.UiDevice
import com.fghbuild.caic.MainActivity
import dagger.hilt.android.testing.HiltAndroidTest
import kotlinx.coroutines.runBlocking
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File

@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class GenScreenshotsTest : E2eTestBase() {

    @get:Rule(order = 1)
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    private val device: UiDevice by lazy {
        UiDevice.getInstance(InstrumentationRegistry.getInstrumentation())
    }

    private val screenshotDir: File by lazy {
        val dir = File(
            Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_PICTURES),
            "caic-screenshots",
        )
        dir.mkdirs()
        dir
    }

    private fun takeScreenshot(name: String) {
        // Let animations settle.
        composeTestRule.waitForIdle()
        Thread.sleep(SETTLE_DELAY_MS)
        device.takeScreenshot(File(screenshotDir, "$name.png"))
    }

    @Test
    fun generateDocumentationScreenshots() = runBlocking {
        // Clear stale screenshots from previous runs.
        screenshotDir.listFiles()?.forEach { it.delete() }

        // Verify the server is configured before waiting for the connection dot.
        // If the DataStore write from e2eConfigureRule hasn't propagated through the
        // StateFlow yet, the UI shows NotConfiguredContent instead of the connection dot.
        composeTestRule.waitUntil(CONNECTION_TIMEOUT_MS) {
            val configured = composeTestRule.onAllNodesWithTag("connection-dot")
                .fetchSemanticsNodes().isNotEmpty()
            if (!configured) {
                val notConfig = composeTestRule.onAllNodesWithTag("not-configured")
                assert(notConfig.fetchSemanticsNodes().isEmpty()) {
                    "Server not configured — DataStore write from e2eConfigureRule never propagated"
                }
            }
            configured
        }

        // Create the same 4 tasks as gen-android-screenshots.sh.
        val id1 = createTaskAPI("Fix token expiry bug in auth middleware")
        val id2 = createTaskAPI("Plan the rate limiting implementation for API endpoints")
        val id3 = createTaskAPI("Which storage backend should we use for session data?")
        val id4 = createTaskAPI("Update CI pipeline to run tests in parallel")

        // Wait for tasks to reach their target states.
        waitForTaskState(id1, "waiting", 30_000)
        waitForTaskState(id2, "has_plan", 30_000)
        waitForTaskState(id3, "asking", 30_000)
        waitForTaskState(id4, "waiting", 30_000)

        // Wait for the task list to load in the UI.
        composeTestRule.waitUntil(LOAD_TIMEOUT_MS) {
            composeTestRule.onAllNodesWithTag("task-$id1")
                .fetchSemanticsNodes().isNotEmpty()
        }

        // Screenshot 1: Task list.
        takeScreenshot("task-list")

        // Tap the first task to open detail view via testTag (immune to title changes).
        composeTestRule.onNodeWithTag("task-$id1").performClick()
        composeTestRule.waitForIdle()
        Thread.sleep(SETTLE_DELAY_MS)

        // Screenshot 2: Task detail.
        takeScreenshot("task-detail")

        // Go back to task list.
        composeTestRule.onNodeWithTag("navigate-back").performClick()
        composeTestRule.waitForIdle()

        // Tap the plan task.
        composeTestRule.onNodeWithTag("task-$id2").performClick()
        composeTestRule.waitForIdle()
        Thread.sleep(SETTLE_DELAY_MS)

        // Screenshot 3: Plan mode.
        takeScreenshot("task-plan")

        // Go back.
        composeTestRule.onNodeWithTag("navigate-back").performClick()
        composeTestRule.waitForIdle()

        // Tap the ask task.
        composeTestRule.onNodeWithTag("task-$id3").performClick()
        composeTestRule.waitForIdle()
        Thread.sleep(SETTLE_DELAY_MS)

        // Screenshot 4: Ask mode.
        takeScreenshot("task-ask")

        // Verify screenshots were saved.
        for (name in listOf("task-list", "task-detail", "task-plan", "task-ask")) {
            val file = File(screenshotDir, "$name.png")
            assert(file.exists()) { "Screenshot $name.png was not created" }
            assert(file.length() > 0) { "Screenshot $name.png is empty" }
        }
    }

    companion object {
        private const val SETTLE_DELAY_MS = 1000L
        // CI emulators render at ~1 FPS (Skipped 63-95 frames, 1.1-1.6s first frame).
        // A full compose pass can take >1s, and the connection dot requires the SSE
        // handshake + compose cycle — 30s is too tight when frames take seconds each.
        private const val CONNECTION_TIMEOUT_MS = 90_000L
        private const val LOAD_TIMEOUT_MS = 60_000L
    }
}
