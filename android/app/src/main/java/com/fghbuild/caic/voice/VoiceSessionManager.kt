// Manages Gemini Live WebSocket session, audio I/O, and function call dispatch.
package com.fghbuild.caic.voice

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioAttributes
import android.media.AudioDeviceInfo
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaRecorder
import android.util.Base64
import com.caic.sdk.ApiClient
import com.fghbuild.caic.data.SettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
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
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton

private const val RECORD_SAMPLE_RATE = 16000
private const val PLAYBACK_SAMPLE_RATE = 24000
private const val AUDIO_BUFFER_SIZE = 4096
private const val WS_CLOSE_NORMAL = 1000
private const val MODEL_NAME = "models/gemini-2.5-flash-native-audio-preview-12-2025"

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

    private val _state = MutableStateFlow(VoiceState())
    val state: StateFlow<VoiceState> = _state.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

    var onSetActiveTask: ((String) -> Unit)? = null

    fun setError(message: String) {
        _state.update { it.copy(connected = false, error = message) }
    }

    suspend fun connect() {
        val settings = settingsRepository.settings.value
        require(settings.serverURL.isNotBlank()) { "Server URL is not configured" }

        val apiClient = ApiClient(settings.serverURL)
        functionHandlers = FunctionHandlers(apiClient).also {
            it.onSetActiveTask = onSetActiveTask
        }

        val tokenResp = apiClient.getVoiceToken()
        val wsUrl = "wss://generativelanguage.googleapis.com/ws/" +
            "google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent" +
            "?access_token=${tokenResp.token}"

        val request = Request.Builder().url(wsUrl).build()
        webSocket = client.newWebSocket(request, createWebSocketListener())
    }

    fun startAudio() {
        routeToBluetoothScoIfAvailable()
        setupAudioRecord()
        setupAudioTrack()
        startRecording()
        _state.update { it.copy(listening = true) }
    }

    fun stopAudio() {
        recordingJob?.cancel()
        recordingJob = null
        audioRecord?.stop()
        audioRecord?.release()
        audioRecord = null
        audioTrack?.stop()
        audioTrack?.release()
        audioTrack = null
        clearCommunicationDevice()
        _state.update { it.copy(listening = false, speaking = false) }
    }

    fun disconnect() {
        stopAudio()
        webSocket?.close(WS_CLOSE_NORMAL, "User disconnected")
        webSocket = null
        functionHandlers = null
        _state.value = VoiceState()
    }

    fun injectText(text: String) {
        val msg = JsonObject(
            mapOf(
                "clientContent" to JsonObject(
                    mapOf(
                        "turns" to JsonArray(
                            listOf(
                                JsonObject(
                                    mapOf(
                                        "role" to JsonPrimitive("user"),
                                        "parts" to JsonArray(
                                            listOf(
                                                JsonObject(
                                                    mapOf("text" to JsonPrimitive(text))
                                                )
                                            )
                                        ),
                                    )
                                )
                            )
                        ),
                        "turnComplete" to JsonPrimitive(true),
                    )
                )
            )
        )
        webSocket?.send(json.encodeToString(JsonElement.serializer(), msg))
    }

    private fun sendSetupMessage(voiceName: String) {
        val setup = buildSetupMessage(voiceName)
        webSocket?.send(setup)
    }

    private fun buildSetupMessage(voiceName: String): String {
        val toolDecls = functionDeclarations.map { fd ->
            JsonObject(
                mapOf(
                    "name" to JsonPrimitive(fd.name),
                    "description" to JsonPrimitive(fd.description),
                    "parameters" to fd.parameters,
                )
            )
        }
        val setup = JsonObject(
            mapOf(
                "setup" to JsonObject(
                    mapOf(
                        "model" to JsonPrimitive(MODEL_NAME),
                        "generationConfig" to buildGenerationConfig(voiceName),
                        "realtimeInputConfig" to buildRealtimeInputConfig(),
                        "systemInstruction" to buildSystemInstruction(),
                        "tools" to JsonArray(
                            listOf(
                                JsonObject(
                                    mapOf(
                                        "functionDeclarations" to JsonArray(toolDecls)
                                    )
                                )
                            )
                        ),
                    )
                )
            )
        )
        return json.encodeToString(JsonElement.serializer(), setup)
    }

    private fun buildGenerationConfig(voiceName: String): JsonElement = JsonObject(
        mapOf(
            "responseModalities" to JsonArray(listOf(JsonPrimitive("AUDIO"))),
            "speechConfig" to JsonObject(
                mapOf(
                    "voiceConfig" to JsonObject(
                        mapOf(
                            "prebuiltVoiceConfig" to JsonObject(
                                mapOf("voiceName" to JsonPrimitive(voiceName))
                            )
                        )
                    )
                )
            ),
        )
    )

    private fun buildRealtimeInputConfig(): JsonElement = JsonObject(
        mapOf(
            "automaticActivityDetection" to JsonObject(
                mapOf(
                    "startOfSpeechSensitivity" to JsonPrimitive(
                        "START_SENSITIVITY_HIGH"
                    ),
                )
            ),
            "activityHandling" to JsonPrimitive("START_OF_ACTIVITY_INTERRUPTS"),
        )
    )

    private fun buildSystemInstruction(): JsonElement = JsonObject(
        mapOf(
            "parts" to JsonArray(
                listOf(
                    JsonObject(
                        mapOf("text" to JsonPrimitive(SYSTEM_INSTRUCTION))
                    )
                )
            )
        )
    )

    private fun createWebSocketListener() = object : WebSocketListener() {
        override fun onOpen(webSocket: WebSocket, response: Response) {
            val voiceName = settingsRepository.settings.value.voiceName
            sendSetupMessage(voiceName)
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            scope.launch { handleServerMessage(text) }
        }

        override fun onFailure(
            webSocket: WebSocket,
            t: Throwable,
            response: Response?,
        ) {
            _state.update { it.copy(connected = false, error = t.message) }
        }

        override fun onClosed(
            webSocket: WebSocket,
            code: Int,
            reason: String,
        ) {
            _state.update { it.copy(connected = false) }
        }
    }

    private suspend fun handleServerMessage(text: String) {
        val msg = json.decodeFromString<JsonElement>(text).jsonObject
        when {
            "setupComplete" in msg -> {
                _state.update { it.copy(connected = true, error = null) }
            }
            "serverContent" in msg -> {
                handleServerContent(msg["serverContent"]!!.jsonObject)
            }
            "toolCall" in msg -> {
                handleToolCall(msg["toolCall"]!!.jsonObject)
            }
            "toolCallCancellation" in msg -> {
                _state.update { it.copy(activeTool = null) }
            }
        }
    }

    private fun handleServerContent(content: JsonObject) {
        val parts = content["modelTurn"]?.jsonObject
            ?.get("parts")?.jsonArray
        parts?.forEach { part ->
            val inlineData = part.jsonObject["inlineData"]?.jsonObject
            if (inlineData != null) {
                val data = inlineData["data"]
                    ?.jsonPrimitive?.content ?: return@forEach
                val pcmBytes = Base64.decode(data, Base64.NO_WRAP)
                playAudio(pcmBytes)
                _state.update { it.copy(speaking = true) }
            }
        }
        val turnComplete = content["turnComplete"]
            ?.jsonPrimitive?.booleanOrNull
        if (turnComplete == true) {
            _state.update { it.copy(speaking = false) }
        }
    }

    private suspend fun handleToolCall(toolCall: JsonObject) {
        val functionCalls = toolCall["functionCalls"]?.jsonArray ?: return
        val responses = functionCalls.mapNotNull { fc ->
            val fcObj = fc.jsonObject
            val name = fcObj["name"]?.jsonPrimitive?.content ?: return@mapNotNull null
            val id = fcObj["id"]?.jsonPrimitive?.content ?: return@mapNotNull null
            val args = fcObj["args"]?.jsonObject ?: JsonObject(emptyMap())

            _state.update { it.copy(activeTool = name) }
            val result = functionHandlers?.handle(name, args) ?: errorJson("No handler")
            _state.update { it.copy(activeTool = null) }

            JsonObject(
                mapOf(
                    "id" to JsonPrimitive(id),
                    "name" to JsonPrimitive(name),
                    "response" to result,
                )
            )
        }
        val responseMsg = JsonObject(
            mapOf(
                "toolResponse" to JsonObject(
                    mapOf(
                        "functionResponses" to JsonArray(responses),
                    )
                )
            )
        )
        webSocket?.send(json.encodeToString(JsonElement.serializer(), responseMsg))
    }

    @SuppressLint("MissingPermission")
    private fun setupAudioRecord() {
        val bufferSize = AudioRecord.getMinBufferSize(
            RECORD_SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        audioRecord = AudioRecord(
            MediaRecorder.AudioSource.VOICE_COMMUNICATION,
            RECORD_SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
            bufferSize.coerceAtLeast(AUDIO_BUFFER_SIZE),
        )
    }

    private fun setupAudioTrack() {
        val bufferSize = AudioTrack.getMinBufferSize(
            PLAYBACK_SAMPLE_RATE,
            AudioFormat.CHANNEL_OUT_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
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
            .setBufferSizeInBytes(bufferSize.coerceAtLeast(AUDIO_BUFFER_SIZE))
            .setTransferMode(AudioTrack.MODE_STREAM)
            .build()
        audioTrack?.play()
    }

    /** Route audio to car hands-free (BT SCO) if connected. */
    private fun routeToBluetoothScoIfAvailable() {
        val scoDevice = audioManager.availableCommunicationDevices
            .firstOrNull { it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO }
            ?: return
        audioManager.mode = AudioManager.MODE_IN_COMMUNICATION
        audioManager.setCommunicationDevice(scoDevice)
    }

    private fun clearCommunicationDevice() {
        audioManager.clearCommunicationDevice()
        audioManager.mode = AudioManager.MODE_NORMAL
    }

    private fun startRecording() {
        audioRecord?.startRecording()
        recordingJob = scope.launch(Dispatchers.IO) {
            val buffer = ByteArray(AUDIO_BUFFER_SIZE)
            while (isActive) {
                val bytesRead = audioRecord?.read(buffer, 0, buffer.size)
                    ?: break
                if (bytesRead > 0) {
                    sendAudioChunk(buffer.copyOf(bytesRead))
                }
            }
        }
    }

    private fun sendAudioChunk(pcmBytes: ByteArray) {
        val encoded = Base64.encodeToString(pcmBytes, Base64.NO_WRAP)
        val msg = JsonObject(
            mapOf(
                "realtimeInput" to JsonObject(
                    mapOf(
                        "mediaChunks" to JsonArray(
                            listOf(
                                JsonObject(
                                    mapOf(
                                        "mimeType" to JsonPrimitive(
                                            "audio/pcm;rate=$RECORD_SAMPLE_RATE"
                                        ),
                                        "data" to JsonPrimitive(encoded),
                                    )
                                )
                            )
                        )
                    )
                )
            )
        )
        webSocket?.send(json.encodeToString(JsonElement.serializer(), msg))
    }

    private fun playAudio(pcmBytes: ByteArray) {
        audioTrack?.write(pcmBytes, 0, pcmBytes.size)
    }

    companion object {
        private const val SYSTEM_INSTRUCTION =
            "You are a voice assistant for caic, a system that manages AI coding agents " +
                "(Claude Code, Gemini CLI) running in containers. The user is a software " +
                "engineer who controls multiple concurrent coding tasks by voice.\n\n" +
                "You have tools to create tasks, send messages to agents, answer agent " +
                "questions, check task status, sync changes, terminate tasks, and restart " +
                "tasks.\n\n" +
                "Behavior guidelines:\n" +
                "- Be concise. The user is often away from the screen.\n" +
                "- Summarize task status: state, elapsed time, cost, what agent is doing.\n" +
                "- When agent asks a question, read question and options clearly. Wait for " +
                "verbal answer, then call answer_question.\n" +
                "- Confirm repo and prompt before creating a task.\n" +
                "- Refer to tasks by short name (first few words of prompt).\n" +
                "- Proactively notify when tasks finish or need input.\n" +
                "- For safety issues during sync, describe each issue and ask whether to force."
    }
}

data class VoiceState(
    val connected: Boolean = false,
    val listening: Boolean = false,
    val speaking: Boolean = false,
    val activeTool: String? = null,
    val error: String? = null,
)

private fun errorJson(message: String): JsonElement =
    JsonObject(mapOf("error" to JsonPrimitive(message)))
