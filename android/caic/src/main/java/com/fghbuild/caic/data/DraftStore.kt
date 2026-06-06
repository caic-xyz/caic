// In-memory store for per-task input drafts (text + images) that survive task switching.
package com.fghbuild.caic.data

import com.caic.sdk.v1.ImageData
import javax.inject.Inject
import javax.inject.Singleton

data class InputDraft(
    val text: String = "",
    val images: List<ImageData> = emptyList(),
)

@Singleton
class DraftStore @Inject constructor() {
    private val drafts = mutableMapOf<String, InputDraft>()

    fun get(taskId: String): InputDraft = drafts[taskId] ?: InputDraft()

    fun setText(taskId: String, text: String) {
        drafts[taskId] = get(taskId).copy(text = text)
    }

    fun setImages(taskId: String, images: List<ImageData>) {
        drafts[taskId] = get(taskId).copy(images = images)
    }

    fun clear(taskId: String) {
        drafts.remove(taskId)
    }
}
