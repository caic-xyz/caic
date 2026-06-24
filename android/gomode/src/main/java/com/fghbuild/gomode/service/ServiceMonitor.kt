// ServiceMonitor turns MCP resources into native attention, notification, and voice context state.
package com.fghbuild.gomode.service

import com.fghbuild.gomode.sdk.v1.Settings
import com.fghbuild.mcp.sdk.v1.JSONRPCNotification
import com.fghbuild.mcp.sdk.v1.NotificationMethod
import com.fghbuild.mcp.sdk.v1.ResourceDescriptor
import com.fghbuild.mcp.sdk.v1.ResourcesReadResult
import com.fghbuild.mcp.sdk.v1.SubscriptionFilter
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

interface ServiceResourceClient {
    suspend fun listResources(): List<ResourceDescriptor>

    suspend fun readResource(uri: String): ResourcesReadResult

    fun listenSubscriptions(notifications: SubscriptionFilter): Flow<JSONRPCNotification>
}

data class ServiceMonitorState(
    val snapshot: ServiceMonitoringSnapshot? = null,
    val error: String? = null,
) {
    val attentionCount: Int
        get() = snapshot?.attentionCount ?: 0

    val notificationText: String?
        get() = snapshot?.notificationText

    val voiceContext: String?
        get() = snapshot?.voiceContext
}

class ServiceMonitor(
    private val scope: CoroutineScope,
    private val clientFactory: (endpointURL: String, protocolVersion: String) -> ServiceResourceClient,
) {
    private val _state = MutableStateFlow(ServiceMonitorState())
    val state: StateFlow<ServiceMonitorState> = _state.asStateFlow()

    private var job: Job? = null

    fun start(serviceURL: String, settings: Settings) {
        job?.cancel()
        _state.value = ServiceMonitorState()
        job = scope.launch {
            run(serviceURL, settings)
        }
    }

    fun stop() {
        job?.cancel()
        job = null
        _state.value = ServiceMonitorState()
    }

    @Suppress("TooGenericExceptionCaught") // Native monitoring must retry transient service and network failures.
    private suspend fun run(serviceURL: String, settings: Settings) {
        val adapter = serviceResourceAdapterFor(settings.service, settings.apiVersion) ?: run {
            _state.value = ServiceMonitorState()
            return
        }
        val group = settings.webShell.toolGroups.firstOrNull() ?: run {
            _state.value = ServiceMonitorState()
            return
        }
        val endpointURL = resolveServiceURL(serviceURL, group.endpoint)
        val client = clientFactory(endpointURL, group.protocolVersion)
        var retryDelayMs = InitialRetryDelayMs
        while (true) {
            try {
                when (monitorOnce(client, adapter)) {
                    MonitorRunResult.Disabled,
                    MonitorRunResult.Static -> return
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

    private suspend fun monitorOnce(
        client: ServiceResourceClient,
        adapter: ServiceResourceAdapter,
    ): MonitorRunResult {
        var plan = refreshPlan(client, adapter) ?: return MonitorRunResult.Disabled
        val notifications = subscriptionFilter(plan) ?: return MonitorRunResult.Static
        client.listenSubscriptions(notifications).collect { notification ->
            when {
                notification.invalidatesResource(plan) -> refreshSnapshot(client, adapter, plan)
                notification.invalidatesResourceList(plan) -> plan = refreshPlan(client, adapter) ?: return@collect
            }
        }
        return MonitorRunResult.Retry
    }

    private suspend fun refreshPlan(
        client: ServiceResourceClient,
        adapter: ServiceResourceAdapter,
    ): ServiceMonitoringPlan? {
        val plan = adapter.monitoringPlan(client.listResources())
        if (plan == null) {
            _state.value = ServiceMonitorState()
            return null
        }
        refreshSnapshot(client, adapter, plan)
        return plan
    }

    private suspend fun refreshSnapshot(
        client: ServiceResourceClient,
        adapter: ServiceResourceAdapter,
        plan: ServiceMonitoringPlan,
    ) {
        val snapshot = adapter.snapshot(client.readResource(plan.resourceURI))
        _state.value = ServiceMonitorState(snapshot = snapshot)
    }
}

private fun nextRetryDelay(delayMs: Long): Long = (delayMs * 2).coerceAtMost(MaxRetryDelayMs)

private fun subscriptionFilter(plan: ServiceMonitoringPlan): SubscriptionFilter? {
    val resourceSubscriptions = plan.resourceSubscriptions.takeIf { it.isNotEmpty() }
    val resourcesListChanged = plan.resourcesListChanged.takeIf { it }
    if (resourceSubscriptions == null && resourcesListChanged == null) return null
    return SubscriptionFilter(
        resourcesListChanged = resourcesListChanged,
        resourceSubscriptions = resourceSubscriptions,
    )
}

private fun JSONRPCNotification.invalidatesResource(plan: ServiceMonitoringPlan): Boolean {
    if (method != NotificationMethod.ResourcesUpdated) return false
    val uri = (params as? JsonObject)?.get("uri")?.jsonPrimitive?.contentOrNull ?: return false
    return uri in plan.resourceSubscriptions
}

private fun JSONRPCNotification.invalidatesResourceList(plan: ServiceMonitoringPlan): Boolean =
    method == NotificationMethod.ResourcesListChanged && plan.resourcesListChanged

private enum class MonitorRunResult {
    Disabled,
    Static,
    Retry,
}

private const val InitialRetryDelayMs = 1_000L
private const val MaxRetryDelayMs = 30_000L
