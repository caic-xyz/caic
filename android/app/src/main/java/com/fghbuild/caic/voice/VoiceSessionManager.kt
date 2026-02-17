// Manages Gemini Live WebSocket session, audio I/O, and function call dispatch.
package com.fghbuild.caic.voice

import android.annotation.SuppressLint
import android.content.Context
import android.util.Log
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaRecorder
import android.os.Handler
import android.os.Looper
import android.net.Uri
import android.util.Base64
import com.caic.sdk.ApiClient
import com.fghbuild.caic.data.SettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton

private const val TAG = "VoiceSession"
private val functionScheduling: Map<String, String> =
    functionDeclarations.mapNotNull { fd ->
        fd.scheduling?.let { fd.name to it }
    }.toMap()
private const val RECORD_SAMPLE_RATE = 16000
private const val PLAYBACK_SAMPLE_RATE = 24000
private const val AUDIO_BUFFER_SIZE = 4096
private const val WS_CLOSE_NORMAL = 1000
private const val MODEL_NAME = "models/gemini-2.5-flash-native-audio-preview-12-2025"
/** Internal AudioRecord buffer = 4x minimum to absorb scheduling jitter. */
private const val RECORD_BUFFER_MULTIPLIER = 4

@Singleton
class VoiceSessionManager @Inject constructor(
    @ApplicationContext context: Context,
    private val settingsRepository: SettingsRepository,
) {
    private val audioManager = context.getSystemService(AudioManager::class.java)
    private val json = Json { ignoreUnknownKeys = true }
    private val client = OkHttpClient.Builder()
        .readTimeout(0, TimeUnit.MILLISECONDS)
        .build()

    private var webSocket: WebSocket? = null
    private var audioRecord: AudioRecord? = null
    private var audioTrack: AudioTrack? = null
    private var recordingJob: Job? = null
    private var functionHandlers: FunctionHandlers? = null
    private var deviceCallback: AudioDeviceCallback? = null

    private val _state = MutableStateFlow(VoiceState())
    val state: StateFlow<VoiceState> = _state.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

    var onSetActiveTask: ((String) -> Unit)? = null

    fun setError(message: String) {
        Log.e(TAG, "setError: $message")
        releaseAudio()
        _state.update {
            it.copy(
                connectStatus = null,
                connected = false,
                listening = false,
                speaking = false,
                error = message,
                errorId = it.errorId + 1,
            )
        }
    }

    private fun setStatus(status: String) {
        Log.i(TAG, status)
        _state.update { it.copy(connectStatus = status, error = null) }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all failures to UI.
    fun connect() {
        // Close any existing connection to prevent WebSocket leaks.
        webSocket?.close(WS_CLOSE_NORMAL, "Reconnecting")
        webSocket = null
        setStatus("Fetching token…")

        scope.launch {
            try {
                val settings = settingsRepository.settings.value
                if (settings.serverURL.isBlank()) {
                    setError("Server URL is not configured")
                    return@launch
                }

                val apiClient = ApiClient(settings.serverURL)
                functionHandlers = FunctionHandlers(apiClient).also {
                    it.onSetActiveTask = onSetActiveTask
                }

                val tokenResp = apiClient.getVoiceToken()
                setStatus("Connecting…")
                // Ephemeral tokens (v1alpha) use access_token + BidiGenerateContentConstrained.
                // Raw API keys (v1beta) use key + BidiGenerateContent.
                val wsUrl = if (tokenResp.ephemeral) {
                    Uri.Builder()
                        .scheme("wss")
                        .authority("generativelanguage.googleapis.com")
                        .path(
                            "/ws/google.ai.generativelanguage.v1alpha" +
                                ".GenerativeService.BidiGenerateContentConstrained",
                        )
                        .appendQueryParameter("access_token", tokenResp.token)
                        .build()
                        .toString()
                } else {
                    Uri.Builder()
                        .scheme("wss")
                        .authority("generativelanguage.googleapis.com")
                        .path("/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent")
                        .appendQueryParameter("key", tokenResp.token)
                        .build()
                        .toString()
                }

                val request = Request.Builder().url(wsUrl).build()
                webSocket = client.newWebSocket(request, createWebSocketListener())
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                setError(e.message ?: "Connection failed")
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all failures to UI.
    private fun startAudio() {
        try {
            refreshAvailableDevices()
            registerDeviceCallback()
            setupAudioRecord()
            check(audioRecord?.state == AudioRecord.STATE_INITIALIZED) {
                "Microphone initialization failed"
            }
            setupAudioTrack()
            startRecording()
            _state.update { it.copy(listening = true) }
        } catch (e: Exception) {
            setError(e.message ?: "Audio setup failed")
        }
    }

    /** Release audio resources without throwing. */
    @Suppress("TooGenericExceptionCaught") // Must not throw from cleanup.
    private fun releaseAudio() {
        recordingJob?.cancel()
        recordingJob = null
        unregisterDeviceCallback()
        try {
            audioRecord?.stop()
        } catch (e: Exception) {
            Log.w(TAG, "AudioRecord.stop() failed: ${e.message}")
        }
        try {
            audioRecord?.release()
        } catch (e: Exception) {
            Log.w(TAG, "AudioRecord.release() failed: ${e.message}")
        }
        audioRecord = null
        try {
            audioTrack?.stop()
        } catch (e: Exception) {
            Log.w(TAG, "AudioTrack.stop() failed: ${e.message}")
        }
        try {
            audioTrack?.release()
        } catch (e: Exception) {
            Log.w(TAG, "AudioTrack.release() failed: ${e.message}")
        }
        audioTrack = null
        clearCommunicationDevice()
    }

    fun disconnect() {
        releaseAudio()
        webSocket?.close(WS_CLOSE_NORMAL, "User disconnected")
        webSocket = null
        functionHandlers = null
        _state.value = VoiceState()  // clears transcripts too
    }

    fun injectText(text: String) {
        val clientContent = BidiGenerateContentClientContent(
            turns = listOf(
                Content(
                    role = "user",
                    parts = listOf(Part(text = text)),
                )
            ),
            turnComplete = true,
        )
        webSocket?.send(json.encodeToString(BidiGenerateContentClientContent.serializer(), clientContent)
            .wrapTopLevel("clientContent"))
    }

    private fun sendSetupMessage(voiceName: String) {
        val setup = buildSetupMessage(voiceName)
        Log.i(TAG, "sending setup message")
        webSocket?.send(setup)
    }

    private fun buildSetupMessage(voiceName: String): String {
        val setup = BidiGenerateContentSetup(
            model = MODEL_NAME,
            generationConfig = GenerationConfig(
                responseModalities = listOf(ResponseModality.AUDIO),
                speechConfig = SpeechConfig(
                    voiceConfig = VoiceConfig(
                        prebuiltVoiceConfig = PrebuiltVoiceConfig(voiceName = voiceName),
                    )
                ),
            ),
            realtimeInputConfig = RealtimeInputConfig(
                automaticActivityDetection = AutomaticActivityDetection(
                    startOfSpeechSensitivity = StartSensitivity.HIGH,
                ),
                activityHandling = ActivityHandling.START_OF_ACTIVITY_INTERRUPTS,
            ),
            systemInstruction = Content(
                parts = listOf(Part(text = SYSTEM_INSTRUCTION)),
            ),
            tools = listOf(
                Tool(
                    functionDeclarations = functionDeclarations.map { fd ->
                        LiveFunctionDeclaration(
                            name = fd.name,
                            description = fd.description,
                            parameters = fd.parameters,
                            behavior = fd.behavior,
                        )
                    }
                )
            ),
            inputAudioTranscription = AudioTranscriptionConfig(),
            outputAudioTranscription = AudioTranscriptionConfig(),
        )
        return json.encodeToString(BidiGenerateContentSetup.serializer(), setup)
            .wrapTopLevel("setup")
    }

    private fun createWebSocketListener() = object : WebSocketListener() {
        /** Ignore callbacks from WebSockets we already replaced or disconnected. */
        private fun isStale(ws: WebSocket) = ws !== this@VoiceSessionManager.webSocket

        override fun onOpen(webSocket: WebSocket, response: Response) {
            if (isStale(webSocket)) return
            setStatus("Sending setup…")
            val voiceName = settingsRepository.settings.value.voiceName
            sendSetupMessage(voiceName)
            setStatus("Waiting for server…")
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            if (isStale(webSocket)) return
            scope.launch { handleServerMessage(text) }
        }

        override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
            if (isStale(webSocket)) return
            scope.launch { handleServerMessage(bytes.utf8()) }
        }

        override fun onFailure(
            webSocket: WebSocket,
            t: Throwable,
            response: Response?,
        ) {
            Log.e(TAG, "WebSocket failure: ${t.message}", t)
            if (isStale(webSocket)) return
            setError(t.message ?: "WebSocket connection failed")
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            Log.i(TAG, "WebSocket closing: code=$code reason=$reason")
            webSocket.close(code, reason)
        }

        override fun onClosed(
            webSocket: WebSocket,
            code: Int,
            reason: String,
        ) {
            Log.i(TAG, "WebSocket closed: code=$code reason=$reason")
            if (isStale(webSocket)) return
            if (!_state.value.connected) {
                // Connection closed before setupComplete — surface an error.
                val msg = formatCloseReason(code, reason)
                setError(msg)
            } else {
                _state.update { it.copy(connected = false, listening = false, speaking = false) }
            }
        }
    }

    /** Map WebSocket close reasons to user-friendly messages.
     *  Close reasons are truncated to 123 bytes by RFC 6455. */
    private fun formatCloseReason(code: Int, reason: String): String {
        if (reason.contains("unregistered callers")) {
            return "Voice auth failed — check that GEMINI_API_KEY is set on the server"
        }
        return reason.ifBlank { "Connection closed (code $code)" }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: malformed messages must not crash.
    private suspend fun handleServerMessage(text: String) {
        try {
            val msg = json.decodeFromString<JsonElement>(text).jsonObject
            when {
                "setupComplete" in msg -> {
                    Log.i(TAG, "setupComplete received, starting audio")
                    _state.update { it.copy(connectStatus = null, connected = true, error = null) }
                    startAudio()
                }
                "serverContent" in msg -> {
                    val serverContent = json.decodeFromJsonElement(
                        BidiGenerateContentServerContent.serializer(),
                        msg["serverContent"]!!,
                    )
                    handleServerContent(serverContent)
                }
                "toolCall" in msg -> {
                    val toolCall = json.decodeFromJsonElement(
                        BidiGenerateContentToolCall.serializer(),
                        msg["toolCall"]!!,
                    )
                    handleToolCall(toolCall)
                }
                "toolCallCancellation" in msg -> {
                    _state.update { it.copy(activeTool = null) }
                }
                else -> {
                    Log.w(TAG, "Unrecognized server message: ${msg.keys}")
                    // Surface error responses from Gemini (e.g. invalid model, auth failure).
                    val error = msg["error"]?.jsonObject
                    if (error != null) {
                        val message = error["message"]?.jsonPrimitive?.content
                            ?: "Server error"
                        setError(message)
                    }
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            setError(e.message ?: "Failed to process server message")
        }
    }

    private fun handleServerContent(content: BidiGenerateContentServerContent) {
        content.modelTurn?.parts?.forEach { part ->
            val inlineData = part.inlineData
            if (inlineData != null) {
                val pcmBytes = Base64.decode(inlineData.data, Base64.NO_WRAP)
                playAudio(pcmBytes)
                _state.update { it.copy(speaking = true) }
            }
        }
        content.inputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.USER, text)) }
        }
        content.outputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.ASSISTANT, text)) }
        }
        if (content.turnComplete == true) {
            _state.update {
                it.copy(
                    speaking = false,
                    transcript = it.transcript.map { e -> e.copy(final = true) },
                )
            }
        }
    }

    private suspend fun handleToolCall(toolCall: BidiGenerateContentToolCall) {
        val responses = toolCall.functionCalls.mapNotNull { fc ->
            val scheduling = functionScheduling[fc.name]
            _state.update { it.copy(activeTool = fc.name) }
            val result = functionHandlers?.handle(fc.name, fc.args) ?: errorJson("No handler")
            _state.update { it.copy(activeTool = null) }

            val response = if (scheduling != null && result is JsonObject) {
                JsonObject(result.toMutableMap().apply {
                    put("scheduling", JsonPrimitive(scheduling))
                })
            } else {
                result
            }
            FunctionResponse(id = fc.id, name = fc.name, response = response)
        }
        val toolResponse = BidiGenerateContentToolResponse(functionResponses = responses)
        webSocket?.send(
            json.encodeToString(BidiGenerateContentToolResponse.serializer(), toolResponse)
                .wrapTopLevel("toolResponse")
        )
    }

    @SuppressLint("MissingPermission")
    private fun setupAudioRecord() {
        val minBuf = AudioRecord.getMinBufferSize(
            RECORD_SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        // Use 4x minimum to absorb scheduling jitter and prevent overruns.
        val bufferSize = (minBuf * RECORD_BUFFER_MULTIPLIER).coerceAtLeast(AUDIO_BUFFER_SIZE)
        Log.i(TAG, "AudioRecord buffer: min=$minBuf actual=$bufferSize")
        audioRecord = AudioRecord.Builder()
            .setAudioSource(MediaRecorder.AudioSource.MIC)
            .setAudioFormat(
                AudioFormat.Builder()
                    .setSampleRate(RECORD_SAMPLE_RATE)
                    .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                    .setChannelMask(AudioFormat.CHANNEL_IN_MONO)
                    .build()
            )
            .setBufferSizeInBytes(bufferSize)
            .build()
    }

    private fun setupAudioTrack() {
        val minBuf = AudioTrack.getMinBufferSize(
            PLAYBACK_SAMPLE_RATE,
            AudioFormat.CHANNEL_OUT_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        val bufferSize = (minBuf * RECORD_BUFFER_MULTIPLIER).coerceAtLeast(AUDIO_BUFFER_SIZE)
        val usage = if (audioManager.communicationDevice != null) {
            AudioAttributes.USAGE_VOICE_COMMUNICATION
        } else {
            AudioAttributes.USAGE_ASSISTANT
        }
        audioTrack = AudioTrack.Builder()
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(usage)
                    .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                    .build()
            )
            .setAudioFormat(
                AudioFormat.Builder()
                    .setSampleRate(PLAYBACK_SAMPLE_RATE)
                    .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                    .setChannelMask(AudioFormat.CHANNEL_OUT_MONO)
                    .build()
            )
            .setBufferSizeInBytes(bufferSize)
            .setTransferMode(AudioTrack.MODE_STREAM)
            .build()
        audioTrack?.play()
    }

    /** Populate available devices list and auto-select BT SCO if present. */
    private fun refreshAvailableDevices() {
        val devices = audioManager.availableCommunicationDevices.map { info ->
            AudioDevice(id = info.id, type = info.type, name = audioDeviceTypeName(info.type))
        }
        val currentSelected = _state.value.selectedDeviceId
        val autoSelect = if (currentSelected != null && devices.any { it.id == currentSelected }) {
            currentSelected
        } else {
            // Auto-select BT SCO if available, preserving legacy behavior.
            devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }?.id
        }
        _state.update { it.copy(availableDevices = devices, selectedDeviceId = autoSelect) }
        if (autoSelect != null) {
            applyCommunicationDevice(autoSelect)
        }
    }

    fun selectAudioDevice(deviceId: Int) {
        _state.update { it.copy(selectedDeviceId = deviceId) }
        applyCommunicationDevice(deviceId)
    }

    private fun applyCommunicationDevice(deviceId: Int) {
        val info = audioManager.availableCommunicationDevices.firstOrNull { it.id == deviceId }
            ?: return
        if (info.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO) {
            audioManager.mode = AudioManager.MODE_IN_COMMUNICATION
        }
        audioManager.setCommunicationDevice(info)
    }

    private fun registerDeviceCallback() {
        val cb = object : AudioDeviceCallback() {
            override fun onAudioDevicesAdded(addedDevices: Array<out AudioDeviceInfo>?) {
                refreshAvailableDevices()
            }
            override fun onAudioDevicesRemoved(removedDevices: Array<out AudioDeviceInfo>?) {
                refreshAvailableDevices()
            }
        }
        deviceCallback = cb
        audioManager.registerAudioDeviceCallback(cb, Handler(Looper.getMainLooper()))
    }

    private fun unregisterDeviceCallback() {
        deviceCallback?.let { audioManager.unregisterAudioDeviceCallback(it) }
        deviceCallback = null
    }

    private fun clearCommunicationDevice() {
        // Only reset mode if we actually changed it (i.e. BT SCO was active).
        if (audioManager.mode == AudioManager.MODE_IN_COMMUNICATION) {
            audioManager.clearCommunicationDevice()
            audioManager.mode = AudioManager.MODE_NORMAL
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: recording failures must not crash.
    private fun startRecording() {
        audioRecord?.startRecording()
        recordingJob = scope.launch(Dispatchers.IO) {
            val buffer = ByteArray(AUDIO_BUFFER_SIZE)
            while (isActive) {
                val bytesRead = try {
                    audioRecord?.read(buffer, 0, buffer.size) ?: return@launch
                } catch (e: Exception) {
                    setError(e.message ?: "Audio recording failed")
                    return@launch
                }
                if (bytesRead > 0) {
                    sendAudioChunk(buffer.copyOf(bytesRead))
                    // RMS of PCM samples (16-bit LE mono), normalized to 0..1.
                    val samples = bytesRead / 2
                    var sumSq = 0.0
                    for (i in 0 until samples) {
                        val lo = buffer[i * 2].toInt() and 0xFF
                        val hi = buffer[i * 2 + 1].toInt()
                        val sample = (hi shl 8) or lo
                        sumSq += sample.toDouble() * sample
                    }
                    val rms = (Math.sqrt(sumSq / samples) / Short.MAX_VALUE).toFloat()
                        .coerceIn(0f, 1f)
                    _state.update { it.copy(micLevel = rms) }
                }
            }
        }
    }

    private fun sendAudioChunk(pcmBytes: ByteArray) {
        val encoded = Base64.encodeToString(pcmBytes, Base64.NO_WRAP)
        val realtimeInput = BidiGenerateContentRealtimeInput(
            audio = Blob(
                mimeType = "audio/pcm;rate=$RECORD_SAMPLE_RATE",
                data = encoded,
            )
        )
        webSocket?.send(
            json.encodeToString(BidiGenerateContentRealtimeInput.serializer(), realtimeInput)
                .wrapTopLevel("realtimeInput")
        )
    }

    private fun playAudio(pcmBytes: ByteArray) {
        // AudioTrack.write() blocks until the buffer is consumed; run off-main to avoid
        // back-pressuring the WebSocket message handler.
        val track = audioTrack ?: return
        scope.launch(Dispatchers.IO) {
            track.write(pcmBytes, 0, pcmBytes.size)
        }
    }

    companion object {
        private const val SYSTEM_INSTRUCTION =
            "You are a voice assistant for caic, a system for managing AI coding agents.\n\n" +
                "## What caic does\n" +
                "caic runs coding agents (Claude Code, Gemini CLI) inside isolated containers " +
                "on a remote server. Each agent works autonomously on a git branch, writing " +
                "code, running tests, and committing changes. The user is a software engineer " +
                "who supervises multiple agents concurrently — often while away from the " +
                "screen — and controls them by voice.\n\n" +
                "## Task lifecycle\n" +
                "A task has a prompt (what to build), a repo, a branch, and a state:\n" +
                "- running: agent is actively working\n" +
                "- asking: agent needs the user to answer a question before it can continue\n" +
                "- waiting: agent is paused, waiting for user input or a message\n" +
                "- terminated: agent finished; result contains the outcome summary\n" +
                "- failed: agent crashed or was aborted; error contains the reason\n\n" +
                "## Context you have\n" +
                "At session start you receive a snapshot of all current tasks. Use it to " +
                "answer questions about task status without calling list_tasks first. Call " +
                "get_task_detail when the user asks for specifics (recent events, diffs).\n\n" +
                "## Tools available\n" +
                "create_task, list_tasks, get_task_detail, send_message, answer_question, " +
                "sync_task, terminate_task, restart_task, set_active_task.\n\n" +
                "## Behavior guidelines\n" +
                "- Be concise. The user is often away from the screen.\n" +
                "- Summarize task status: state, elapsed time, cost, what the agent is doing.\n" +
                "- When an agent is asking, read the question and options clearly, wait for " +
                "the verbal answer, then call answer_question.\n" +
                "- Confirm repo and prompt before creating a task.\n" +
                "- Refer to tasks by short name (first few words of the prompt).\n" +
                "- Proactively notify the user when tasks finish or need input.\n" +
                "- For safety issues during sync, describe each issue and ask whether to force."
    }
}

enum class TranscriptSpeaker { USER, ASSISTANT }

data class TranscriptEntry(val speaker: TranscriptSpeaker, val text: String, val final: Boolean = false)

data class AudioDevice(val id: Int, val type: Int, val name: String)

data class VoiceState(
    val connectStatus: String? = null,
    val connected: Boolean = false,
    val listening: Boolean = false,
    val speaking: Boolean = false,
    val activeTool: String? = null,
    val error: String? = null,
    val errorId: Long = 0,
    /** Conversation transcript log; each entry is one speaker turn. */
    val transcript: List<TranscriptEntry> = emptyList(),
    /** RMS mic input level, normalized 0..1. */
    val micLevel: Float = 0f,
    /** Available audio input devices. */
    val availableDevices: List<AudioDevice> = emptyList(),
    /** Currently selected audio device ID, or null for system default. */
    val selectedDeviceId: Int? = null,
)

/**
 * Append a transcription chunk to the log.
 * If the last entry is from the same speaker and not yet finalized, concatenate the new
 * chunk onto it (the API streams one word/phrase at a time per message).
 * Otherwise start a new entry.
 */
private fun List<TranscriptEntry>.appendChunk(speaker: TranscriptSpeaker, text: String): List<TranscriptEntry> =
    if (isNotEmpty() && last().speaker == speaker && !last().final) {
        dropLast(1) + TranscriptEntry(speaker, last().text + text)
    } else {
        this + TranscriptEntry(speaker, text)
    }

private fun errorJson(message: String): JsonElement =
    JsonObject(mapOf("error" to JsonPrimitive(message)))

/** Wraps a serialized JSON object as a top-level discriminated message: {"key": {...}}. */
private fun String.wrapTopLevel(key: String): String = """{"$key":$this}"""

@Suppress("CyclomaticComplexMethod") // Simple exhaustive mapping, no logic.
private fun audioDeviceTypeName(type: Int): String = when (type) {
    AudioDeviceInfo.TYPE_BLUETOOTH_SCO -> "Bluetooth"
    AudioDeviceInfo.TYPE_BLUETOOTH_A2DP -> "BT A2DP"
    AudioDeviceInfo.TYPE_BUILTIN_EARPIECE -> "Earpiece"
    AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> "Speaker"
    AudioDeviceInfo.TYPE_BUILTIN_MIC -> "Built-in Mic"
    AudioDeviceInfo.TYPE_USB_DEVICE -> "USB"
    AudioDeviceInfo.TYPE_USB_HEADSET -> "USB Headset"
    AudioDeviceInfo.TYPE_WIRED_HEADSET -> "Wired Headset"
    AudioDeviceInfo.TYPE_WIRED_HEADPHONES -> "Wired Headphones"
    else -> "Device $type"
}
