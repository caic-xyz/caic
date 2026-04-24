// Compose UI tests for TaskDetailBody: initial prompt visibility across task states.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsNotDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.unit.dp
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.caic.sdk.v1.Harnesses
import com.caic.sdk.v1.Task
import com.fghbuild.caic.ui.theme.CaicTheme
import java.time.Instant
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class TaskDetailBodyTest {

    @get:Rule
    val composeTestRule = createComposeRule()

    private fun makeTask(prompt: String, state: String = "failed") = Task(
        id = "t1",
        initialPrompt = prompt,
        title = "Test task",
        container = "c1",
        state = state,
        stateUpdatedAt = Instant.EPOCH,
        costUSD = 0.0,
        duration = 0.0,
        numTurns = 0,
        cumulativeInputTokens = 0,
        cumulativeOutputTokens = 0,
        cumulativeCacheCreationInputTokens = 0,
        cumulativeCacheReadInputTokens = 0,
        activeInputTokens = 0,
        activeCacheReadTokens = 0,
        contextWindowLimit = 0,
        harness = Harnesses.Claude,
    )

    private fun setBody(state: TaskDetailState) {
        composeTestRule.setContent {
            CaicTheme {
                TaskDetailBody(
                    state = state,
                    padding = PaddingValues(0.dp),
                    onAnswer = { _ -> },
                    onClearAndExecutePlan = {},
                    onNavigateToDiff = {},
                    onLoadToolInput = null,
                )
            }
        }
    }

    @Test
    fun showsPromptWhenFailedTaskHasNoMessages() {
        // Regression: before the fix, isReady=true (set when Ready SSE event arrives
        // for a failed task) caused the prompt block to be hidden entirely.
        setBody(
            TaskDetailState(
                task = makeTask("Fix the login bug", state = "failed"),
                hasMessages = false,
                isReady = true,
            ),
        )
        composeTestRule.onNodeWithText("Fix the login bug").assertIsDisplayed()
    }

    @Test
    fun hidesSpinnerWhenReadyWithNoMessages() {
        // No spinner once the SSE history is loaded, even if no messages exist.
        setBody(
            TaskDetailState(
                task = makeTask("Do something", state = "failed"),
                hasMessages = false,
                isReady = true,
            ),
        )
        composeTestRule.onNodeWithTag("loading").assertIsNotDisplayed()
    }

    @Test
    fun showsSpinnerBeforeReadyWithPrompt() {
        // Spinner visible while SSE history is still loading.
        setBody(
            TaskDetailState(
                task = makeTask("Do something", state = "running"),
                hasMessages = false,
                isReady = false,
            ),
        )
        composeTestRule.onNodeWithText("Do something").assertIsDisplayed()
        composeTestRule.onNodeWithTag("loading").assertIsDisplayed()
    }

    @Test
    fun showsOnlySpinnerWhenNoPromptAndNotReady() {
        setBody(TaskDetailState(task = makeTask("", state = "running"), hasMessages = false, isReady = false))
        composeTestRule.onNodeWithTag("loading").assertIsDisplayed()
    }

    @Test
    fun showsNothingWhenNoPromptAndReady() {
        // No prompt, loading complete: blank state, no spinner.
        setBody(TaskDetailState(task = makeTask("", state = "failed"), hasMessages = false, isReady = true))
        composeTestRule.onNodeWithTag("loading").assertIsNotDisplayed()
    }
}
