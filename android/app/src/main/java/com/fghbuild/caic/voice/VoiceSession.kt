// Manages Gemini Live voice session via WebRTC, audio I/O, and function call dispatch. Keep in sync with frontend/src/VoiceSession.ts
package com.fghbuild.caic.voice

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.os.Handler
import android.os.Looper
import android.util.Log
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.VoiceRTCOfferReq
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.webrtc.DataChannel
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpReceiver
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import java.nio.ByteBuffer
import java.nio.charset.StandardCharsets
import javax.inject.Inject
import javax.inject.Singleton

private const val TAG = "VoiceSession"
private const val MODEL_NAME = "models/gemini-3.1-flash-live-preview"

@Singleton
class VoiceSession @Inject constructor(
    @param:ApplicationContext private val appContext: Context,
    private val settingsRepository: SettingsRepository,
    private val taskRepository: TaskRepository,
) {
    private val audioManager = appContext.getSystemService(AudioManager::class.java)
    private val json = Json { ignoreUnknownKeys = true }

    private var peerConnection: PeerConnection? = null
    private var dataChannel: DataChannel? = null
    private var rtcSessionID: String? = null
    private var pcFactory: PeerConnectionFactory? = null
    private var rtcAudioSource: org.webrtc.AudioSource? = null
    private var localAudioTrack: org.webrtc.AudioTrack? = null
    private var functionHandlers: FunctionHandlers? = null
    private var availableHarnesses: List<String> = emptyList()
    private var availableRepos: List<String> = emptyList()
    private var defaultHarness: String = ""
    private var defaultModel: String = ""
    private var serverCaps: ServerCaps = ServerCaps()
    private var deviceCallback: AudioDeviceCallback? = null
    private var scoReceiver: BroadcastReceiver? = null
    private var audioFocusRequest: AudioFocusRequest? = null

    private val _state = MutableStateFlow(VoiceState())
    val state: StateFlow<VoiceState> = _state.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

    val taskNumberMap = TaskNumberMap()

    /** Task IDs to exclude from AI context (e.g. already-purged tasks at session start). */
    @Volatile
    var excludedTaskIds: Set<String> = emptySet()

    /** Hot-path mute flag — avoids StateFlow read on every audio chunk. */
    @Volatile
    private var muted = false

    /** True while the model is speaking — injected text is queued and sent after the turn ends. */
    private var speakerActive = false

    /** Text notifications buffered while the model is speaking; flushed on turn end. */
    private val pendingNotifications = ArrayList<String>()

    fun setError(message: String) {
        Log.e(TAG, "setError: $message")
        abandonAudioFocus()
        VoiceService.stop(appContext)
        unregisterDeviceCallback()
        unregisterScoReceiver()
        clearCommunicationDevice()
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

    /**
     * Connect via WebRTC data channel through the caic backend bridge.
     */
    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all failures to UI.
    fun connect() {
        peerConnection?.close()
        peerConnection?.dispose()
        peerConnection = null
        clearTranscript()
        requestAudioFocus()
        VoiceService.start(appContext)
        refreshAvailableDevices()
        registerDeviceCallback()
        registerScoReceiver()
        setStatus("Setting up WebRTC…")

        scope.launch {
            try {
                val settings = settingsRepository.settings.value
                if (settings.serverURL.isBlank()) {
                    setError("Server URL is not configured")
                    return@launch
                }

                val apiClient = ApiClient(settings.serverURL, tokenProvider = { settingsRepository.settings.value.authToken })
                availableHarnesses = apiClient.listHarnesses().map { it.name }
                availableRepos = apiClient.listRepos().map { it.path }
                val config = apiClient.getConfig()
                serverCaps = ServerCaps(
                    tailscaleAvailable = config.tailscaleAvailable,
                    usbAvailable = config.usbAvailable,
                    displayAvailable = config.displayAvailable,
                    sudoAvailable = config.sudoAvailable,
                )
                val prefs = settingsRepository.serverPreferences.value
                defaultHarness = prefs?.harness?.ifBlank { null }
                    ?: availableHarnesses.firstOrNull() ?: ""
                defaultModel = prefs?.harness?.let { h -> prefs.models?.get(h) }?.ifBlank { null } ?: ""
                functionHandlers = FunctionHandlers(
                    apiClient, taskRepository, settings.serverURL, taskNumberMap,
                    { excludedTaskIds }, defaultHarness, defaultModel,
                )

                // Initialize WebRTC factory.
                if (pcFactory == null) {
                    PeerConnectionFactory.initialize(
                        PeerConnectionFactory.InitializationOptions.builder(appContext)
                            .setEnableInternalTracer(false)
                            .createInitializationOptions(),
                    )
                    pcFactory = PeerConnectionFactory.builder().createPeerConnectionFactory()
                }
                val factory = pcFactory ?: run {
                    setError("WebRTC factory init failed")
                    return@launch
                }

                val iceServers = listOf(
                    PeerConnection.IceServer.builder("stun:stun.l.google.com:19302").createIceServer()
                )
                val rtcConfig = PeerConnection.RTCConfiguration(iceServers)

                val pc = factory.createPeerConnection(rtcConfig, createPeerConnectionObserver()) ?: run {
                    setError("Failed to create PeerConnection")
                    return@launch
                }
                peerConnection = pc

                // Add local audio track (mic → RTP) so the SDP offer
                // contains an m=audio line required by the backend bridge.
                val audioConstraints = MediaConstraints().apply {
                    mandatory.add(
                        MediaConstraints.KeyValuePair("googEchoCancellation", "true"),
                    )
                    mandatory.add(
                        MediaConstraints.KeyValuePair("googAutoGainControl", "true"),
                    )
                    mandatory.add(
                        MediaConstraints.KeyValuePair("googNoiseSuppression", "true"),
                    )
                }
                val audioSrc = factory.createAudioSource(audioConstraints)
                rtcAudioSource = audioSrc
                val micTrack = factory.createAudioTrack("mic-audio", audioSrc)
                localAudioTrack = micTrack
                pc.addTrack(micTrack)

                val dcInit = DataChannel.Init().apply { ordered = true }
                val dc = pc.createDataChannel("gemini", dcInit) ?: run {
                    setError("Failed to create data channel")
                    pc.dispose()
                    return@launch
                }
                dataChannel = dc

                dc.registerObserver(object : DataChannel.Observer {
                    override fun onBufferedAmountChange(amount: Long) = Unit
                    override fun onStateChange() {
                        Log.d(TAG, "DC state: ${dc.state()}")
                        if (dc.state() == DataChannel.State.OPEN) {
                            setStatus("Waiting for server…")
                            val voiceName = settingsRepository.settings.value.voiceName
                            sendSetupMessage(voiceName)
                        }
                    }
                    override fun onMessage(buffer: DataChannel.Buffer) {
                        val data = ByteArray(buffer.data.remaining())
                        buffer.data.get(data)
                        val text = String(data, StandardCharsets.UTF_8)
                        scope.launch { handleServerMessage(text) }
                    }
                })

                // SDP offer/answer exchange.
                setStatus("Signaling…")
                pc.createOffer(object : SdpObserver {
                    override fun onCreateSuccess(desc: SessionDescription) {
                        pc.setLocalDescription(noOpSdpObserver(), desc)
                        scope.launch {
                            try {
                                val resp = apiClient.voiceRTCOffer(VoiceRTCOfferReq(sdp = desc.description))
                                rtcSessionID = resp.sessionID
                                val answer = SessionDescription(SessionDescription.Type.ANSWER, resp.sdp)
                                pc.setRemoteDescription(noOpSdpObserver(), answer)
                                Log.i(TAG, "WebRTC signaling complete, session=${resp.sessionID}")
                            } catch (e: CancellationException) {
                                throw e
                            } catch (e: Exception) {
                                setError("SDP exchange failed: ${e.message}")
                            }
                        }
                    }
                    override fun onCreateFailure(error: String) {
                        setError("Create offer failed: $error")
                    }
                    override fun onSetSuccess() = Unit
                    override fun onSetFailure(p0: String) = Unit
                }, MediaConstraints())
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                setError("WebRTC connection failed: ${e.message}")
            }
        }
    }

    @Suppress("TooManyFunctions") // PeerConnection.Observer has many required overrides.
    private fun createPeerConnectionObserver() = object : PeerConnection.Observer {
        override fun onIceCandidate(candidate: IceCandidate) = Unit
        override fun onIceConnectionChange(state: PeerConnection.IceConnectionState) {
            Log.d(TAG, "ICE state: $state")
            when (state) {
                PeerConnection.IceConnectionState.FAILED,
                PeerConnection.IceConnectionState.DISCONNECTED,
                PeerConnection.IceConnectionState.CLOSED -> setError("WebRTC ICE $state")
                else -> Unit
            }
        }
        override fun onSignalingChange(state: PeerConnection.SignalingState) = Unit
        override fun onIceConnectionReceivingChange(receiving: Boolean) = Unit
        override fun onIceGatheringChange(state: PeerConnection.IceGatheringState) = Unit
        override fun onIceCandidatesRemoved(candidates: Array<out IceCandidate>) = Unit
        override fun onAddStream(stream: MediaStream) = Unit
        override fun onRemoveStream(stream: MediaStream) = Unit
        override fun onDataChannel(dc: DataChannel) = Unit
        override fun onRenegotiationNeeded() = Unit
        override fun onAddTrack(receiver: RtpReceiver, streams: Array<out MediaStream>) {
            val track = receiver.track()
            if (track is org.webrtc.AudioTrack) {
                track.setEnabled(true)
                Log.i(TAG, "WebRTC remote audio track enabled")
            }
        }
    }

    private fun noOpSdpObserver() = object : SdpObserver {
        override fun onCreateSuccess(p0: SessionDescription) = Unit
        override fun onSetSuccess() = Unit
        override fun onCreateFailure(p0: String) {
            Log.w(TAG, "SDP failure: $p0")
        }
        override fun onSetFailure(p0: String) {
            Log.w(TAG, "SDP failure: $p0")
        }
    }

    /** Toggle microphone mute via the RTP audio track. */
    fun toggleMute() {
        muted = !muted
        _state.update { it.copy(muted = muted) }
        localAudioTrack?.setEnabled(!muted)
    }

    fun selectAudioDevice(deviceId: Int) {
        _state.update { it.copy(selectedDeviceId = deviceId) }
        applyCommunicationDevice(deviceId)
    }

    fun disconnect() {
        muted = false
        abandonAudioFocus()
        VoiceService.stop(appContext)
        unregisterDeviceCallback()
        unregisterScoReceiver()
        clearCommunicationDevice()
        dataChannel?.close()
        dataChannel?.dispose()
        dataChannel = null
        localAudioTrack?.dispose()
        localAudioTrack = null
        rtcAudioSource?.dispose()
        rtcAudioSource = null
        peerConnection?.close()
        peerConnection?.dispose()
        peerConnection = null
        rtcSessionID = null
        functionHandlers = null
        // Preserve transcript so the user can review it after disconnecting.
        _state.value = VoiceState(transcript = _state.value.transcript.map { it.copy(final = true) })
    }

    fun clearTranscript() {
        _state.update { it.copy(transcript = emptyList()) }
    }

    fun injectText(text: String) {
        if (speakerActive) {
            pendingNotifications.add(text)
            return
        }
        sendClientContent(text)
    }

    /** Send a message via the WebRTC data channel. */
    private fun send(text: String) {
        val dc = dataChannel
        if (dc != null && dc.state() == DataChannel.State.OPEN) {
            val buf = ByteBuffer.wrap(text.toByteArray(StandardCharsets.UTF_8))
            dc.send(DataChannel.Buffer(buf, false))
        }
    }

    private fun sendClientContent(text: String) {
        val clientContent = BidiGenerateContentClientContent(
            turns = listOf(
                Content(
                    role = "user",
                    parts = listOf(Part(text = text)),
                )
            ),
            turnComplete = true,
        )
        send(json.encodeToString(BidiGenerateContentClientContent.serializer(), clientContent)
            .wrapTopLevel("clientContent"))
    }

    private fun flushPendingNotifications() {
        if (pendingNotifications.isEmpty()) return
        val text = pendingNotifications.joinToString("\n")
        pendingNotifications.clear()
        sendClientContent(text)
    }

    private fun sendSetupMessage(voiceName: String) {
        val setup = buildSetupMessage(voiceName, availableHarnesses, availableRepos, serverCaps)
        Log.i(TAG, "sending setup message")
        send(setup)
    }

    private fun buildSetupMessage(
        voiceName: String, harnesses: List<String>, repos: List<String>, caps: ServerCaps,
    ): String {
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
                    functionDeclarations = buildFunctionDeclarations(
                        harnesses, repos, defaultHarness.ifBlank { null }, caps,
                    ).map { fd ->
                        LiveFunctionDeclaration(
                            name = fd.name,
                            description = fd.description,
                            parameters = fd.parameters,
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

    @Suppress("TooGenericExceptionCaught") // Error boundary: malformed messages must not crash.
    private suspend fun handleServerMessage(text: String) {
        try {
            val msg = json.decodeFromString<JsonElement>(text).jsonObject
            when {
                "setupComplete" in msg -> {
                    Log.i(TAG, "setupComplete received")
                    _state.update {
                        it.copy(
                            connectStatus = null,
                            connected = true,
                            listening = true,
                            error = null,
                        )
                    }
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
                "error" in msg -> {
                    val message = msg["error"]?.jsonObject
                        ?.get("message")?.jsonPrimitive?.content
                        ?: "Server error"
                    setError(message)
                }
                else -> {
                    Log.w(TAG, "Unrecognized server message: ${msg.keys}")
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            setError(e.message ?: "Failed to process server message")
        }
    }

    private fun handleServerContent(content: BidiGenerateContentServerContent) {
        // Audio playback is handled via WebRTC RTP; inlineData audio is ignored here.
        content.inputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.USER, text)) }
        }
        content.outputTranscription?.text?.let { text ->
            _state.update { it.copy(transcript = it.transcript.appendChunk(TranscriptSpeaker.ASSISTANT, text)) }
        }
        if (content.interrupted == true) {
            // User barged in — model stopped.
            speakerActive = false
            flushPendingNotifications()
            _state.update { it.copy(speaking = false) }
        }
        if (content.turnComplete == true) {
            // Model finished speaking.
            speakerActive = false
            flushPendingNotifications()
            _state.update {
                it.copy(
                    speaking = false,
                    transcript = it.transcript.map { e -> e.copy(final = true) },
                )
            }
        }
    }

    private suspend fun handleToolCall(toolCall: BidiGenerateContentToolCall) {
        val responses = toolCall.functionCalls.map { fc ->
            try {
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

                val response = result
                FunctionResponse(id = fc.id, name = fc.name, response = response)
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                _state.update { it.copy(activeTool = null) }
                val errMsg = e.message ?: "Unknown error"
                Log.e(TAG, "Tool ${fc.name} threw: $errMsg", e)
                _state.update {
                    it.copy(transcript = it.transcript + TranscriptEntry(
                        TranscriptSpeaker.ASSISTANT, "[${fc.name}] $errMsg", final = true,
                    ))
                }
                FunctionResponse(id = fc.id, name = fc.name, response = errorJson(errMsg))
            }
        }
        val toolResponse = BidiGenerateContentToolResponse(functionResponses = responses)
        send(
            json.encodeToString(BidiGenerateContentToolResponse.serializer(), toolResponse)
                .wrapTopLevel("toolResponse")
        )
    }

    // -----------------------------------------------------------------------
    // Audio device management (transport-agnostic)
    // -----------------------------------------------------------------------

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
        var scoWasConnected = false
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                if (intent.action != AudioManager.ACTION_SCO_AUDIO_STATE_UPDATED) return
                val state = intent.getIntExtra(
                    AudioManager.EXTRA_SCO_AUDIO_STATE, AudioManager.SCO_AUDIO_STATE_ERROR,
                )
                if (state == AudioManager.SCO_AUDIO_STATE_CONNECTED) {
                    scoWasConnected = true
                    return
                }
                if (state != AudioManager.SCO_AUDIO_STATE_DISCONNECTED) return
                // Ignore spurious disconnects fired during HFP negotiation before SCO is up.
                if (!scoWasConnected) return
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

    companion object {
        private const val SYSTEM_INSTRUCTION =
            "You are a voice assistant for caic, a system for managing AI coding agents.\n\n" +
                "## What caic does\n" +
                "caic runs coding agents (Claude Code, Codex, etc) inside isolated containers " +
                "on a remote server. Each agent works autonomously on a git branch, writing " +
                "code, running tests, and committing changes. The user is a software engineer " +
                "who supervises multiple agents concurrently — often while away from the " +
                "screen — and controls them by voice.\n\n" +
                "## Task lifecycle\n" +
                "A task has a prompt (what to build), a repo, a branch, and a state:\n" +
                "- pending: task is queued, waiting to start\n" +
                "- branching: creating git branch\n" +
                "- provisioning: starting container\n" +
                "- starting: launching agent session\n" +
                "- running: agent is actively working\n" +
                "- waiting: agent completed a turn, awaiting user input\n" +
                "- asking: agent asked a question, needs the user to answer\n" +
                "- has_plan: agent produced a plan, awaiting approval\n" +
                "- pulling: pulling changes from container\n" +
                "- pushing: pushing changes to remote\n" +
                "- purging: cleanup in progress, container being deleted\n" +
                "- purged: container deleted; result contains the outcome\n" +
                "- failed: agent crashed or was aborted; error has the reason\n\n" +
                "## Context you have\n" +
                "At session start you receive a snapshot of all current tasks. Use it to " +
                "answer questions about task status without calling tasks_list first. Call " +
                "task_get_detail when the user asks for specifics (recent events, diffs).\n\n" +
                "## On connection\n" +
                "When the session starts, say exactly one word: \"Ready\". " +
                "Do not say anything else — no greeting, no summary, no explanation. " +
                "After saying \"Ready\", stop and remain silent until the user speaks. " +
                "Always speak fast.\n\n" +
                "## Behavior guidelines\n" +
                "- Do not ask follow-up questions like 'would you like me to…' " +
                "or 'should I also…'. Answer the user's request and stop. " +
                "Only ask a question if the user's request is genuinely ambiguous " +
                "or you misunderstood something critical — then ask the single " +
                "clarifying question needed and nothing else.\n" +
                "- Be concise. The user is often away from the screen.\n" +
                "- Summarize task status: state and what the agent is doing. " +
                "Only mention elapsed time or cost when the user specifically asks.\n" +
                "- When an agent is asking, read the question and options clearly, wait for " +
                "the verbal answer, then call task_answer_question.\n" +
                "- When creating a task, use the default repo, harness, and model from the " +
                "session context unless the user specifies otherwise. " +
                "Confirm repo and prompt before creating.\n" +
                "- Refer to tasks by its title.\n" +
                "- Proactively notify the user when tasks finish or need input.\n" +
                "- Free tools: agent_last_message, tasks_list, task_get_detail, get_usage. Call them whenever useful without asking.\n" +
                "- When the user asks for a status update, call agent_last_message for each waiting/asking task to get latest output.\n" +
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
    /** Available audio input/output devices. */
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

