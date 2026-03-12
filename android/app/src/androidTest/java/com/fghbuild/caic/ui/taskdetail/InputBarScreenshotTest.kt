// Compose UI tests for the InputBar screenshot attach flow.
package com.fghbuild.caic.ui.taskdetail

import android.graphics.Bitmap
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.caic.sdk.v1.ImageData
import com.fghbuild.caic.ui.theme.CaicTheme
import com.fghbuild.caic.util.bitmapToImageData
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class InputBarScreenshotTest {

    @get:Rule
    val composeTestRule = createComposeRule()

    /** Produces a minimal valid ImageData by encoding a tiny solid-colour bitmap. */
    private fun makeTestImage(): ImageData {
        val bmp = Bitmap.createBitmap(10, 10, Bitmap.Config.ARGB_8888)
        val img = bitmapToImageData(bmp)
        bmp.recycle()
        return img
    }

    private fun setContent(
        supportsImages: Boolean = true,
        pendingImages: List<ImageData> = emptyList(),
        onScreenshot: () -> Unit = {},
        onRemoveImage: (Int) -> Unit = {},
    ) {
        composeTestRule.setContent {
            CaicTheme {
                InputBar(
                    draft = "",
                    onDraftChange = {},
                    onSend = {},
                    onSync = {},
                    onStop = {},
                    onPurge = {},
                    onRevive = {},
                    sending = false,
                    pendingAction = null,
                    supportsImages = supportsImages,
                    pendingImages = pendingImages,
                    onAttachGallery = {},
                    onAttachCamera = {},
                    onScreenshot = onScreenshot,
                    onRemoveImage = onRemoveImage,
                )
            }
        }
    }

    @Test
    fun attachButton_visible_whenSupportsImages() {
        setContent(supportsImages = true)
        composeTestRule.onNodeWithContentDescription("Attach image").assertIsDisplayed()
    }

    @Test
    fun attachButton_absent_whenSupportsImagesFalse() {
        setContent(supportsImages = false)
        composeTestRule.onNodeWithContentDescription("Attach image").assertDoesNotExist()
    }

    @Test
    fun attachMenu_containsScreenshotOption() {
        setContent()
        composeTestRule.onNodeWithContentDescription("Attach image").performClick()
        composeTestRule.onNodeWithText("Screenshot").assertIsDisplayed()
    }

    @Test
    fun screenshotItem_click_invokesOnScreenshot() {
        var called = false
        setContent(onScreenshot = { called = true })
        composeTestRule.onNodeWithContentDescription("Attach image").performClick()
        composeTestRule.onNodeWithText("Screenshot").performClick()
        assertTrue("onScreenshot must be invoked when Screenshot is tapped", called)
    }

    @Test
    fun pendingImage_thumbnailVisible() {
        setContent(pendingImages = listOf(makeTestImage()))
        composeTestRule.onNodeWithContentDescription("Attached image").assertIsDisplayed()
    }

    @Test
    fun pendingImage_removeButton_invokesOnRemoveImage() {
        var removedIndex = -1
        setContent(
            pendingImages = listOf(makeTestImage()),
            onRemoveImage = { removedIndex = it },
        )
        composeTestRule.onNodeWithContentDescription("Remove").performClick()
        assertEquals(0, removedIndex)
    }
}
