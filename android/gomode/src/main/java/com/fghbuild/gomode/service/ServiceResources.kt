// Service resource parsing turns generic Go Mode MCP resources into native monitoring state.
package com.fghbuild.gomode.service

import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import kotlinx.coroutines.CancellationException
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull
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
    val moreItemsHint: String? = null,
    val omittedItemCount: Int = 0,
) {
    val attentionItems: List<ServiceItemSummary>
        get() = items.filter { it.needsAttention }

    val attentionCount: Int
        get() = attentionItems.size

    val notificationText: String?
        get() =
            when (attentionCount) {
                0 -> null
                1 -> "${attentionItems.single().title} needs attention"
                else -> "$attentionCount items need attention"
            }

    val voiceContext: String
        get() {
            // Android owns this bounded session baseline; see the canonical contract in
            // gomode/docs/ANDROID_SHELL.md#service-item-voice-context-ownership.
            if (items.isEmpty() && omittedItemCount == 0) return "No visible service items."
            // Keep these setup-context limits and formatting aligned with the browser's ServiceItems.ts.
            val limit = minOf(items.size, MAX_INITIAL_SERVICE_ITEMS)
            var included = 0
            for (count in 1..limit) {
                if (formatVoiceContext(count).length > MAX_INITIAL_SERVICE_CONTEXT_CHARS) break
                included = count
            }
            return formatVoiceContext(included)
        }

    private fun formatVoiceContext(count: Int): String {
        val lines =
            items
                .take(count)
                .map { item ->
                    val reference =
                        item.reference
                            ?.takeIf { it.isNotEmpty() }
                            ?.let { "$it: " }
                            .orEmpty()
                    val state =
                        item.state
                            .takeIf { it.isNotEmpty() }
                            ?.let {
                                " ($it${if (item.needsAttention) ", needs attention" else ""})"
                            }.orEmpty()
                    "- $reference${item.title}$state"
                }.toMutableList()
        val omitted = omittedItemCount + items.size - count
        if (omitted > 0) {
            val hint = moreItemsHint?.takeIf { it.isNotEmpty() }?.let { " $it" }.orEmpty()
            lines += "- … $omitted more items omitted.$hint"
        }
        return "Current service items:\n${lines.joinToString("\n")}"
    }
}

data class ServiceItemSummary(
    val id: String,
    val reference: String? = null,
    val title: String,
    val state: String,
    val needsAttention: Boolean,
)

fun serviceMonitoringPlan(resources: List<ResourceDescriptor>): ServiceMonitoringPlan? {
    val itemsResource = resources.firstOrNull { it.uri == GOMODE_ITEMS_RESOURCE_URI } ?: return null
    if (!isJSONResource(itemsResource)) return null
    val notificationResource = resources.firstOrNull { it.uri == GOMODE_NOTIFICATIONS_RESOURCE_URI }
    if (notificationResource != null && !isJSONResource(notificationResource)) return null
    return ServiceMonitoringPlan(
        itemsResourceURI = itemsResource.uri,
        notificationResourceURI = notificationResource?.uri,
    )
}

fun serviceMonitoringSnapshot(
    readResults: Map<String, ResourcesReadResult>,
    plan: ServiceMonitoringPlan,
): ServiceMonitoringSnapshot {
    val root = Json.parseToJsonElement(resourceText(readResults, plan.itemsResourceURI))
    require(root is JsonObject) { "resource ${plan.itemsResourceURI} must be a JSON object" }
    val items =
        root["items"] as? JsonArray
            ?: throw IllegalArgumentException("resource ${plan.itemsResourceURI} is missing items array")
    val omittedItemCount =
        root["omittedCount"]?.let { value ->
            value.jsonPrimitive.intOrNull
                ?: throw IllegalArgumentException("resource ${plan.itemsResourceURI} has a non-integer omittedCount")
        } ?: 0
    require(omittedItemCount >= 0) { "resource ${plan.itemsResourceURI} has a negative omittedCount" }
    return ServiceMonitoringSnapshot(
        items =
            items.mapIndexed { index, element ->
                val item =
                    element as? JsonObject
                        ?: throw IllegalArgumentException(
                            "resource ${plan.itemsResourceURI} item $index must be an object",
                        )
                ServiceItemSummary(
                    id = item.requiredString("id", plan.itemsResourceURI, index),
                    reference = item.optionalString("reference"),
                    title = item.requiredString("title", plan.itemsResourceURI, index),
                    state = item.optionalString("state").orEmpty(),
                    needsAttention = item["needsAttention"]?.jsonPrimitive?.booleanOrNull ?: false,
                )
            },
        moreItemsHint = root["moreItemsHint"]?.jsonPrimitive?.contentOrNull?.take(MAX_MORE_ITEMS_HINT_CHARS),
        omittedItemCount = omittedItemCount,
    )
}

suspend fun readInitialServiceContext(client: ServiceResourceClient): String {
    return try {
        val plan = serviceMonitoringPlan(client.listResources()) ?: return "No visible service items."
        val items = client.readResource(plan.itemsResourceURI)
        serviceMonitoringSnapshot(mapOf(plan.itemsResourceURI to items), plan).voiceContext
    } catch (e: CancellationException) {
        throw e
    } catch (_: Exception) {
        // Service context is advisory: resource failures and version skew must
        // not prevent the otherwise independent voice transport from starting.
        "No visible service items."
    }
}

private fun isJSONResource(resource: ResourceDescriptor): Boolean =
    resource.mimeType == null || resource.mimeType == JSON_MIME_TYPE

private fun resourceText(
    readResults: Map<String, ResourcesReadResult>,
    uri: String,
): String {
    val readResult = readResults[uri] ?: throw IllegalArgumentException("resource read result is missing $uri")
    val content =
        readResult.contents.firstOrNull { it.uri == uri }
            ?: throw IllegalArgumentException("resource read result is missing $uri")
    require(content.mimeType == null || content.mimeType == JSON_MIME_TYPE) {
        "resource $uri must be JSON, got ${content.mimeType.orEmpty()}"
    }
    return content.text ?: throw IllegalArgumentException("resource $uri is missing text content")
}

private fun JsonObject.requiredString(
    field: String,
    uri: String,
    index: Int,
): String =
    optionalString(field)?.takeIf { it.isNotBlank() }
        ?: throw IllegalArgumentException("resource $uri item $index is missing $field")

private fun JsonObject.optionalString(field: String): String? = this[field]?.jsonPrimitive?.contentOrNull

internal const val GOMODE_ITEMS_RESOURCE_URI = "gomode://items"
internal const val GOMODE_NOTIFICATIONS_RESOURCE_URI = "gomode://notifications"
private const val JSON_MIME_TYPE = "application/json"
private const val MAX_INITIAL_SERVICE_CONTEXT_CHARS = 4000
private const val MAX_INITIAL_SERVICE_ITEMS = 20
private const val MAX_MORE_ITEMS_HINT_CHARS = 512
