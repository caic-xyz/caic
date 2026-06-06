// Unit tests for the per-task draft store.
package com.fghbuild.caic.data

import com.caic.sdk.v1.ImageData
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class DraftStoreTest {
    @Test
    fun `get returns empty draft for unknown task`() {
        val store = DraftStore()
        val draft = store.get("unknown")
        assertEquals("", draft.text)
        assertTrue(draft.images.isEmpty())
    }

    @Test
    fun `get returns same empty draft on repeated calls for unknown task`() {
        val store = DraftStore()
        val d1 = store.get("unknown")
        val d2 = store.get("unknown")
        assertEquals(d1.text, d2.text)
        assertTrue(d1.images.isEmpty())
        assertTrue(d2.images.isEmpty())
    }

    @Test
    fun `setText stores and retrieves text`() {
        val store = DraftStore()
        store.setText("task1", "hello world")
        assertEquals("hello world", store.get("task1").text)
    }

    @Test
    fun `setText overwrites previous text`() {
        val store = DraftStore()
        store.setText("task1", "first")
        store.setText("task1", "second")
        assertEquals("second", store.get("task1").text)
    }

    @Test
    fun `setText preserves images`() {
        val store = DraftStore()
        val img1 = ImageData(mediaType = "image/png", data = "base64a")
        val img2 = ImageData(mediaType = "image/jpeg", data = "base64b")
        store.setImages("task1", listOf(img1, img2))
        store.setText("task1", "with images")
        val draft = store.get("task1")
        assertEquals("with images", draft.text)
        assertEquals(2, draft.images.size)
        assertEquals("image/png", draft.images[0].mediaType)
    }

    @Test
    fun `setImages stores and retrieves images`() {
        val store = DraftStore()
        val imgs = listOf(ImageData("image/png", "base64abc"))
        store.setImages("task1", imgs)
        assertEquals(1, store.get("task1").images.size)
        assertEquals("image/png", store.get("task1").images[0].mediaType)
        assertEquals("base64abc", store.get("task1").images[0].data)
    }

    @Test
    fun `setImages overwrites previous images`() {
        val store = DraftStore()
        store.setImages("task1", listOf(ImageData("image/png", "old")))
        store.setImages("task1", listOf(ImageData("image/jpeg", "new")))
        assertEquals(1, store.get("task1").images.size)
        assertEquals("image/jpeg", store.get("task1").images[0].mediaType)
    }

    @Test
    fun `setImages preserves text`() {
        val store = DraftStore()
        store.setText("task1", "keep me")
        store.setImages("task1", listOf(ImageData("image/png", "data")))
        assertEquals("keep me", store.get("task1").text)
    }

    @Test
    fun `clear removes draft`() {
        val store = DraftStore()
        store.setText("task1", "text")
        store.setImages("task1", listOf(ImageData("image/png", "data")))
        store.clear("task1")
        assertEquals("", store.get("task1").text)
        assertTrue(store.get("task1").images.isEmpty())
    }

    @Test
    fun `clear on unknown task is a no-op`() {
        val store = DraftStore()
        store.clear("nonexistent")
        assertEquals("", store.get("nonexistent").text)
    }

    @Test
    fun `independent drafts for different tasks`() {
        val store = DraftStore()
        store.setText("task1", "text1")
        store.setText("task2", "text2")
        assertEquals("text1", store.get("task1").text)
        assertEquals("text2", store.get("task2").text)
    }

    @Test
    fun `clear one task does not affect another`() {
        val store = DraftStore()
        store.setText("task1", "text1")
        store.setText("task2", "text2")
        store.clear("task1")
        assertEquals("", store.get("task1").text)
        assertEquals("text2", store.get("task2").text)
    }
}
