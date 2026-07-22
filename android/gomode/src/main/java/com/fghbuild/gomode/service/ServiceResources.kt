// Service resource parsing turns generic Go Mode MCP resources into native monitoring state.
package com.fghbuild.gomode.service

import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

data class ServiceMonitoringPlan(
    val itemsResourceURI: String,
    val notificationResourceURI: String? = null,
    val resourceSubscriptions: List<String> = listOfNotNull(itemsResourceURI, notificationResourceURI),
    val resourcesListChanged: Boolean = true,
) {
    val resourceURIs: List<String>
        get() = listOfNotNull(itemsResourceURI, notificationResourceURI)
}

data class ServiceMonitoringSnapshot(
    val items: List<ServiceItemSummary>,
) {
    val attentionItems: List<ServiceItemSummary>
        get() = items.filter { it.needsAttention }

    val attentionCount: Int
        get() = attentionItems.size

    val notificationText: String?
        get() = when (attentionCount) {
            0 -> null
            1 -> "${attentionItems.single().title} needs attention"
            else -> "$attentionCount items need attention"
        }

    val voiceContext: String
        get() {
            if (items.isEmpty()) return "No visible service items."
            return items.joinToString(
                separator = "\n",
                prefix = "Visible service items:\n",
            ) { item ->
                val attention = if (item.needsAttention) " needs attention" else ""
                "- ${item.title}: ${item.state}$attention"
            }
        }
}

data class ServiceItemSummary(
    val id: String,
    val title: String,
    val state: String,
    val needsAttention: Boolean,
)

fun serviceMonitoringPlan(resources: List<ResourceDescriptor>): ServiceMonitoringPlan? {
    val itemsResource = resources.firstOrNull { it.uri == GoModeItemsResourceURI } ?: return null
    if (!isJSONResource(itemsResource)) return null
    val notificationResource = resources.firstOrNull { it.uri == GoModeNotificationsResourceURI }
    if (notificationResource != null && !isJSONResource(notificationResource)) return null
    return ServiceMonitoringPlan(
        itemsResourceURI = itemsResource.uri,
        notificationResourceURI = notificationResource?.uri,
    )
}

fun serviceMonitoringSnapshot(readResults: Map<String, ResourcesReadResult>, plan: ServiceMonitoringPlan): ServiceMonitoringSnapshot {
    val root = Json.parseToJsonElement(resourceText(readResults, plan.itemsResourceURI))
    require(root is JsonArray) { "resource ${plan.itemsResourceURI} must be a JSON array" }
    return ServiceMonitoringSnapshot(
        items = root.mapIndexed { index, element ->
            val item = element as? JsonObject
                ?: throw IllegalArgumentException("resource ${plan.itemsResourceURI} item $index must be an object")
            ServiceItemSummary(
                id = item.requiredString("id", plan.itemsResourceURI, index),
                title = item.requiredString("title", plan.itemsResourceURI, index),
                state = item.optionalString("state").orEmpty(),
                needsAttention = item["needsAttention"]?.jsonPrimitive?.booleanOrNull ?: false,
            )
        },
    )
}

private fun isJSONResource(resource: ResourceDescriptor): Boolean =
    resource.mimeType == null || resource.mimeType == JsonMimeType

private fun resourceText(readResults: Map<String, ResourcesReadResult>, uri: String): String {
    val readResult = readResults[uri] ?: throw IllegalArgumentException("resource read result is missing $uri")
    val content = readResult.contents.firstOrNull { it.uri == uri }
        ?: throw IllegalArgumentException("resource read result is missing $uri")
    require(content.mimeType == null || content.mimeType == JsonMimeType) {
        "resource $uri must be JSON, got ${content.mimeType.orEmpty()}"
    }
    return content.text ?: throw IllegalArgumentException("resource $uri is missing text content")
}

private fun JsonObject.requiredString(field: String, uri: String, index: Int): String =
    optionalString(field)?.takeIf { it.isNotBlank() }
        ?: throw IllegalArgumentException("resource $uri item $index is missing $field")

private fun JsonObject.optionalString(field: String): String? = this[field]?.jsonPrimitive?.contentOrNull

internal const val GoModeItemsResourceURI = "gomode://items"
internal const val GoModeNotificationsResourceURI = "gomode://notifications"
private const val JsonMimeType = "application/json"
