// Gemini Live backend adapter and wire types for the WebRTC voice bridge.

//go:build !race

package voicertc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/coder/websocket"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

const (
	// geminiWSEndpoint is the Gemini Live BidiGenerateContent WebSocket URL.
	geminiWSEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	// geminiModelName is the default Gemini Live model used by the first gateway backend.
	geminiModelName = "models/gemini-3.1-flash-live-preview"

	// wsReadLimit is the max WebSocket message size (16 MiB for audio chunks).
	wsReadLimit = 16 * 1024 * 1024
)

// Gemini Live type maintenance:
// 1. Choose a local checkout location for googleapis/go-genai, cloning it if
//    needed with `git clone https://github.com/googleapis/go-genai`.
// 2. Refresh that checkout with `git pull --ff-only`.
// 3. Treat live.go, types.go, and live_converters.go from that checkout as the
//    source of truth. Upstream is
//    https://github.com/googleapis/go-genai/blob/main/live.go.
// 4. For server messages, mirror the fields on LiveServerMessage and nested
//    LiveServer* types. For client payloads, mirror LiveClientMessage,
//    LiveClientSetup, LiveSendRealtimeInputParameters, and
//    LiveSendToolResponseParameters, then confirm MLDev placement in
//    live_converters.go.
// 5. Keep only provider wire DTOs here. Gateway protocol changes belong in
//    backend/internal/voicegateway/api/v1 and must be translated explicitly in
//    protocol.go.
// 6. After edits, run the focused voicertc tests and make lint.

type geminiBridgeBackend struct {
	apiKey string
}

func newGeminiBridgeBackend(apiKey string) (backendConnector, error) {
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY not configured")
	}
	return &geminiBridgeBackend{apiKey: apiKey}, nil
}

func (b *geminiBridgeBackend) connect(
	ctx context.Context,
	sessionID string,
	sink backendSink,
) (backendSession, error) {
	geminiURL := geminiWSEndpoint + "?key=" + url.QueryEscape(b.apiKey)
	wsConn, _, err := websocket.Dial(ctx, geminiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to Gemini: %w", err)
	}
	wsConn.SetReadLimit(wsReadLimit)
	sess := &geminiBridgeSession{
		id:   sessionID,
		sink: sink,
		ws:   wsConn,
	}
	go sess.rxLoop(ctx)
	return sess, nil
}

type geminiBridgeSession struct {
	id   string
	sink backendSink

	mu sync.Mutex
	ws *websocket.Conn
}

func (s *geminiBridgeSession) acceptClientMessage(ctx context.Context, data []byte) error {
	providerMsg, err := translateGatewayClientMessage(data)
	if err != nil {
		return err
	}
	if len(providerMsg) == 0 {
		return nil
	}
	s.mu.Lock()
	wsConn := s.ws
	s.mu.Unlock()
	if wsConn == nil {
		return nil
	}
	return wsConn.Write(ctx, websocket.MessageText, providerMsg)
}

func (s *geminiBridgeSession) acceptMicPCM(ctx context.Context, pcm []byte) error {
	b64 := base64.StdEncoding.EncodeToString(pcm)
	chunk := geminiAudioChunk{}
	chunk.RealtimeInput.Audio = geminiBlob{
		MimeType: fmt.Sprintf("audio/pcm;rate=%d", micSampleRate),
		Data:     b64,
	}
	msg, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("marshal audio: %w", err)
	}
	s.mu.Lock()
	wsConn := s.ws
	s.mu.Unlock()
	if wsConn == nil {
		return nil
	}
	return wsConn.Write(ctx, websocket.MessageText, msg)
}

func (s *geminiBridgeSession) close() error {
	s.mu.Lock()
	wsConn := s.ws
	s.ws = nil
	s.mu.Unlock()
	if wsConn == nil {
		return nil
	}
	return wsConn.Close(websocket.StatusNormalClosure, "session closed")
}

func (s *geminiBridgeSession) rxLoop(ctx context.Context) {
	for {
		s.mu.Lock()
		wsConn := s.ws
		s.mu.Unlock()
		if wsConn == nil {
			return
		}
		_, data, err := wsConn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "voicertc: gemini read failed", "session", s.id, "err", err)
				s.sink.sendGatewayError("Connection to Gemini lost: " + geminiCloseReason(err))
			}
			// Don't cancel here. The client receives the error and disconnects,
			// which closes the data channel and triggers gateway cleanup.
			return
		}

		if modified, ok := s.handleAudioExtraction(data); ok {
			data = modified
		}

		if geminiHasSetupComplete(data) {
			s.sink.backendReady(ctx)
		}

		clientMessages, err := translateGeminiServerMessage(data)
		if err != nil {
			slog.WarnContext(ctx, "voicertc: provider message translation failed", "session", s.id, "err", err)
			continue
		}
		for _, clientMessage := range clientMessages {
			if err := s.sink.sendGatewayMessage(ctx, clientMessage); err != nil {
				slog.WarnContext(ctx, "voicertc: provider→dc send failed", "session", s.id, "err", err)
				s.sink.cancelSession()
				return
			}
		}
	}
}

// handleAudioExtraction checks if a Gemini message contains serverContent with
// inlineData audio. If so, it appends the PCM bytes to the audio buffer for
// real-time playback, and returns a modified message with the audio stripped.
// On interrupted=true it clears the buffer immediately.
func (s *geminiBridgeSession) handleAudioExtraction(data []byte) ([]byte, bool) {
	var msg geminiBidiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false
	}
	if msg.ServerContent == nil {
		return nil, false
	}

	if msg.ServerContent.Interrupted {
		s.sink.clearAssistantAudio()
	}

	if len(msg.ServerContent.ModelTurn.Parts) == 0 {
		return nil, false
	}

	hadAudio := false
	filteredParts := make([]modelPart, 0, len(msg.ServerContent.ModelTurn.Parts))
	for _, part := range msg.ServerContent.ModelTurn.Parts {
		if part.InlineData.Data == "" {
			filteredParts = append(filteredParts, part)
			continue
		}
		hadAudio = true
		if mt := part.InlineData.MimeType; mt != "" && mt != "audio/pcm;rate=24000" {
			slog.Warn("voicertc: unexpected audio mime type", "session", s.id, "mimeType", mt)
			s.sink.sendGatewayError("Unexpected audio format from Gemini: " + mt)
			s.sink.cancelSession()
			return nil, false
		}
		pcmBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			slog.Debug("voicertc: base64 decode failed", "session", s.id, "err", err)
			continue
		}
		s.sink.addAssistantPCM(pcmBytes)
	}
	if !hadAudio {
		return nil, false
	}

	msg.ServerContent.ModelTurn.Parts = filteredParts
	rebuilt, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return rebuilt, true
}

func geminiHasSetupComplete(data []byte) bool {
	var msg geminiBidiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return false
	}
	return msg.SetupComplete != nil
}

// geminiCloseReason extracts a human-readable reason from a Gemini WebSocket
// close error, stripping the websocket library's internal error chain. Returns
// the raw error string if the error is not a close frame.
func geminiCloseReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		if ce.Reason != "" {
			return ce.Reason
		}
		return ce.Code.String()
	}
	return err.Error()
}

// geminiBidiMessage is a top-level Gemini BidiGenerateContent message.
//
// Known Live fields are typed for translation and payload construction;
// jsonutil.Overflow preserves unknown fields while stripping audio parts.
type geminiBidiMessage struct {
	jsonutil.Overflow

	SetupComplete                *geminiSetupComplete           `json:"setupComplete,omitempty"`
	ServerContent                *serverContent                 `json:"serverContent,omitempty"`
	ToolCall                     *geminiToolCall                `json:"toolCall,omitempty"`
	ToolCallCancellation         *geminiToolCallCancellation    `json:"toolCallCancellation,omitempty"`
	UsageMetadata                *geminiUsageMetadata           `json:"usageMetadata,omitempty"`
	GoAway                       *geminiGoAway                  `json:"goAway,omitempty"`
	SessionResumptionUpdate      *geminiSessionResumptionUpdate `json:"sessionResumptionUpdate,omitempty"`
	VoiceActivityDetectionSignal *geminiVoiceActivitySignal     `json:"voiceActivityDetectionSignal,omitempty"`
	VoiceActivity                *geminiVoiceActivity           `json:"voiceActivity,omitempty"`
	Error                        *geminiError                   `json:"error,omitempty"`
}

type geminiSetupComplete struct {
	// SessionID is Gemini's identifier for the established live session.
	SessionID string `json:"sessionId,omitzero"`
}

type geminiSetupMessage struct {
	Setup geminiSetup `json:"setup"`
}

// geminiSetup configures the live stream for the duration of the session.
type geminiSetup struct {
	// Model is the fully qualified Gemini model name.
	Model                    string                         `json:"model,omitzero"`
	GenerationConfig         geminiGenerationConfig         `json:"generationConfig,omitzero"`
	SystemInstruction        geminiContent                  `json:"systemInstruction,omitzero"`
	Tools                    []geminiTool                   `json:"tools,omitempty"`
	RealtimeInputConfig      geminiRealtimeInputConfig      `json:"realtimeInputConfig,omitzero"`
	SessionResumption        geminiSessionResumptionConfig  `json:"sessionResumption,omitzero"`
	ContextWindowCompression geminiContextWindowCompression `json:"contextWindowCompression,omitzero"`
	InputAudioTranscription  geminiAudioTranscriptionConfig `json:"inputAudioTranscription"`
	OutputAudioTranscription geminiAudioTranscriptionConfig `json:"outputAudioTranscription"`
	Proactivity              geminiProactivityConfig        `json:"proactivity,omitzero"`
	ExplicitVADSignal        *bool                          `json:"explicitVadSignal,omitempty"`
	AvatarConfig             geminiAvatarConfig             `json:"avatarConfig,omitzero"`
	SafetySettings           []json.RawMessage              `json:"safetySettings,omitempty"`
}

// geminiGenerationConfig carries the live subset of generation settings.
type geminiGenerationConfig struct {
	// ResponseModalities chooses which modalities Gemini may return.
	ResponseModalities []string `json:"responseModalities,omitempty"`
	Temperature        *float32 `json:"temperature,omitempty"`
	TopP               *float32 `json:"topP,omitempty"`
	TopK               *float32 `json:"topK,omitempty"`
	// MaxOutputTokens bounds generated output; Gemini chooses a default when zero.
	MaxOutputTokens int32 `json:"maxOutputTokens,omitzero"`
	// MediaResolution controls input media sampling for token usage and detail.
	MediaResolution string `json:"mediaResolution,omitzero"`
	// Seed asks Gemini to make repeated requests more reproducible.
	Seed                  *int32             `json:"seed,omitempty"`
	SpeechConfig          geminiSpeechConfig `json:"speechConfig,omitzero"`
	ThinkingConfig        json.RawMessage    `json:"thinkingConfig,omitempty"`
	EnableAffectiveDialog *bool              `json:"enableAffectiveDialog,omitempty"`
	TranslationConfig     json.RawMessage    `json:"translationConfig,omitempty"`
}

// geminiSpeechConfig controls generated speech and transcription behavior.
type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig,omitzero"`
	// LanguageCode is the speech synthesis language hint.
	LanguageCode string `json:"languageCode,omitzero"`
}

// geminiVoiceConfig selects the voice used for speech synthesis.
type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoiceConfig `json:"prebuiltVoiceConfig,omitzero"`
}

// geminiPrebuiltVoiceConfig names one of Gemini's prebuilt voices.
type geminiPrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName,omitzero"`
}

// geminiContent is a multi-part message with an optional producer role.
type geminiContent struct {
	Parts []geminiPart `json:"parts,omitempty"`
	// Role is usually user or model; Gemini defaults it when omitted.
	Role string `json:"role,omitzero"`
}

// geminiPart is one content fragment. Only the subset used by Live is modeled.
type geminiPart struct {
	Text string `json:"text,omitzero"`
}

// geminiTool exposes callable capabilities to Gemini.
type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

// geminiFunctionDeclaration describes a function Gemini can ask the client to run.
type geminiFunctionDeclaration struct {
	// Name must match the later function call and response names.
	Name string `json:"name,omitzero"`
	// Description helps Gemini decide whether and how to call the function.
	Description string `json:"description,omitzero"`
	// Parameters is Gemini's Schema form. ParametersJsonSchema is raw JSON Schema.
	Parameters json.RawMessage `json:"parameters,omitempty"`
	// ParametersJsonSchema must describe an object and is mutually exclusive with Parameters.
	ParametersJsonSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
	Response             json.RawMessage `json:"response,omitempty"`
	// ResponseJsonSchema is mutually exclusive with Response.
	ResponseJsonSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	// Behavior controls Live function-call behavior for supported modes.
	Behavior string `json:"behavior,omitzero"`
}

// geminiRealtimeInputConfig controls how realtime activity is detected and grouped.
type geminiRealtimeInputConfig struct {
	AutomaticActivityDetection geminiAutomaticActivityDetection `json:"automaticActivityDetection,omitzero"`
	// ActivityHandling defines how user activity affects model generation.
	ActivityHandling string `json:"activityHandling,omitzero"`
	// TurnCoverage defines which realtime input is included in the user's turn.
	TurnCoverage string `json:"turnCoverage,omitzero"`
}

// geminiAutomaticActivityDetection configures Gemini's server-side activity detection.
type geminiAutomaticActivityDetection struct {
	// Disabled means the client must send explicit activity markers.
	Disabled bool `json:"disabled,omitzero"`
	// StartOfSpeechSensitivity changes how readily speech begins an activity.
	StartOfSpeechSensitivity string `json:"startOfSpeechSensitivity,omitzero"`
	// EndOfSpeechSensitivity changes how readily speech is considered ended.
	EndOfSpeechSensitivity string `json:"endOfSpeechSensitivity,omitzero"`
	// PrefixPaddingMs is required speech duration before start-of-speech is committed.
	PrefixPaddingMs *int32 `json:"prefixPaddingMs,omitempty"`
	// SilenceDurationMs is required silence duration before end-of-speech is committed.
	SilenceDurationMs *int32 `json:"silenceDurationMs,omitempty"`
}

// geminiSessionResumptionConfig enables Gemini Live session resumption updates.
type geminiSessionResumptionConfig struct {
	// Handle restores a previous session when present.
	Handle string `json:"handle,omitzero"`
	// Transparent asks Gemini to report the last consumed client message index.
	Transparent bool `json:"transparent,omitzero"`
}

// geminiContextWindowCompression lets Gemini trim context when it grows too large.
type geminiContextWindowCompression struct {
	// TriggerTokens is the context size that starts compression.
	TriggerTokens *int64              `json:"triggerTokens,omitempty,string"`
	SlidingWindow geminiSlidingWindow `json:"slidingWindow,omitzero"`
}

// geminiSlidingWindow keeps a suffix of context at user-turn boundaries.
type geminiSlidingWindow struct {
	// TargetTokens is the approximate context size to keep after compression.
	TargetTokens *int64 `json:"targetTokens,omitempty,string"`
}

// geminiAudioTranscriptionConfig configures input or output audio transcription.
type geminiAudioTranscriptionConfig struct {
	// LanguageCodes are BCP-47 hints; Gemini detects language when omitted.
	LanguageCodes []string `json:"languageCodes,omitempty"`
}

// geminiProactivityConfig controls whether Gemini may stay silent for irrelevant input.
type geminiProactivityConfig struct {
	ProactiveAudio *bool `json:"proactiveAudio,omitempty"`
}

// geminiAvatarConfig configures avatar output for supported live sessions.
type geminiAvatarConfig struct {
	// AvatarName selects a prebuilt avatar.
	AvatarName       string                 `json:"avatarName,omitzero"`
	CustomizedAvatar geminiCustomizedAvatar `json:"customizedAvatar,omitzero"`
	// AudioBitrateBps controls compressed audio bitrate when avatar output is used.
	AudioBitrateBps *int32 `json:"audioBitrateBps,omitempty"`
	// VideoBitrateBps controls compressed video bitrate when avatar output is used.
	VideoBitrateBps *int32 `json:"videoBitrateBps,omitempty"`
}

// geminiCustomizedAvatar provides a reference image for avatar generation.
type geminiCustomizedAvatar struct {
	// ImageMIMEType is the MIME type for ImageData.
	ImageMIMEType string `json:"imageMimeType,omitzero"`
	// ImageData is the reference image bytes.
	ImageData []byte `json:"imageData,omitempty"`
}

// serverContent is Gemini's incremental model update for the current turn.
type serverContent struct {
	jsonutil.Overflow

	// ModelTurn contains generated content for the current conversation.
	ModelTurn modelTurn `json:"modelTurn,omitzero"`
	// TurnComplete means Gemini is done generating until more client input arrives.
	TurnComplete bool `json:"turnComplete,omitzero"`
	// Interrupted means client input stopped current model generation.
	Interrupted bool `json:"interrupted,omitzero"`
	// GroundingMetadata is returned when grounding is enabled.
	GroundingMetadata json.RawMessage `json:"groundingMetadata,omitempty"`
	// GenerationComplete means Gemini finished producing all content for the turn.
	GenerationComplete bool `json:"generationComplete,omitzero"`
	// InputTranscription is independent from model-turn ordering.
	InputTranscription transcription `json:"inputTranscription,omitzero"`
	// OutputTranscription is independent from model-turn ordering.
	OutputTranscription transcription `json:"outputTranscription,omitzero"`
	// URLContextMetadata is returned for URL context tool use.
	URLContextMetadata json.RawMessage `json:"urlContextMetadata,omitempty"`
	// TurnCompleteReason explains why Gemini completed the turn.
	TurnCompleteReason string `json:"turnCompleteReason,omitzero"`
	// WaitingForInput means Gemini is holding response generation for more input.
	WaitingForInput bool `json:"waitingForInput,omitzero"`
}

// modelTurn is generated content from Gemini.
type modelTurn struct {
	Parts []modelPart `json:"parts,omitempty"`
	Role  string      `json:"role,omitzero"`
}

// modelPart represents one generated content part.
type modelPart struct {
	// InlineData can carry audio, image, or video bytes.
	InlineData inlineData `json:"inlineData,omitzero"`
	Text       string     `json:"text,omitzero"`
}

// inlineData is base64-encoded bytes plus their MIME type.
type inlineData struct {
	MimeType string `json:"mimeType,omitzero"`
	Data     string `json:"data"`
}

// transcription is Gemini's text view of input or output audio.
type transcription struct {
	Text string `json:"text,omitzero"`
	// Finished marks the end of this transcription stream.
	Finished bool `json:"finished,omitzero"`
	// LanguageCode is the BCP-47 language code for the transcript.
	LanguageCode string `json:"languageCode,omitzero"`
}

// geminiError is a provider error sent on the Live WebSocket.
type geminiError struct {
	Message string `json:"message,omitempty"`
}

// geminiToolCall asks the client to execute one or more function calls.
type geminiToolCall struct {
	FunctionCalls []geminiFunctionCall `json:"functionCalls,omitempty"`
}

// geminiFunctionCall is a model-predicted function invocation.
type geminiFunctionCall struct {
	// ID is echoed by the matching function response when present.
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// geminiToolCallCancellation cancels pending function calls by ID.
type geminiToolCallCancellation struct {
	IDs []string `json:"ids,omitempty"`
}

type geminiToolResponseMessage struct {
	ToolResponse geminiToolResponse `json:"toolResponse"`
}

// geminiToolResponse returns client function results to Gemini.
type geminiToolResponse struct {
	FunctionResponses []geminiFunctionResponse `json:"functionResponses,omitempty"`
}

// geminiFunctionResponse is the result for one Gemini function call.
type geminiFunctionResponse struct {
	// ID matches the corresponding function call ID when Gemini supplied one.
	ID   string `json:"id,omitzero"`
	Name string `json:"name,omitzero"`
	// Response is the JSON object used as model context for the function result.
	Response json.RawMessage `json:"response,omitempty"`
	// Parts may carry ordered MIME-typed response content.
	Parts json.RawMessage `json:"parts,omitempty"`
	// WillContinue marks generator-style non-blocking function responses.
	WillContinue *bool `json:"willContinue,omitempty"`
	// Scheduling controls when non-blocking function responses re-enter the conversation.
	Scheduling string `json:"scheduling,omitzero"`
}

// geminiAudioChunk is the JSON shape for a Gemini realtime audio input chunk.
// See https://ai.google.dev/api/generate-content#mediablob
type geminiAudioChunk struct {
	RealtimeInput geminiRealtimeInput `json:"realtimeInput"`
}

type geminiRealtimeText struct {
	RealtimeInput geminiRealtimeInput `json:"realtimeInput"`
}

// geminiRealtimeInput carries continuously streamed user input.
type geminiRealtimeInput struct {
	MediaChunks []geminiBlob `json:"mediaChunks,omitempty"`
	// Audio is the realtime audio input stream.
	Audio geminiBlob `json:"audio,omitzero"`
	// AudioStreamEnd marks microphone shutdown while automatic activity detection is enabled.
	AudioStreamEnd bool `json:"audioStreamEnd,omitzero"`
	// Video is the realtime video input stream.
	Video geminiBlob `json:"video,omitzero"`
	// Text is realtime text input.
	Text string `json:"text,omitzero"`
	// ActivityStart marks user activity when automatic detection is disabled.
	ActivityStart json.RawMessage `json:"activityStart,omitempty"`
	// ActivityEnd marks the end of user activity when automatic detection is disabled.
	ActivityEnd json.RawMessage `json:"activityEnd,omitempty"`
}

// geminiBlob carries bytes for a specific media type.
type geminiBlob struct {
	MimeType string `json:"mimeType,omitzero"`
	Data     string `json:"data,omitzero"`
}

// geminiUsageMetadata reports token usage for Live responses.
type geminiUsageMetadata struct {
	// PromptTokenCount includes all prompt media and cached content.
	PromptTokenCount           int32                      `json:"promptTokenCount,omitempty"`
	CachedContentTokenCount    int32                      `json:"cachedContentTokenCount,omitempty"`
	ResponseTokenCount         int32                      `json:"responseTokenCount,omitempty"`
	ToolUsePromptTokenCount    int32                      `json:"toolUsePromptTokenCount,omitempty"`
	ThoughtsTokenCount         int32                      `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount            int32                      `json:"totalTokenCount,omitempty"`
	PromptTokensDetails        []geminiModalityTokenCount `json:"promptTokensDetails,omitempty"`
	CacheTokensDetails         []geminiModalityTokenCount `json:"cacheTokensDetails,omitempty"`
	ResponseTokensDetails      []geminiModalityTokenCount `json:"responseTokensDetails,omitempty"`
	ToolUsePromptTokensDetails []geminiModalityTokenCount `json:"toolUsePromptTokensDetails,omitempty"`
	TrafficType                string                     `json:"trafficType,omitempty"`
	ServiceTier                string                     `json:"serviceTier,omitempty"`
}

// geminiModalityTokenCount breaks token usage down by modality.
type geminiModalityTokenCount struct {
	Modality   string `json:"modality,omitzero"`
	TokenCount int32  `json:"tokenCount,omitzero"`
}

// geminiGoAway warns that Gemini will soon close the Live session.
type geminiGoAway struct {
	TimeLeft json.RawMessage `json:"timeLeft,omitempty"`
}

// geminiSessionResumptionUpdate reports resumable Live session state.
type geminiSessionResumptionUpdate struct {
	// NewHandle is the session state handle to use for resumption.
	NewHandle string `json:"newHandle,omitzero"`
	// Resumable indicates whether the current state can be resumed.
	Resumable bool `json:"resumable,omitzero"`
	// LastConsumedClientMessageIndex helps clients buffer only unsaved realtime input.
	LastConsumedClientMessageIndex int64 `json:"lastConsumedClientMessageIndex,omitempty,string"`
}

// geminiVoiceActivitySignal reports the type of VAD signal Gemini detected.
type geminiVoiceActivitySignal struct {
	VADSignalType string `json:"vadSignalType,omitzero"`
}

// geminiVoiceActivity reports Gemini's voice-activity state.
type geminiVoiceActivity struct {
	VoiceActivityType string `json:"voiceActivityType,omitzero"`
}
