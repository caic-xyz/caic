// Shell-side service resource adapters map MCP resources into neutral Go Mode monitoring state.
package com.fghbuild.gomode.service

import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

data class ServiceMonitoringPlan(
    val resourceURI: String,
    val notificationResourceURI: String? = null,
    val resourceSubscriptions: List<String> = listOfNotNull(resourceURI, notificationResourceURI),
    val resourcesListChanged: Boolean = true,
) {
    val resourceURIs: List<String>
        get() = listOfNotNull(resourceURI, notificationResourceURI)
}

interface ServiceResourceAdapter {
    val service: String
    val apiVersion: Int

    fun monitoringPlan(resources: List<ResourceDescriptor>): ServiceMonitoringPlan?

    fun snapshot(readResults: Map<String, ResourcesReadResult>): ServiceMonitoringSnapshot
}

data class ServiceMonitoringSnapshot(
    val service: String,
    val resourceURI: String,
    val tasks: List<ServiceTaskSummary>,
) {
    val attentionTasks: List<ServiceTaskSummary>
        get() = tasks.filter { it.needsAttention }

    val attentionCount: Int
        get() = attentionTasks.size

    val notificationText: String?
        get() = when (attentionCount) {
            0 -> null
            1 -> "${attentionTasks.single().title} needs attention"
            else -> "$attentionCount tasks need attention"
        }

    val voiceContext: String
        get() {
            if (tasks.isEmpty()) return "No visible service tasks."
            return tasks.joinToString(
                separator = "\n",
                prefix = "Visible service tasks:\n",
            ) { task ->
                val attention = if (task.needsAttention) " needs attention" else ""
                "- ${task.title}: ${task.state}$attention"
            }
        }
}

data class ServiceTaskSummary(
    val id: String,
    val title: String,
    val state: String,
    val needsAttention: Boolean,
)

fun serviceResourceAdapterFor(service: String, apiVersion: Int): ServiceResourceAdapter? =
    serviceResourceAdapters.firstOrNull { it.service == service && it.apiVersion == apiVersion }

private val serviceResourceAdapters = listOf(CaicTasksResourceAdapter)

private object CaicTasksResourceAdapter : ServiceResourceAdapter {
    override val service = "caic"
    override val apiVersion = 1

    override fun monitoringPlan(resources: List<ResourceDescriptor>): ServiceMonitoringPlan? {
        val resource = resources.firstOrNull { it.uri == CaicTasksResourceURI } ?: return null
        if (!isJSONResource(resource)) return null
        val notificationResource = resources.firstOrNull { it.uri == GoModeNotificationsResourceURI }
        if (notificationResource != null && !isJSONResource(notificationResource)) return null
        return ServiceMonitoringPlan(
            resourceURI = CaicTasksResourceURI,
            notificationResourceURI = notificationResource?.uri,
        )
    }

    override fun snapshot(readResults: Map<String, ResourcesReadResult>): ServiceMonitoringSnapshot {
        val text = resourceText(readResults, CaicTasksResourceURI)
        return ServiceMonitoringSnapshot(
            service = service,
            resourceURI = CaicTasksResourceURI,
            tasks = parseCaicTasks(text),
        )
    }
}

private fun isJSONResource(resource: ResourceDescriptor): Boolean =
    resource.mimeType == null || resource.mimeType == JsonMimeType

private fun resourceText(readResults: Map<String, ResourcesReadResult>, uri: String): String {
    val readResult = readResults[uri] ?: throw IllegalArgumentException("resource read result is missing $uri")
    val content = readResult.contents.firstOrNull { it.uri == uri }
        ?: throw IllegalArgumentException("resource read result is missing $uri")
    val mimeType = content.mimeType
    require(mimeType == null || mimeType == JsonMimeType) {
        "resource $uri must be JSON, got ${mimeType.orEmpty()}"
    }
    return content.text ?: throw IllegalArgumentException("resource $uri is missing text content")
}

private fun parseCaicTasks(text: String): List<ServiceTaskSummary> {
    val root = Json.parseToJsonElement(text)
    require(root is JsonArray) { "caic tasks resource must be a JSON array" }
    return root.mapIndexed { index, element ->
        val task = element as? JsonObject
            ?: throw IllegalArgumentException("caic tasks resource item $index must be an object")
        val id = task.requiredString("id", index)
        val title = task.optionalString("title")?.ifBlank { id } ?: id
        val state = task.requiredString("state", index)
        ServiceTaskSummary(
            id = id,
            title = title,
            state = state,
            needsAttention = CaicTaskState.fromWire(state)?.needsAttention == true || !task.optionalString("error").isNullOrBlank(),
        )
    }
}

private fun JsonObject.requiredString(field: String, index: Int): String =
    optionalString(field)?.takeIf { it.isNotBlank() }
        ?: throw IllegalArgumentException("caic tasks resource item $index is missing $field")

private fun JsonObject.optionalString(field: String): String? = this[field]?.jsonPrimitive?.contentOrNull

private enum class CaicTaskState(val wireName: String, val needsAttention: Boolean) {
    Pending("pending", false),
    Branching("branching", false),
    Provisioning("provisioning", false),
    Starting("starting", false),
    Running("running", false),
    Waiting("waiting", true),
    Asking("asking", true),
    HasPlan("has_plan", true),
    Pulling("pulling", false),
    Pushing("pushing", false),
    Stopping("stopping", false),
    Stopped("stopped", false),
    Purging("purging", false),
    Crashed("crashed", true),
    Failed("failed", true),
    Purged("purged", false),
    ;

    companion object {
        fun fromWire(value: String): CaicTaskState? = entries.firstOrNull { it.wireName == value }
    }
}

internal const val GoModeNotificationsResourceURI = "gomode://notifications"
private const val CaicTasksResourceURI = "caic://tasks"
private const val JsonMimeType = "application/json"
