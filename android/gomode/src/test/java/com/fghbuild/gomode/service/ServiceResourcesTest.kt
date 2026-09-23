// Unit tests for generic Go Mode service monitoring resources.
package com.fghbuild.gomode.service

import com.fghbuild.mcp.sdk.v1.CacheScope
import com.fghbuild.mcp.sdk.v1.ResourceContent
import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import com.fghbuild.mcp.sdk.v1.ResultType
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ServiceResourcesTest {
    @Test
    fun `missing items resource disables monitoring`() {
        val resources =
            listOf(ResourceDescriptor(uri = "service://other", name = "other", mimeType = "application/json"))

        assertNull(serviceMonitoringPlan(resources))
    }

    @Test
    fun `non JSON items resource disables monitoring`() {
        val resources =
            listOf(ResourceDescriptor(uri = GOMODE_ITEMS_RESOURCE_URI, name = "items", mimeType = "text/plain"))

        assertNull(serviceMonitoringPlan(resources))
    }

    @Test
    fun `generic items produce attention and voice context`() {
        val resources =
            listOf(
                ResourceDescriptor(uri = GOMODE_ITEMS_RESOURCE_URI, name = "items", mimeType = "application/json"),
                ResourceDescriptor(
                    uri = GOMODE_NOTIFICATIONS_RESOURCE_URI,
                    name = "notifications",
                    mimeType = "application/json",
                ),
            )
        val plan = serviceMonitoringPlan(resources)
        val snapshot =
            serviceMonitoringSnapshot(
                mapOf(GOMODE_ITEMS_RESOURCE_URI to itemsReadResult(ITEMS_JSON)),
                requireNotNull(plan),
            )

        assertEquals(GOMODE_ITEMS_RESOURCE_URI, plan.itemsResourceURI)
        assertEquals(listOf(GOMODE_ITEMS_RESOURCE_URI, GOMODE_NOTIFICATIONS_RESOURCE_URI), plan.resourceSubscriptions)
        assertEquals(listOf("i1", "i2", "i3"), snapshot.items.map { it.id })
        assertEquals(4, snapshot.omittedItemCount)
        assertEquals(listOf("Review plan", "Fix tests"), snapshot.attentionItems.map { it.title })
        assertEquals(2, snapshot.attentionCount)
        assertEquals("2 items need attention", snapshot.notificationText)
        assertTrue(snapshot.voiceContext.contains("Task #2: Review plan (awaiting input, needs attention)"))
        assertTrue(snapshot.voiceContext.contains("Task #1: Build feature (active)"))
        assertTrue(snapshot.voiceContext.contains("4 more items omitted. Call tasks_list and follow nextCursor."))
    }

    @Test
    fun `voice updates report changed items without replaying unchanged baseline`() {
        val baseline = ServiceMonitoringSnapshot(listOf(ServiceItemSummary("1", "Item #1", "Build", "active", false)))
        assertNull(serviceItemVoiceChanges(baseline, baseline))

        val changed =
            ServiceMonitoringSnapshot(
                listOf(
                    ServiceItemSummary("1", "Item #1", "Build", "waiting", true),
                    ServiceItemSummary("2", "Item #2", "Review", "active", false),
                ),
            )
        assertEquals(
            "Service item updates:\n- Item #1: Build (waiting), needs attention\n- Item #2: Review (active)",
            serviceItemVoiceChanges(baseline, changed),
        )
        assertNull(serviceItemVoiceChanges(changed, changed))
    }

    private fun itemsReadResult(text: String) =
        ResourcesReadResult(
            resultType = ResultType.Complete,
            contents =
                listOf(
                    ResourceContent(
                        uri = GOMODE_ITEMS_RESOURCE_URI,
                        mimeType = "application/json",
                        text = text,
                    ),
                ),
            ttlMs = 1000,
            cacheScope = CacheScope.Private,
        )

    private companion object {
        const val ITEMS_JSON = """
            {"items": [
              {"id": "i1", "reference": "Task #1", "title": "Build feature", "state": "active", "needsAttention": false},
              {"id": "i2", "reference": "Task #2", "title": "Review plan", "state": "awaiting input", "needsAttention": true},
              {"id": "i3", "reference": "Task #3", "title": "Fix tests", "state": "failed", "needsAttention": true}
            ], "moreItemsHint": "Call tasks_list and follow nextCursor.", "omittedCount": 4}
        """
    }
}
