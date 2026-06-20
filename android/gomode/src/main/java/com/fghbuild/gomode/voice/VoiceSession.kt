// Manages Go Mode voice gateway sessions via WebRTC, service MCP tools, and Android audio routing.
package com.fghbuild.gomode.voice

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
import android.webkit.CookieManager
import com.caic.voicegateway.sdk.v1.ContextUpdate
import com.caic.voicegateway.sdk.v1.Error
import com.caic.voicegateway.sdk.v1.MessageEnvelope
import com.caic.voicegateway.sdk.v1.MessageKind
import com.caic.voicegateway.sdk.v1.SessionSetup
import com.caic.voicegateway.sdk.v1.Speaker
import com.caic.voicegateway.sdk.v1.ToolCall
import com.caic.voicegateway.sdk.v1.ToolDeclaration
import com.caic.voicegateway.sdk.v1.ToolResult
import com.caic.voicegateway.sdk.v1.TranscriptDelta
import com.caic.voicegateway.sdk.v1.UserMessage
import com.caic.voicegateway.sdk.v1.VoiceConfig
import com.caic.voicegateway.sdk.v1.VoiceRTCOfferReq
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.service.ServiceSettingsClient
import com.fghbuild.gomode.service.serviceOrigin
import com.fghbuild.mcp.sdk.v1.ToolDescriptor
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
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
import java.net.URI
import java.nio.ByteBuffer
import java.nio.charset.StandardCharsets
import kotlin.math.sqrt

private const val TAG = "GoModeVoiceSession"
private const val MIC_LEVEL_POLL_MS = 100L

class VoiceSession(
    private val appContext: Context,
    private val settingsRepository: SettingsRepository,
    private val settingsClient: ServiceSettingsClient = ServiceSettingsClient(),
) {
    private val audioManager = appContext.getSystemService(AudioManager::class.java)
    private val json = Json {
        encodeDefaults = true
        ignoreUnknownKeys = true
    }

    private var peerConnection: PeerConnection? = null
    private var dataChannel: DataChannel? = null
    private var rtcSessionID: String? = null
    private var pcFactory: PeerConnectionFactory? = null
    private var rtcAudioSource: org.webrtc.AudioSource? = null
    private var localAudioTrack: org.webrtc.AudioTrack? = null
    private var micLevelJob: Job? = null
    private val micEnergySamples = mutableMapOf<String, MicEnergySample>()
    private var mcpClient: McpClient? = null
    private var mcpTools: List<ToolDescriptor> = emptyList()
    private var deviceCallback: AudioDeviceCallback? = null
    private var scoReceiver: BroadcastReceiver? = null
    private var audioFocusRequest: AudioFocusRequest? = null

    private val _state = MutableStateFlow(VoiceState())
    val state: StateFlow<VoiceState> = _state.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

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

    /** Connect via WebRTC data channel through the configured service voice gateway. */
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
                if (settings.activeServiceURL.isBlank()) {
                    setError("Service URL is not configured")
                    return@launch
                }
                val serviceSettings = settingsClient.fetch(settings.activeServiceURL)
                val voiceGatewayURL = serviceSettings.webShell.voiceGateway.url
                if (voiceGatewayURL.isNullOrBlank()) {
                    setError("Voice is not available for this service")
                    return@launch
                }
                // Single active skill today. SKILL.md frontmatter activation across
                // the toolGroups catalog (progressive disclosure) is future work;
                // see gomode/docs/ANDROID_SHELL.md.
                val group = serviceSettings.webShell.toolGroups.firstOrNull()
                if (group == null) {
                    setError("Voice is not available for this service")
                    return@launch
                }
                val mcpEndpointURL = resolveServiceURL(settings.activeServiceURL, group.endpoint)
                val voiceGatewayEndpointURL = resolveServiceURL(settings.activeServiceURL, voiceGatewayURL)
                if (group.authRequired && cookieFor(mcpEndpointURL).isNullOrBlank()) {
                    setError("Sign in to the hosted service before using voice")
                    return@launch
                }
                if (serviceSettings.webShell.voiceGateway.authRequired == true &&
                    cookieFor(voiceGatewayEndpointURL).isNullOrBlank()
                ) {
                    setError("Sign in to the hosted service before using voice")
                    return@launch
                }

                val client = McpClient(
                    endpointURL = mcpEndpointURL,
                    protocolVersion = group.protocolVersion,
                    cookieProvider = { cookieFor(mcpEndpointURL) },
                )
                mcpClient = client
                val systemInstruction = client.serverInstructions().ifBlank { FALLBACK_SYSTEM_INSTRUCTION }
                mcpTools = client.listTools()
                val voiceGatewayClient =
                    com.caic.voicegateway.sdk.v1.ApiClient(voiceGatewayEndpointURL)
                val voiceGatewayHeaders = cookieHeaders(voiceGatewayEndpointURL)

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
                startMicLevelMonitoring()

                val dcInit = DataChannel.Init().apply { ordered = true }
                val dc = pc.createDataChannel("voice-gateway", dcInit) ?: run {
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
                            sendSetupMessage(systemInstruction)
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
                                val resp = voiceGatewayClient.voiceRTCOffer(
                                    VoiceRTCOfferReq(sdp = desc.description),
                                    headers = voiceGatewayHeaders,
                                )
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

    private fun startMicLevelMonitoring() {
        micLevelJob?.cancel()
        micEnergySamples.clear()
        micLevelJob = scope.launch {
            while (true) {
                val pc = peerConnection ?: break
                if (muted) {
                    _state.update { it.copy(micLevel = 0f) }
                } else {
                    pc.getStats { report ->
                        val level = micLevelFromStats(report) ?: return@getStats
                        _state.update { it.copy(micLevel = level) }
                    }
                }
                delay(MIC_LEVEL_POLL_MS)
            }
        }
    }

    private fun stopMicLevelMonitoring() {
        micLevelJob?.cancel()
        micLevelJob = null
        micEnergySamples.clear()
    }

    private fun micLevelFromStats(report: org.webrtc.RTCStatsReport): Float? {
        var maxLevel: Float? = null
        report.statsMap.forEach { (id, stat) ->
            if (!isLocalAudioStat(stat)) return@forEach
            val directLevel = (stat.members["audioLevel"] as? Number)?.toFloat()
            if (directLevel != null) {
                maxLevel = maxOf(maxLevel ?: 0f, directLevel.coerceIn(0f, 1f))
                return@forEach
            }
            val energy = (stat.members["totalAudioEnergy"] as? Number)?.toDouble() ?: return@forEach
            val duration = (stat.members["totalSamplesDuration"] as? Number)?.toDouble() ?: return@forEach
            val previous = micEnergySamples.put(id, MicEnergySample(energy, duration)) ?: return@forEach
            val energyDelta = energy - previous.energy
            val durationDelta = duration - previous.duration
            if (energyDelta <= 0.0 || durationDelta <= 0.0) return@forEach
            val rms = sqrt(energyDelta / durationDelta).toFloat().coerceIn(0f, 1f)
            maxLevel = maxOf(maxLevel ?: 0f, rms)
        }
        return maxLevel
    }

    private fun isLocalAudioStat(stat: org.webrtc.RTCStats): Boolean {
        val type = stat.type.lowercase()
        if ("inbound" in type || type.startsWith("remote-")) return false
        val kind = stat.members["kind"] as? String ?: stat.members["mediaType"] as? String
        return kind == null || kind == "audio"
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
        _state.update { it.copy(muted = muted, micLevel = if (muted) 0f else it.micLevel) }
        localAudioTrack?.setEnabled(!muted)
    }

    fun selectAudioDevice(deviceId: Int) {
        _state.update { it.copy(selectedDeviceId = deviceId) }
        applyCommunicationDevice(deviceId)
    }

    fun disconnect() {
        muted = false
        stopMicLevelMonitoring()
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
        mcpClient = null
        mcpTools = emptyList()
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
        send(json.encodeToString(ContextUpdate.serializer(), gatewayContextUpdate(text)))
    }

    private fun sendUserMessage(text: String) {
        send(json.encodeToString(UserMessage.serializer(), gatewayUserMessage(text)))
    }

    private fun flushPendingNotifications() {
        if (pendingNotifications.isEmpty()) return
        val text = pendingNotifications.joinToString("\n")
        pendingNotifications.clear()
        sendClientContent(text)
    }

    private fun sendSetupMessage(systemInstruction: String) {
        val tools = mcpTools.map { d ->
            ToolDeclaration(
                name = d.name,
                description = d.description.orEmpty(),
                parameters = d.inputSchema as? JsonObject ?: JsonObject(emptyMap()),
            )
        }
        val setup = gatewaySessionSetup(tools, systemInstruction)
        Log.i(TAG, "sending setup message")
        send(json.encodeToString(SessionSetup.serializer(), setup))
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: malformed messages must not crash.
    private suspend fun handleServerMessage(text: String) {
        try {
            val env = json.decodeFromString(MessageEnvelope.serializer(), text)
            when (env.kind) {
                MessageKind.SessionReady -> {
                    Log.i(TAG, "session.ready received")
                    _state.update {
                        it.copy(
                            connectStatus = null,
                            connected = true,
                            listening = true,
                            error = null,
                        )
                    }
                    sendUserMessage("Say exactly one word: Ready")
                }
                MessageKind.TranscriptDelta -> {
                    handleTranscriptDelta(json.decodeFromString(TranscriptDelta.serializer(), text))
                }
                MessageKind.SpeechStarted -> {
                    speakerActive = true
                    _state.update { it.copy(speaking = true) }
                }
                MessageKind.SpeechEnded -> {
                    speakerActive = false
                    flushPendingNotifications()
                    _state.update {
                        it.copy(
                            speaking = false,
                            transcript = it.transcript.map { e -> e.copy(final = true) },
                        )
                    }
                }
                MessageKind.Interrupted -> {
                    speakerActive = false
                    flushPendingNotifications()
                    _state.update { it.copy(speaking = false, activeTool = null) }
                }
                MessageKind.ToolCall -> {
                    handleToolCall(json.decodeFromString(ToolCall.serializer(), text))
                }
                MessageKind.Error -> {
                    val msg = json.decodeFromString(Error.serializer(), text)
                    setError(msg.message)
                }
                else -> {
                    Log.w(TAG, "Unrecognized server message: ${env.kind}")
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            setError(e.message ?: "Failed to process server message")
        }
    }

    private fun handleTranscriptDelta(msg: TranscriptDelta) {
        val speaker = when (msg.speaker) {
            Speaker.User -> TranscriptSpeaker.USER
            Speaker.Assistant -> TranscriptSpeaker.ASSISTANT
            else -> return
        }
        val chunk = msg.text ?: return
        _state.update { it.copy(transcript = it.transcript.appendChunk(speaker, chunk)) }
    }

    private suspend fun handleToolCall(msg: ToolCall) {
        val id = msg.id
        val name = msg.name
        try {
            _state.update { it.copy(activeTool = name) }
            val args = msg.args as? JsonObject ?: JsonObject(emptyMap())
            val client = mcpClient ?: run {
                _state.update { it.copy(activeTool = null) }
                sendToolResult(id, name, errorJson("No MCP client"))
                return
            }
            val result = client.callTool(name, args)
            _state.update { it.copy(activeTool = null) }
            if (result.isError) {
                val errMsg = result.structuredContent["error"]?.jsonPrimitive?.content ?: "Tool error"
                Log.e(TAG, "Tool $name failed: $errMsg")
                _state.update {
                    it.copy(transcript = it.transcript + TranscriptEntry(
                        TranscriptSpeaker.ASSISTANT, "[$name] $errMsg", final = true,
                    ))
                }
            }
            sendToolResult(id, name, result.structuredContent)
        } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
            _state.update { it.copy(activeTool = null) }
            val errMsg = e.message ?: "Unknown error"
            Log.e(TAG, "Tool $name threw: $errMsg", e)
            _state.update {
                it.copy(transcript = it.transcript + TranscriptEntry(
                    TranscriptSpeaker.ASSISTANT, "[$name] $errMsg", final = true,
                ))
            }
            sendToolResult(id, name, errorJson(errMsg))
        }
    }

    private fun sendToolResult(id: String, name: String, result: JsonElement) {
        send(json.encodeToString(ToolResult.serializer(), gatewayToolResult(id, name, result)))
    }

    private fun gatewayContextUpdate(text: String) = ContextUpdate(
        kind = MessageKind.ContextUpdate,
        context = com.caic.voicegateway.sdk.v1.Context(text = text),
    )

    private fun gatewayUserMessage(text: String) = UserMessage(
        kind = MessageKind.UserMessage,
        text = text,
    )

    private fun gatewaySessionSetup(
        tools: List<ToolDeclaration>,
        systemInstruction: String,
    ) = SessionSetup(
        kind = MessageKind.SessionSetup,
        voice = VoiceConfig(
            name = "Orus",
            language = "en",
        ),
        tools = tools,
        context = com.caic.voicegateway.sdk.v1.Context(systemInstruction = systemInstruction),
    )

    private fun gatewayToolResult(id: String, name: String, result: JsonElement) = ToolResult(
        kind = MessageKind.ToolResult,
        id = id,
        name = name,
        result = result,
    )

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

    private fun cookieHeaders(url: String): Map<String, String> =
        cookieFor(url)?.takeIf { it.isNotBlank() }?.let { mapOf("Cookie" to it) } ?: emptyMap()

    private fun cookieFor(url: String): String? = CookieManager.getInstance().getCookie(url)

    companion object {
        private const val FALLBACK_SYSTEM_INSTRUCTION =
            "You are a concise voice assistant for a Go Mode service running in an Android shell. " +
                "Use the service MCP tools whenever they are useful. Always speak fast and keep answers short."

        fun resolveServiceURL(baseURL: String, advertisedURL: String): String {
            val baseOrigin = URI(serviceOrigin(baseURL).trimEnd('/') + "/")
            return baseOrigin.resolve(advertisedURL).toString().trimEnd('/')
        }
    }
}

enum class TranscriptSpeaker { USER, ASSISTANT }

data class TranscriptEntry(val speaker: TranscriptSpeaker, val text: String, val final: Boolean = false)

data class AudioDevice(val id: Int, val type: Int, val name: String)

private data class MicEnergySample(val energy: Double, val duration: Double)

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
