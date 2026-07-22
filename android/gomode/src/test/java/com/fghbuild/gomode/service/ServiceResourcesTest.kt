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
        val resources = listOf(ResourceDescriptor(uri = "service://other", name = "other", mimeType = "application/json"))

        assertNull(serviceMonitoringPlan(resources))
    }

    @Test
    fun `non JSON items resource disables monitoring`() {
        val resources = listOf(ResourceDescriptor(uri = GoModeItemsResourceURI, name = "items", mimeType = "text/plain"))

        assertNull(serviceMonitoringPlan(resources))
    }

    @Test
    fun `generic items produce attention and voice context`() {
        val resources = listOf(
            ResourceDescriptor(uri = GoModeItemsResourceURI, name = "items", mimeType = "application/json"),
            ResourceDescriptor(uri = GoModeNotificationsResourceURI, name = "notifications", mimeType = "application/json"),
        )
        val plan = serviceMonitoringPlan(resources)
        val snapshot = serviceMonitoringSnapshot(
            mapOf(GoModeItemsResourceURI to itemsReadResult(ITEMS_JSON)),
            requireNotNull(plan),
        )

        assertEquals(GoModeItemsResourceURI, plan.itemsResourceURI)
        assertEquals(listOf(GoModeItemsResourceURI, GoModeNotificationsResourceURI), plan.resourceSubscriptions)
        assertEquals(listOf("i1", "i2", "i3"), snapshot.items.map { it.id })
        assertEquals(listOf("Review plan", "Fix tests"), snapshot.attentionItems.map { it.title })
        assertEquals(2, snapshot.attentionCount)
        assertEquals("2 items need attention", snapshot.notificationText)
        assertTrue(snapshot.voiceContext.contains("Review plan: awaiting input needs attention"))
        assertTrue(snapshot.voiceContext.contains("Build feature: active"))
    }

    private fun itemsReadResult(text: String) = ResourcesReadResult(
        resultType = ResultType.Complete,
        contents = listOf(
            ResourceContent(
                uri = GoModeItemsResourceURI,
                mimeType = "application/json",
                text = text,
            )
        ),
        ttlMs = 1000,
        cacheScope = CacheScope.Private,
    )

    private companion object {
        const val ITEMS_JSON = """
            [
              {"id": "i1", "title": "Build feature", "state": "active", "needsAttention": false},
              {"id": "i2", "title": "Review plan", "state": "awaiting input", "needsAttention": true},
              {"id": "i3", "title": "Fix tests", "state": "failed", "needsAttention": true}
            ]
        """
    }
}
