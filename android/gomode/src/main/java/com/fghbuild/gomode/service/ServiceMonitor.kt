// ServiceMonitor turns MCP resources into native attention, service notification, and voice context state.
package com.fghbuild.gomode.service

import com.fghbuild.gomode.sdk.v1.Settings
import com.fghbuild.mcp.sdk.v1.CacheScope
import com.fghbuild.mcp.sdk.v1.JSONRPCNotification
import com.fghbuild.mcp.sdk.v1.NotificationMethod
import com.fghbuild.mcp.sdk.v1.ResourceContent
import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import com.fghbuild.mcp.sdk.v1.ResultType
import com.fghbuild.mcp.sdk.v1.SubscriptionFilter
import com.fghbuild.mcp.sdk.v1.SubscriptionsInitialStateParams
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.jsonPrimitive

interface ServiceResourceClient {
    suspend fun listResources(): List<ResourceDescriptor>

    suspend fun readResource(uri: String): ResourcesReadResult

    fun listenSubscriptions(notifications: SubscriptionFilter): Flow<JSONRPCNotification>
}

data class ServiceNotification(
    val id: String,
    val title: String,
    val text: String,
)

data class ServiceMonitorState(
    val snapshot: ServiceMonitoringSnapshot? = null,
    val notifications: List<ServiceNotification> = emptyList(),
    val error: String? = null,
) {
    val attentionCount: Int
        get() = snapshot?.attentionCount ?: 0

    val notificationText: String?
        get() = snapshot?.notificationText
}

class ServiceMonitor(
    private val scope: CoroutineScope,
    private val clientFactory: (endpointURL: String, protocolVersion: String) -> ServiceResourceClient,
) {
    private val _state = MutableStateFlow(ServiceMonitorState())
    val state: StateFlow<ServiceMonitorState> = _state.asStateFlow()

    private var job: Job? = null
    internal val observedNotificationIDs = mutableSetOf<String>()

    fun start(
        serviceURL: String,
        settings: Settings,
    ) {
        job?.cancel()
        observedNotificationIDs.clear()
        _state.value = ServiceMonitorState()
        job =
            scope.launch {
                run(serviceURL, settings)
            }
    }

    fun stop() {
        job?.cancel()
        job = null
        observedNotificationIDs.clear()
        _state.value = ServiceMonitorState()
    }

    @Suppress("TooGenericExceptionCaught") // Native monitoring must retry transient service and network failures.
    private suspend fun run(serviceURL: String, settings: Settings) {
        val group =
            settings.webShell.toolGroups.firstOrNull() ?: run {
                _state.value = ServiceMonitorState()
                return
            }
        val endpointURL = resolveServiceURL(serviceURL, group.endpoint)
        val client = clientFactory(endpointURL, group.protocolVersion)
        var retryDelayMs = INITIAL_RETRY_DELAY_MS
        while (true) {
            try {
                when (monitorOnce(client)) {
                    MonitorRunResult.Disabled,
                    MonitorRunResult.Static,
                    -> {
                        return
                    }

                    MonitorRunResult.Retry -> {
                        _state.value = ServiceMonitorState(error = "MCP subscription stream ended")
                        delay(retryDelayMs)
                        retryDelayMs = nextRetryDelay(retryDelayMs)
                    }
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _state.value = ServiceMonitorState(error = e.message ?: "Service monitoring failed")
                delay(retryDelayMs)
                retryDelayMs = nextRetryDelay(retryDelayMs)
            }
        }
    }

    private suspend fun monitorOnce(client: ServiceResourceClient): MonitorRunResult {
        var plan = refreshPlan(client) ?: return MonitorRunResult.Disabled
        val notifications = subscriptionFilter(plan) ?: return MonitorRunResult.Static
        val initial = InitialStateWindow()
        client.listenSubscriptions(notifications).collect { notification ->
            val initialState = notification.initialStatePayload()
            when {
                initialState != null -> {
                    initial.recordInitialState(initialState.uri, initialState.contents)
                    if (plan.resourceURIs.all { it in initial.deliveredContents }) {
                        applyDeliveredSnapshot(plan, initial.deliveredContents)
                    }
                }

                notification.invalidatesResource(plan) -> {
                    if (initial.consumeLegacyUpdate(notification.resourceUri())) return@collect
                    refreshSnapshot(client, plan)
                }

                notification.invalidatesResourceList(plan) -> {
                    if (initial.consumeInitialListChanged()) {
                        plan = refreshPlanAfterLeadingListChanged(client, plan) ?: return@collect
                    } else {
                        plan = refreshPlan(client) ?: return@collect
                    }
                }
            }
        }
        return MonitorRunResult.Retry
    }

    private suspend fun refreshPlan(client: ServiceResourceClient): ServiceMonitoringPlan? {
        val plan = serviceMonitoringPlan(client.listResources())
        if (plan == null) {
            _state.value = ServiceMonitorState()
            return null
        }
        refreshSnapshot(client, plan)
        return plan
    }

    // The leading resources/list_changed arrives before the opening window's
    // state authority (delivered payloads or the legacy re-read burst), so it
    // re-checks the resource list without re-reading state unless the list
    // actually changed; the list call itself predates this phase.
    private suspend fun refreshPlanAfterLeadingListChanged(
        client: ServiceResourceClient,
        plan: ServiceMonitoringPlan,
    ): ServiceMonitoringPlan? {
        val newPlan = serviceMonitoringPlan(client.listResources())
        if (newPlan == null) {
            _state.value = ServiceMonitorState()
            return null
        }
        if (newPlan != plan) {
            refreshSnapshot(client, newPlan)
        }
        return newPlan
    }

    private suspend fun refreshSnapshot(
        client: ServiceResourceClient,
        plan: ServiceMonitoringPlan,
    ) {
        val readResults = plan.resourceURIs.associateWith { uri -> client.readResource(uri) }
        val snapshot = serviceMonitoringSnapshot(readResults, plan)
        _state.value =
            ServiceMonitorState(
                snapshot = snapshot,
                notifications = serviceNotifications(readResults, plan),
            )
    }

    // Exposes the delivered baseline with the same state construction as
    // refreshSnapshot, so only the state authority changes.
    private fun applyDeliveredSnapshot(
        plan: ServiceMonitoringPlan,
        delivered: Map<String, List<ResourceContent>>,
    ) {
        val readResults =
            plan.resourceURIs.associateWith { uri ->
                ResourcesReadResult(
                    resultType = ResultType.Complete,
                    contents = delivered[uri].orEmpty(),
                    ttlMs = 0,
                    cacheScope = CacheScope.Private,
                )
            }
        val snapshot = serviceMonitoringSnapshot(readResults, plan)
        _state.value =
            ServiceMonitorState(
                snapshot = snapshot,
                notifications = serviceNotifications(readResults, plan),
            )
    }
}

private fun ServiceMonitor.serviceNotifications(
    readResults: Map<String, ResourcesReadResult>,
    plan: ServiceMonitoringPlan,
): List<ServiceNotification> {
    val uri = plan.notificationResourceURI ?: return emptyList()
    val readResult = readResults[uri] ?: throw IllegalArgumentException("resource read result is missing $uri")
    val content =
        readResult.contents.firstOrNull { it.uri == uri }
            ?: throw IllegalArgumentException("resource read result is missing $uri")
    val text = content.text ?: throw IllegalArgumentException("resource $uri is missing text content")
    val events =
        Json.parseToJsonElement(text) as? JsonArray
            ?: throw IllegalArgumentException("resource $uri must be a JSON array")
    return events.mapIndexedNotNull { index, element ->
        val event =
            element as? JsonObject
                ?: throw IllegalArgumentException("resource $uri item $index must be an object")
        val id = event.requiredNotificationString("id", uri, index)
        if (!observedNotificationIDs.add(id)) return@mapIndexedNotNull null
        ServiceNotification(
            id = id,
            title = event.requiredNotificationString("title", uri, index),
            text = event.requiredNotificationString("body", uri, index),
        )
    }
}

private fun JsonObject.requiredNotificationString(
    field: String,
    uri: String,
    index: Int,
): String =
    this[field]?.jsonPrimitive?.contentOrNull?.takeIf { it.isNotBlank() }
        ?: throw IllegalArgumentException("resource $uri item $index is missing $field")

private fun nextRetryDelay(delayMs: Long): Long = (delayMs * 2).coerceAtMost(MAX_RETRY_DELAY_MS)

private fun subscriptionFilter(plan: ServiceMonitoringPlan): SubscriptionFilter? {
    val resourceSubscriptions = plan.resourceSubscriptions.takeIf { it.isNotEmpty() }
    val resourcesListChanged = plan.resourcesListChanged.takeIf { it }
    if (resourceSubscriptions == null && resourcesListChanged == null) return null
    return SubscriptionFilter(
        resourcesListChanged = resourcesListChanged,
        resourceSubscriptions = resourceSubscriptions,
    )
}

// Tracks the opening window of a subscription stream: the caic-native
// initial_state payloads, the legacy resources/updated re-read burst that
// always follows each payload, and the leading resources/list_changed
// notification. A payload that covers every monitored URI is the authoritative
// baseline and is exposed without a re-read; the legacy notifications that
// follow delivered payloads are consumed without round trips, while URIs that
// arrive without a payload keep the re-read fallback.
private class InitialStateWindow {
    private val contents = mutableMapOf<String, List<ResourceContent>>()
    private val consumedUpdates = mutableSetOf<String>()
    private var consumedListChanged = false

    val deliveredContents: Map<String, List<ResourceContent>>
        get() = contents

    fun recordInitialState(
        uri: String,
        initialContents: List<ResourceContent>,
    ) {
        contents[uri] = initialContents
    }

    fun consumeLegacyUpdate(uri: String?): Boolean {
        if (uri == null || uri !in contents) return false
        return consumedUpdates.add(uri)
    }

    fun consumeInitialListChanged(): Boolean {
        if (consumedListChanged) return false
        consumedListChanged = true
        return true
    }
}

private fun JSONRPCNotification.resourceUri(): String? =
    (params as? JsonObject)?.get("uri")?.jsonPrimitive?.contentOrNull

private fun JSONRPCNotification.initialStatePayload(): SubscriptionsInitialStateParams? {
    if (method != NotificationMethod.SubscriptionsInitialState) return null
    val params = this.params as? JsonObject ?: return null
    return try {
        mcpJson.decodeFromJsonElement(params)
    } catch (_: SerializationException) {
        null
    }
}

private fun JSONRPCNotification.invalidatesResource(plan: ServiceMonitoringPlan): Boolean {
    if (method != NotificationMethod.ResourcesUpdated) return false
    return resourceUri() in plan.resourceSubscriptions
}

private fun JSONRPCNotification.invalidatesResourceList(plan: ServiceMonitoringPlan): Boolean =
    method == NotificationMethod.ResourcesListChanged && plan.resourcesListChanged

private enum class MonitorRunResult {
    Disabled,
    Static,
    Retry,
}

private val mcpJson = Json { ignoreUnknownKeys = true }

private const val INITIAL_RETRY_DELAY_MS = 1_000L
private const val MAX_RETRY_DELAY_MS = 30_000L
