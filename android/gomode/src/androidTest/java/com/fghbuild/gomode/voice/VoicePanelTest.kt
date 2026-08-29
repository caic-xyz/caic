// Compose UI tests for the compact and expandable Go Mode voice panel transcript.
package com.fghbuild.gomode.voice

import androidx.compose.material3.MaterialTheme
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performClick
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test

class VoicePanelTest {
    @get:Rule
    val composeRule = createComposeRule()

    @Test
    fun transcriptIsCollapsedByDefaultAndCanBeExpanded() {
        composeRule.setContent {
            MaterialTheme {
                VoicePanel(
                    voiceState = VoiceState(
                        connected = true,
                        listening = true,
                        transcript = listOf(TranscriptEntry(TranscriptSpeaker.ASSISTANT, "Four tasks are running.")),
                    ),
                    voiceEnabled = true,
                    onConnect = {},
                    onDisconnect = {},
                    onToggleMute = {},
                    onSelectDevice = {},
                    onClearTranscript = {},
                    onOpenSettings = {},
                    serviceAttentionText = null,
                )
            }
        }

        assertTrue(composeRule.onAllNodesWithTag("gomode-voice-transcript").fetchSemanticsNodes().isEmpty())
        composeRule.onNodeWithTag("gomode-voice-transcript-toggle").performClick()
        composeRule.onNodeWithTag("gomode-voice-transcript").assertIsDisplayed()
    }
}
