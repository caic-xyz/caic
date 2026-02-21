// Manages Gemini Live WebSocket session, audio I/O, and function call dispatch.
package com.fghbuild.caic.voice

import android.annotation.SuppressLint
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.util.Log
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioFocusRequest
import android.media.AudioDeviceInfo
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaRecorder
import android.media.audiofx.AcousticEchoCanceler
import android.os.Handler
import android.os.Looper
import android.net.Uri
import android.util.Base64
import com.caic.sdk.v1.ApiClient
import com.fghbuild.caic.data.SettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.asCoroutineDispatcher
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
import java.util.concurrent.Executors
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
/** Poll interval (ms) when mic is paused during model playback. */
private const val PAUSE_POLL_MS = 20L

@Singleton
class VoiceSessionManager @Inject constructor(
    @param:ApplicationContext private val appContext: Context,
    private val settingsRepository: SettingsRepository,
) {
    private val audioManager = appContext.getSystemService(AudioManager::class.java)
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
    private var scoReceiver: BroadcastReceiver? = null
    private var audioFocusRequest: AudioFocusRequest? = null

    private val _state = MutableStateFlow(VoiceState())
    val state: StateFlow<VoiceState> = _state.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

    /** Single-threaded dispatcher for AudioTrack.write() — serializes writes and prevents
     *  concurrent access that can SIGSEGV in native AudioTrack code. */
    private val playbackDispatcher = Executors.newSingleThreadExecutor().asCoroutineDispatcher()

    val taskNumberMap = TaskNumberMap()

    /** Task IDs to exclude from AI context (e.g. already-terminated tasks at session start). */
    @Volatile
    var excludedTaskIds: Set<String> = emptySet()

    /** Hot-path mute flag — avoids StateFlow read on every audio chunk. */
    @Volatile
    private var muted = false

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
                functionHandlers = FunctionHandlers(apiClient, taskNumberMap) { excludedTaskIds }

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
            requestAudioFocus()
            VoiceService.start(appContext)
            refreshAvailableDevices()
            registerDeviceCallback()
            registerScoReceiver()
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
        abandonAudioFocus()
        VoiceService.stop(appContext)
        recordingJob?.cancel()
        recordingJob = null
        unregisterDeviceCallback()
        unregisterScoReceiver()
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

    /** Toggle microphone mute. Recording continues but audio is discarded. */
    fun toggleMute() {
        muted = !muted
        _state.update { it.copy(muted = muted) }
        if (!muted) {
            // Drain stale audio buffered while muted.
            val rec = audioRecord ?: return
            val drain = ByteArray(AUDIO_BUFFER_SIZE)
            while (rec.read(drain, 0, drain.size, AudioRecord.READ_NON_BLOCKING) > 0) {
                // discard
            }
        }
    }

    fun disconnect() {
        muted = false
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
                // Pause mic while model speaks so speaker output doesn't feed back
                // into the recording as garbled input. AEC alone is not reliable.
                pauseRecording()
                playAudio(pcmBytes)
                _state.update { it.copy(speaking = true, micLevel = 0f) }
            }
        }
        content.inputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.USER, text)) }
        }
        content.outputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.ASSISTANT, text)) }
        }
        if (content.interrupted == true) {
            // User barged in — resume mic immediately.
            resumeRecording()
            _state.update { it.copy(speaking = false) }
        }
        if (content.turnComplete == true) {
            // Model finished speaking — resume mic so user can reply.
            resumeRecording()
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
            // Surface tool errors in the transcript so they're visible in the UI.
            val errorMsg = (result as? JsonObject)?.get("error")?.jsonPrimitive?.content
            if (errorMsg != null) {
                Log.e(TAG, "Tool ${fc.name} failed: $errorMsg")
                _state.update {
                    it.copy(transcript = it.transcript + TranscriptEntry(
                        TranscriptSpeaker.ASSISTANT, "[${fc.name}] $errorMsg", final = true,
                    ))
                }
            }

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
        val bufferSize = AudioRecord.getMinBufferSize(
            RECORD_SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        Log.i(TAG, "AudioRecord buffer: $bufferSize")
        // Match Firebase AI SDK: VOICE_COMMUNICATION + AcousticEchoCanceler.
        audioRecord = AudioRecord.Builder()
            .setAudioSource(MediaRecorder.AudioSource.VOICE_COMMUNICATION)
            .setAudioFormat(
                AudioFormat.Builder()
                    .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                    .setSampleRate(RECORD_SAMPLE_RATE)
                    .setChannelMask(AudioFormat.CHANNEL_IN_MONO)
                    .build()
            )
            .setBufferSizeInBytes(bufferSize)
            .build()
        audioRecord?.let { rec ->
            if (AcousticEchoCanceler.isAvailable()) {
                AcousticEchoCanceler.create(rec.audioSessionId)?.enabled = true
            }
            Log.i(TAG, "AudioRecord: rate=${rec.sampleRate} state=${rec.state}")
        }
    }

    private fun setupAudioTrack() {
        val bufferSize = AudioTrack.getMinBufferSize(
            PLAYBACK_SAMPLE_RATE,
            AudioFormat.CHANNEL_OUT_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        // USAGE_MEDIA avoids the telephony DSP reroute latency that VOICE_COMMUNICATION
        // causes on many devices (clips the first 1-2s of playback). HFP signaling is
        // handled by the AudioFocusRequest's USAGE_VOICE_COMMUNICATION instead.
        audioTrack = AudioTrack.Builder()
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(AudioAttributes.USAGE_MEDIA)
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

    /** Populate available devices list and auto-select the best device. */
    private fun refreshAvailableDevices() {
        val devices = audioManager.availableCommunicationDevices.map { info ->
            AudioDevice(id = info.id, type = info.type, name = audioDeviceTypeName(info.type))
        }
        val currentSelected = _state.value.selectedDeviceId
        val autoSelect = if (currentSelected != null && devices.any { it.id == currentSelected }) {
            currentSelected
        } else {
            // Priority: BT SCO > USB headset/device > wired headphones > built-in speaker.
            devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }?.id
                ?: devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_USB_HEADSET }?.id
                ?: devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_USB_DEVICE }?.id
                ?: devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_WIRED_HEADPHONES }?.id
                ?: devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_WIRED_HEADSET }?.id
                ?: devices.firstOrNull { it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER }?.id
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
        audioManager.setCommunicationDevice(info)
    }

    private fun registerDeviceCallback() {
        val cb = object : AudioDeviceCallback() {
            override fun onAudioDevicesAdded(addedDevices: Array<out AudioDeviceInfo>?) {
                refreshAvailableDevices()
            }
            override fun onAudioDevicesRemoved(removedDevices: Array<out AudioDeviceInfo>?) {
                val selectedId = _state.value.selectedDeviceId
                val lostBt = selectedId != null && removedDevices?.any {
                    it.id == selectedId && it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO
                } == true
                if (lostBt) {
                    Log.i(TAG, "Selected Bluetooth device removed, disconnecting")
                    disconnect()
                } else {
                    refreshAvailableDevices()
                }
            }
        }
        deviceCallback = cb
        audioManager.registerAudioDeviceCallback(cb, Handler(Looper.getMainLooper()))
    }

    private fun unregisterDeviceCallback() {
        deviceCallback?.let { audioManager.unregisterAudioDeviceCallback(it) }
        deviceCallback = null
    }

    /** Listen for SCO audio link teardown — fired when the car's HFP hang-up disconnects
     *  the audio channel without removing the BT device from the system. */
    private fun registerScoReceiver() {
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                if (intent.action != AudioManager.ACTION_SCO_AUDIO_STATE_UPDATED) return
                val state = intent.getIntExtra(
                    AudioManager.EXTRA_SCO_AUDIO_STATE, AudioManager.SCO_AUDIO_STATE_ERROR,
                )
                if (state != AudioManager.SCO_AUDIO_STATE_DISCONNECTED) return
                val selectedId = _state.value.selectedDeviceId ?: return
                val isBtSco = _state.value.availableDevices.any {
                    it.id == selectedId && it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO
                }
                if (isBtSco) {
                    Log.i(TAG, "SCO audio disconnected (HFP hang-up), disconnecting")
                    disconnect()
                }
            }
        }
        scoReceiver = receiver
        appContext.registerReceiver(
            receiver,
            IntentFilter(AudioManager.ACTION_SCO_AUDIO_STATE_UPDATED),
        )
    }

    private fun unregisterScoReceiver() {
        scoReceiver?.let { appContext.unregisterReceiver(it) }
        scoReceiver = null
    }

    private fun clearCommunicationDevice() {
        audioManager.clearCommunicationDevice()
    }

    /** Request exclusive audio focus so music/podcasts pause while the voice session is active. */
    private fun requestAudioFocus() {
        val request = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN)
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                    .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                    .build()
            )
            .setOnAudioFocusChangeListener { focusChange ->
                if (focusChange == AudioManager.AUDIOFOCUS_LOSS ||
                    focusChange == AudioManager.AUDIOFOCUS_LOSS_TRANSIENT
                ) {
                    Log.i(TAG, "Audio focus lost (change=$focusChange), disconnecting")
                    disconnect()
                }
            }
            .build()
        audioFocusRequest = request
        audioManager.requestAudioFocus(request)
    }

    private fun abandonAudioFocus() {
        audioFocusRequest?.let { audioManager.abandonAudioFocusRequest(it) }
        audioFocusRequest = null
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: recording failures must not crash.
    private fun startRecording() {
        audioRecord?.startRecording()
        recordingJob = scope.launch(Dispatchers.IO) {
            val buffer = ByteArray(AUDIO_BUFFER_SIZE)
            while (isActive) {
                val rec = audioRecord ?: return@launch
                val bytesRead = readMicChunk(rec, buffer)
                if (bytesRead < 0) return@launch
                if (bytesRead > 0) {
                    if (muted) {
                        _state.update { it.copy(micLevel = 0f) }
                    } else {
                        sendAudioChunk(buffer.copyOf(bytesRead))
                        _state.update { it.copy(micLevel = rmsLevel(buffer, bytesRead)) }
                    }
                }
            }
        }
    }

    /**
     * Read one chunk from the mic, handling the paused state.
     * Returns >0 on success, 0 when paused/skipped, -1 on fatal error.
     */
    @Suppress("TooGenericExceptionCaught") // Error boundary: recording failures must not crash.
    private fun readMicChunk(rec: AudioRecord, buffer: ByteArray): Int {
        // When paused (model speaking), poll instead of reading.
        if (rec.recordingState != AudioRecord.RECORDSTATE_RECORDING) {
            Thread.sleep(PAUSE_POLL_MS)
            return 0
        }
        return try {
            rec.read(buffer, 0, buffer.size)
        } catch (e: IllegalStateException) {
            Log.w(TAG, "AudioRecord.read() interrupted: ${e.message}")
            0
        } catch (e: Exception) {
            setError(e.message ?: "Audio recording failed")
            -1
        }
    }

    /** RMS of PCM 16-bit LE mono samples, normalized to 0..1.
     *  Applies sqrt scaling so normal speech levels produce visible indicator movement. */
    private fun rmsLevel(buffer: ByteArray, bytesRead: Int): Float {
        val samples = bytesRead / 2
        var sumSq = 0.0
        for (i in 0 until samples) {
            val lo = buffer[i * 2].toInt() and 0xFF
            val hi = buffer[i * 2 + 1].toInt()
            val sample = (hi shl 8) or lo
            sumSq += sample.toDouble() * sample
        }
        val linear = (Math.sqrt(sumSq / samples) / Short.MAX_VALUE).toFloat()
        // Sqrt compresses the range so typical speech (~0.02-0.10 linear) maps to ~0.15-0.30,
        // making the indicator visibly responsive.
        return Math.sqrt(linear.toDouble()).toFloat().coerceIn(0f, 1f)
    }

    private fun sendAudioChunk(pcmBytes: ByteArray) {
        val encoded = Base64.encodeToString(pcmBytes, Base64.NO_WRAP)
        val realtimeInput = BidiGenerateContentRealtimeInput(
            audio = Blob(
                mimeType = "audio/pcm",
                data = encoded,
            )
        )
        webSocket?.send(
            json.encodeToString(BidiGenerateContentRealtimeInput.serializer(), realtimeInput)
                .wrapTopLevel("realtimeInput")
        )
    }

    /** Pause the microphone to prevent speaker→mic echo feedback. */
    private fun pauseRecording() {
        val rec = audioRecord ?: return
        if (rec.recordingState == AudioRecord.RECORDSTATE_RECORDING) {
            rec.stop()
        }
    }

    /** Resume the microphone after model finishes speaking. */
    private fun resumeRecording() {
        val rec = audioRecord ?: return
        if (rec.recordingState == AudioRecord.RECORDSTATE_STOPPED) {
            rec.startRecording()
            // Drain any stale audio from the internal buffer so we only send
            // fresh samples recorded after the model stopped speaking.
            val drain = ByteArray(AUDIO_BUFFER_SIZE)
            while (rec.read(drain, 0, drain.size, AudioRecord.READ_NON_BLOCKING) > 0) {
                // discard
            }
        }
    }

    private fun playAudio(pcmBytes: ByteArray) {
        // AudioTrack.write() blocks until the buffer is consumed; run off-main to avoid
        // back-pressuring the WebSocket message handler. Uses a single-threaded dispatcher
        // to serialize writes — concurrent writes cause SIGSEGV in native AudioTrack code.
        scope.launch(playbackDispatcher) {
            val track = audioTrack ?: return@launch
            try {
                track.write(pcmBytes, 0, pcmBytes.size)
            } catch (_: IllegalStateException) {
                // AudioTrack was released while write was queued — harmless on disconnect.
            }
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
                "answer questions about task status without calling tasks_list first. Call " +
                "task_get_detail when the user asks for specifics (recent events, diffs).\n\n" +
                "## On connection\n" +
                "When the session starts, say only \"Ready\" and nothing else. Wait for " +
                "the user to speak first.\n\n" +
                "## Tools available\n" +
                "task_create, tasks_list, task_get_detail, task_send_message, task_answer_question, " +
                "task_push_branch_to_remote, task_terminate.\n\n" +
                "## Behavior guidelines\n" +
                "- Be concise. The user is often away from the screen.\n" +
                "- Summarize task status: state, elapsed time, cost, what the agent is doing.\n" +
                "- When an agent is asking, read the question and options clearly, wait for " +
                "the verbal answer, then call task_answer_question.\n" +
                "- When creating a task, use the default repo if one is provided in the " +
                "session context and the user doesn't specify a different one. " +
                "Confirm repo and prompt before creating.\n" +
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
    val muted: Boolean = false,
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
