package com.fghbuild.caic

import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithText
import androidx.test.ext.junit.runners.AndroidJUnit4
import dagger.hilt.android.testing.HiltAndroidRule
import dagger.hilt.android.testing.HiltAndroidTest
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class NavigationTest {

    @get:Rule(order = 0)
    val hiltRule = HiltAndroidRule(this)

    @get:Rule(order = 1)
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    @Test
    fun appStarts_andShowsTaskList() {
        // Wait for the app to load — multiple nodes may contain "caic".
        val nodes = composeTestRule.onAllNodesWithText("caic")
        assert(nodes.fetchSemanticsNodes().isNotEmpty()) { "Expected at least one node with text 'caic'" }
    }

    @Test
    fun appStarts_andChecksUnknownKey() {
        composeTestRule.onNodeWithText("Unknown key:", substring = true).assertDoesNotExist()
    }
}
