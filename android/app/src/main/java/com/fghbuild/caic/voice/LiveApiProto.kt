// Kotlin data classes for the Gemini Live API WebSocket protocol.
// Reference: https://ai.google.dev/api/live
// All message types are represented even if not yet used, to prevent future confusion.
@file:Suppress("MatchingDeclarationName")

package com.fghbuild.caic.voice

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

/** Activity handling mode for real-time input. */
object ActivityHandling {
    const val UNSPECIFIED = "ACTIVITY_HANDLING_UNSPECIFIED"
    const val START_OF_ACTIVITY_INTERRUPTS = "START_OF_ACTIVITY_INTERRUPTS"
    const val NO_INTERRUPTION = "NO_INTERRUPTION"
}

/** How much of the input stream is included in the current turn. */
object TurnCoverage {
    const val UNSPECIFIED = "TURN_COVERAGE_UNSPECIFIED"
    const val ONLY_ACTIVITY = "TURN_INCLUDES_ONLY_ACTIVITY"
    const val ALL_INPUT = "TURN_INCLUDES_ALL_INPUT"
}

/** Sensitivity for start-of-speech detection. */
object StartSensitivity {
    const val UNSPECIFIED = "START_SENSITIVITY_UNSPECIFIED"
    const val HIGH = "START_SENSITIVITY_HIGH"
    const val LOW = "START_SENSITIVITY_LOW"
}

/** Sensitivity for end-of-speech detection. */
object EndSensitivity {
    const val UNSPECIFIED = "END_SENSITIVITY_UNSPECIFIED"
    const val HIGH = "END_SENSITIVITY_HIGH"
    const val LOW = "END_SENSITIVITY_LOW"
}

/** Behavior for NON_BLOCKING function declarations. */
object FunctionBehavior {
    const val NON_BLOCKING = "NON_BLOCKING"
}

/** Scheduling hint in function responses for NON_BLOCKING functions. */
object FunctionResponseScheduling {
    const val INTERRUPT = "INTERRUPT"
    const val WHEN_IDLE = "WHEN_IDLE"
    const val SILENT = "SILENT"
}

/** Response modalities for generation config. */
object ResponseModality {
    const val AUDIO = "AUDIO"
    const val TEXT = "TEXT"
    const val IMAGE = "IMAGE"
}

// ---------------------------------------------------------------------------
// Common types
// ---------------------------------------------------------------------------

/** Binary data container (audio, video, image chunks). */
@Serializable
data class Blob(
    val mimeType: String,
    val data: String,            // base64-encoded bytes
)

/** A single part of a Content message. Exactly one field is set. */
@Serializable
data class Part(
    val text: String? = null,
    val inlineData: Blob? = null,
    val functionCall: FunctionCall? = null,
    val functionResponse: FunctionResponse? = null,
)

/** A conversation turn (role + parts). */
@Serializable
data class Content(
    val parts: List<Part> = emptyList(),
    val role: String? = null,    // "user" | "model"
)

// ---------------------------------------------------------------------------
// Function calling types
// ---------------------------------------------------------------------------

/** A function call requested by the model. */
@Serializable
data class FunctionCall(
    val id: String,
    val name: String,
    val args: JsonObject = JsonObject(emptyMap()),
)

/**
 * Client's response to a FunctionCall.
 *
 * For NON_BLOCKING functions, include `scheduling` inside `response`:
 *   response = mapOf("result" to ..., "scheduling" to "WHEN_IDLE")
 */
@Serializable
data class FunctionResponse(
    val id: String,
    val name: String,
    val response: JsonElement,   // arbitrary struct; may include "scheduling" key
)

// ---------------------------------------------------------------------------
// Setup message types
// ---------------------------------------------------------------------------

/** Prebuilt voice selection. */
@Serializable
data class PrebuiltVoiceConfig(
    val voiceName: String,       // e.g. "ORUS", "AOEDE", "CHARON", "FENRIR", "KORE", "ZEPHYR"
)

/** Voice output configuration. */
@Serializable
data class VoiceConfig(
    val prebuiltVoiceConfig: PrebuiltVoiceConfig? = null,
)

/** Speech output parameters. */
@Serializable
data class SpeechConfig(
    val voiceConfig: VoiceConfig? = null,
)

/** Audio/text/image output modalities + speech config. */
@Serializable
data class GenerationConfig(
    val responseModalities: List<String>,
    val speechConfig: SpeechConfig? = null,
    val temperature: Double? = null,
    val topP: Double? = null,
    val topK: Int? = null,
    val maxOutputTokens: Int? = null,
    val candidateCount: Int? = null,
    val presencePenalty: Double? = null,
    val frequencyPenalty: Double? = null,
)

/** Voice Activity Detection / turn-taking settings. */
@Serializable
data class AutomaticActivityDetection(
    val disabled: Boolean? = null,
    val startOfSpeechSensitivity: String? = null,  // StartSensitivity.*
    val endOfSpeechSensitivity: String? = null,    // EndSensitivity.*
    val prefixPaddingMs: Int? = null,
    val silenceDurationMs: Int? = null,
)

/** Real-time input stream configuration. */
@Serializable
data class RealtimeInputConfig(
    val automaticActivityDetection: AutomaticActivityDetection? = null,
    val activityHandling: String? = null,          // ActivityHandling.*
    val turnCoverage: String? = null,              // TurnCoverage.*
)

/** Session resumption using a handle from a previous session. */
@Serializable
data class SessionResumptionConfig(
    val handle: String,
)

/** Context window compression to extend long sessions. */
@Serializable
data class ContextWindowCompressionConfig(
    val triggerTokens: Long? = null,
    val slidingWindow: SlidingWindow? = null,
)

/** Sliding-window strategy for context compression. */
@Serializable
data class SlidingWindow(
    val targetTokens: Long? = null,
)

/** Audio transcription config (currently no fields). */
@Serializable
data class AudioTranscriptionConfig(
    val unused: Boolean? = null,    // placeholder — type has no fields in the API
)

/** Proactivity config — allows model to proactively respond. */
@Serializable
data class ProactivityConfig(
    val proactiveAudio: Boolean? = null,
)

/** Tool containing function declarations. */
@Serializable
data class Tool(
    val functionDeclarations: List<LiveFunctionDeclaration> = emptyList(),
)

/**
 * Function declaration as sent to the Gemini Live API.
 * gemini-3.1-flash-live-preview only supports synchronous function calling;
 * the behavior field is removed.
 */
@Serializable
data class LiveFunctionDeclaration(
    val name: String,
    val description: String,
    val parameters: JsonElement,
)

/** The first client message on every session. */
@Serializable
data class BidiGenerateContentSetup(
    val model: String,
    val generationConfig: GenerationConfig? = null,
    val systemInstruction: Content? = null,
    val tools: List<Tool> = emptyList(),
    val realtimeInputConfig: RealtimeInputConfig? = null,
    val sessionResumption: SessionResumptionConfig? = null,
    val contextWindowCompression: ContextWindowCompressionConfig? = null,
    val inputAudioTranscription: AudioTranscriptionConfig? = null,
    val outputAudioTranscription: AudioTranscriptionConfig? = null,
    val proactivity: ProactivityConfig? = null,
)

// ---------------------------------------------------------------------------
// Client → Server messages (each message has exactly one top-level field)
// ---------------------------------------------------------------------------

/** Incremental conversation update sent by the client. */
@Serializable
data class BidiGenerateContentClientContent(
    val turns: List<Content> = emptyList(),
    val turnComplete: Boolean? = null,
)

/** Real-time audio/video/text stream from the user. */
@Serializable
data class BidiGenerateContentRealtimeInput(
    val audio: Blob? = null,
    val video: Blob? = null,
    val text: String? = null,
    val activityStart: ActivityStart? = null,
    val activityEnd: ActivityEnd? = null,
    val audioStreamEnd: Boolean? = null,
    @SerialName("mediaChunks") val mediaChunks: List<Blob>? = null,  // deprecated
)

/** Marks the start of user activity (manual VAD). */
@Serializable
class ActivityStart

/** Marks the end of user activity (manual VAD). */
@Serializable
class ActivityEnd

/** Client's responses to server-issued function calls. */
@Serializable
data class BidiGenerateContentToolResponse(
    val functionResponses: List<FunctionResponse> = emptyList(),
)

// ---------------------------------------------------------------------------
// Server → Client messages
// ---------------------------------------------------------------------------

/** Server acknowledges setup — no fields. */
@Serializable
class BidiGenerateContentSetupComplete

/** Model-generated incremental response. */
@Serializable
data class BidiGenerateContentServerContent(
    val modelTurn: Content? = null,
    val turnComplete: Boolean? = null,
    val interrupted: Boolean? = null,
    val generationComplete: Boolean? = null,
    val inputTranscription: BidiGenerateContentTranscription? = null,
    val outputTranscription: BidiGenerateContentTranscription? = null,
)

/** Audio/text transcription result. */
@Serializable
data class BidiGenerateContentTranscription(
    val text: String? = null,
)

/** Server requests the client to execute one or more functions. */
@Serializable
data class BidiGenerateContentToolCall(
    val functionCalls: List<FunctionCall> = emptyList(),
)

/** Server cancels previously issued tool calls (e.g. on barge-in). */
@Serializable
data class BidiGenerateContentToolCallCancellation(
    val ids: List<String> = emptyList(),
)
