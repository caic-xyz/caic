// HaloService: bridge between caic task state and a Halo smart glasses device over BLE.
//
// Manages HaloConnection lifecycle, observes TaskRepository for task state changes,
// sends status sprites and text to the Halo display, and dispatches button click
// events (RxClick) as caic actions.
//
// Testable: all dependencies are constructor-injected; HaloConnection is open with
// overridable methods.
package com.fghbuild.caic.halo

import android.content.Context
import android.util.Log
import com.caic.halo.ble.HaloConnection
import com.caic.halo.ble.HaloDevice
import com.caic.halo.ble.HaloException
import com.caic.halo.ble.HaloScannedDevice
import com.caic.halo.msg.ClickType
import com.caic.halo.msg.HalosideApp
import com.caic.halo.msg.RxClick
import com.caic.halo.msg.TxMessage
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.catch
import kotlinx.coroutines.launch
import javax.inject.Inject
import javax.inject.Singleton

private const val TAG = "HaloService"

/** Connection state exposed to the UI via [HaloService.state]. */
enum class HaloConnectionState {
    Disconnected,
    Scanning,
    Connecting,
    Connected,
    /** Connected but HalosideApp failed to start. */
    Error,
}

/**
 * Minimal TxMessage that wraps a UTF-8 string payload for data messages.
 * Used to send status text to the Halo device via HalosideApp.send().
 */
private class TxRaw(private val text: String) : TxMessage {
    override fun pack(): ByteArray = text.toByteArray(Charsets.UTF_8)
}

@Singleton
class HaloService @Inject constructor(
    @param:ApplicationContext private val context: Context,
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
    private val haloConnection: HaloConnection,
) {
    private val _state = MutableStateFlow(HaloConnectionState.Disconnected)
    val state: StateFlow<HaloConnectionState> = _state.asStateFlow()

    private var device: HaloDevice? = null
    private var halosideApp: HalosideApp? = null
    private var observeJob: Job? = null
    private var clickJob: Job? = null

    /** Address of the currently connected device, or null. */
    val connectedAddress: String?
        get() = device?.uuid

    // ---- Public API ----

    /**
     * Start observing settings and task state. Call once from a long-lived scope.
     * If a Halo address is persisted and auto-connect is enabled, attempts to reconnect.
     */
    fun start(scope: CoroutineScope) {
        scope.launch {
            settingsRepository.settings.collect { settings ->
                // TODO: auto-reconnect when haloAddress is persisted
            }
        }
    }

    /** Begin BLE scanning for Halo/Frame devices. */
    fun scan(): kotlinx.coroutines.flow.Flow<HaloScannedDevice> {
        _state.value = HaloConnectionState.Scanning
        return haloConnection.scan()
    }

    /** Stop an active scan. Called when the user selects a device or leaves the screen. */
    fun stopScan() {
        if (_state.value == HaloConnectionState.Scanning) {
            _state.value = HaloConnectionState.Disconnected
        }
    }

    /**
     * Connect to a scanned Halo device, boot the HalosideApp, and begin observing task state.
     * Returns [Result.success] on successful connection, or [Result.failure] with the error.
     */
    @Suppress("TooGenericExceptionCaught") // Error boundary.
    suspend fun connect(scanned: HaloScannedDevice): Result<Unit> {
        _state.value = HaloConnectionState.Connecting
        return try {
            val dev = haloConnection.connect(scanned)
            device = dev

            val app = HalosideApp(context, dev)
            halosideApp = app

            // Upload and start the device-side application.
            app.start(mainLuaSource())

            // Begin observing task state and button clicks.
            startObservingTasks()
            startObservingClicks()

            _state.value = HaloConnectionState.Connected
            Result.success(Unit)
        } catch (e: HaloException) {
            Log.e(TAG, "Connection failed: ${e.message}", e)
            _state.value = HaloConnectionState.Error
            Result.failure(e)
        } catch (e: Exception) {
            Log.e(TAG, "Unexpected error during connect: ${e.message}", e)
            _state.value = HaloConnectionState.Error
            Result.failure(e)
        }
    }

    /** Disconnect from the Halo device and stop observing. */
    suspend fun disconnect() {
        clickJob?.cancel()
        clickJob = null
        observeJob?.cancel()
        observeJob = null
        try {
            haloConnection.disconnect()
        } catch (_: Exception) { }
        device = null
        halosideApp = null
        _state.value = HaloConnectionState.Disconnected
    }

    // ---- Task state observation ----

    private fun startObservingTasks() {
        observeJob?.cancel()
        observeJob = CoroutineScope(Dispatchers.Default).launch {
            var prevSnapshot: List<Task> = emptyList()
            taskRepository.tasks.collect { tasks ->
                val changes = diffTasks(prevSnapshot, tasks)
                prevSnapshot = tasks
                if (changes.isEmpty()) return@collect
                sendStatusUpdate(tasks)
            }
        }
    }

    private suspend fun sendStatusUpdate(tasks: List<Task>) {
        val app = halosideApp ?: return
        try {
            val primary = primaryTask(tasks)
            val summary = buildStatusString(tasks, primary)
            app.send(MSG_CODE_STATUS, TxRaw(summary))
        } catch (e: HaloException) {
            Log.w(TAG, "Failed to send status update: ${e.message}")
        }
    }

    // ---- Button click observation ----

    private fun startObservingClicks() {
        clickJob?.cancel()
        val dev = device ?: return
        clickJob = CoroutineScope(Dispatchers.Default).launch {
            RxClick().attach(dev.dataResponse)
                .catch { Log.w(TAG, "RxClick parse error: ${it.message}") }
                .collect { click -> handleClick(click) }
        }
    }

    private suspend fun handleClick(click: ClickType) {
        Log.d(TAG, "Halo click: $click")
        val tasks = taskRepository.tasks.value
        when (click) {
            ClickType.SINGLE -> cycleAttentionTask(tasks)
            ClickType.DOUBLE -> readCurrentTaskStatus(tasks)
            ClickType.LONG -> purgeCurrentTask(tasks)
        }
    }

    private suspend fun cycleAttentionTask(tasks: List<Task>) {
        // TODO: cycle through attention-needed tasks and display the next one
        Log.d(TAG, "cycleAttentionTask: ${tasks.size} tasks")
    }

    private suspend fun readCurrentTaskStatus(tasks: List<Task>) {
        // TODO: TTS the current task status (deferred: needs audio pipeline)
        Log.d(TAG, "readCurrentTaskStatus: ${tasks.size} tasks")
    }

    private suspend fun purgeCurrentTask(tasks: List<Task>) {
        // TODO: purge the currently displayed task
        Log.d(TAG, "purgeCurrentTask: ${tasks.size} tasks")
    }

    // ---- Display helpers ----

    companion object {
        /** Message code for status payloads (app → device). */
        const val MSG_CODE_STATUS = 0x10

        /**
         * Returns the single most important task to display: first attention-needed task,
         * otherwise the most recently updated active task.
         */
        fun primaryTask(tasks: List<Task>): Task? {
            val attention = tasks.firstOrNull { it.state in ATTENTION_STATES }
            if (attention != null) return attention
            return tasks.maxByOrNull { it.stateUpdatedAt }
        }

        /** States that mean the user should pay attention. */
        val ATTENTION_STATES = setOf(TaskState.Waiting, TaskState.Asking, TaskState.HasPlan)

        /**
         * Build a compact status string for the Halo display.
         * Format: "3 tasks  •  <primary task title>  •  <state label>"
         * Fits within ~40 characters for the Halo's small internal font.
         */
        fun buildStatusString(tasks: List<Task>, primary: Task?): String {
            val count = "${tasks.size} task${if (tasks.size != 1) "s" else ""}"
            val title = primary?.title?.take(20) ?: "none"
            val state = primary?.let { stateLabel(it.state) } ?: ""
            return "$count  •  $title  •  $state"
        }

        /** Compact human-readable label for each task state. */
        fun stateLabel(state: TaskState): String = when (state) {
            TaskState.Pending -> "Pending"
            TaskState.Branching -> "Branch"
            TaskState.Provisioning -> "Prov"
            TaskState.Starting -> "Start"
            TaskState.Running -> "Running"
            TaskState.Waiting -> "Waiting"
            TaskState.Asking -> "Asking"
            TaskState.HasPlan -> "Plan"
            TaskState.Pulling -> "Pull"
            TaskState.Pushing -> "Push"
            TaskState.Stopping -> "Stopping"
            TaskState.Stopped -> "Stopped"
            TaskState.Purging -> "Purging"
            TaskState.Purged -> "Done"
            TaskState.Failed -> "Failed"
            is TaskState.Other -> state.value.take(6)
        }

        /**
         * Compute which tasks changed state between two snapshots.
         * Returns tasks whose state differs from the previous snapshot.
         */
        fun diffTasks(prev: List<Task>, curr: List<Task>): List<Task> {
            val prevMap = prev.associateBy { it.id }
            return curr.filter { task ->
                val prevTask = prevMap[task.id]
                prevTask == null || prevTask.state != task.state
            }
        }

        /**
         * The Lua source for the Halo device application.
         * Runs a loop: wait for status messages → render on display → report button clicks.
         */
        fun mainLuaSource(): String = """
-- caic Halo app: display task status and dispatch button clicks.
-- Phone sends status as data message (msgCode 0x10): compact text string.
-- Button clicks are sent back as RxClick data messages.

local status_text = "caic"

-- Render the current status on the display.
local function render()
    frame.display.text(status_text, 1, 20)
    frame.display.show()
end

-- Process incoming data messages from the phone.
local function on_data(msg_code, payload)
    if msg_code == 0x10 then
        status_text = payload
        render()
    end
end

-- Button callback: send click gesture back to the phone.
local click_start = 0
local click_count = 0
local long_sent = false

local function reset_click()
    click_start = 0
    click_count = 0
    long_sent = false
end

local function on_button(pressed)
    if pressed then
        if click_start == 0 then
            click_start = frame.time.utc()
            click_count = 0
            long_sent = false
        end
        click_count = click_count + 1
    else
        local elapsed = frame.time.utc() - click_start
        local gesture
        if elapsed > 1.0 and not long_sent then
            gesture = "long"
            long_sent = true
        elseif click_count == 1 then
            gesture = "single"
        else
            gesture = "double"
        end
        frame.bluetooth.send(string.format('{"gesture":"%s"}', gesture))
        reset_click()
    end
end

-- Initial render.
render()

-- Main event loop: poll for data, handle button presses.
while true do
    frame.button.on_change(on_button)
    data.set_data_response_callback(on_data)
    frame.sleep(0.1)
end
""".trimIndent()
    }
}
