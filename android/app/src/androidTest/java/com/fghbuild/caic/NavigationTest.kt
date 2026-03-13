package com.fghbuild.caic

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithText
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class NavigationTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    @Test
    fun appStarts_andShowsTaskList() {
        // Wait for the app to load.
        composeTestRule.onNodeWithText("caic").assertIsDisplayed()
    }

    @Test
    fun appStarts_andChecksUnknownKey() {
        composeTestRule.onNodeWithText("Unknown key:", substring = true).assertDoesNotExist()
    }
}
