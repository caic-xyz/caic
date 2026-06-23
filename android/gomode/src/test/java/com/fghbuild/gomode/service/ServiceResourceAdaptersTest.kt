// Unit tests for Go Mode shell-side service resource adapters.
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

class ServiceResourceAdaptersTest {
    @Test
    fun `adapter is selected by service and api version`() {
        val adapter = serviceResourceAdapterFor(service = "caic", apiVersion = 1)

        assertEquals("caic", adapter?.service)
        assertNull(serviceResourceAdapterFor(service = "caic", apiVersion = 2))
        assertNull(serviceResourceAdapterFor(service = "other", apiVersion = 1))
    }

    @Test
    fun `unsupported resources disable monitoring cleanly`() {
        val adapter = serviceResourceAdapterFor(service = "caic", apiVersion = 1)
        val resources = listOf(ResourceDescriptor(uri = "caic://usage", name = "usage", mimeType = "application/json"))
        val nonJSONTasks = listOf(ResourceDescriptor(uri = "caic://tasks", name = "tasks", mimeType = "text/plain"))

        assertNull(adapter?.monitoringPlan(resources))
        assertNull(adapter?.monitoringPlan(nonJSONTasks))
    }

    @Test
    fun `caic adapter maps fake tasks without caic dto imports`() {
        val adapter = serviceResourceAdapterFor(service = "caic", apiVersion = 1)
        val resources = listOf(ResourceDescriptor(uri = "caic://tasks", name = "tasks", mimeType = "application/json"))
        val plan = adapter?.monitoringPlan(resources)

        val snapshot = adapter?.snapshot(caicTasksReadResult(FAKE_TASKS_JSON))

        assertEquals("caic://tasks", plan?.resourceURI)
        assertEquals(listOf("caic://tasks"), plan?.resourceSubscriptions)
        assertEquals(true, plan?.resourcesListChanged)
        assertEquals(listOf("t1", "t2", "t3"), snapshot?.tasks?.map { it.id })
        assertEquals(listOf("Review plan", "Fix tests"), snapshot?.attentionTasks?.map { it.title })
        assertEquals(2, snapshot?.attentionCount)
        assertEquals("2 tasks need attention", snapshot?.notificationText)
        assertTrue(snapshot?.voiceContext?.contains("Review plan: asking needs attention") == true)
        assertTrue(snapshot?.voiceContext?.contains("Build feature: running") == true)
    }

    private fun caicTasksReadResult(text: String) = ResourcesReadResult(
        resultType = ResultType.Complete,
        contents = listOf(
            ResourceContent(
                uri = "caic://tasks",
                mimeType = "application/json",
                text = text,
            )
        ),
        ttlMs = 1000,
        cacheScope = CacheScope.Private,
    )

    private companion object {
        const val FAKE_TASKS_JSON = """
            [
              {"id": "t1", "title": "Build feature", "state": "running"},
              {"id": "t2", "title": "Review plan", "state": "asking"},
              {"id": "t3", "title": "Fix tests", "state": "failed", "error": "lint failed"}
            ]
        """
    }
}
