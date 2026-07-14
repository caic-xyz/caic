// Data-channel protocol types for voice gateway WebRTC sessions.

package v1

import "encoding/json"

// MessageKind identifies a voice gateway WebRTC data-channel message.
type MessageKind string

const (
	// MessageKindSessionSetup starts a provider-neutral voice session.
	MessageKindSessionSetup MessageKind = "session.setup"
	// MessageKindContextUpdate sends provider-neutral text context to the session.
	MessageKindContextUpdate MessageKind = "context.update"
	// MessageKindUserMessage appends completed user text and asks the assistant to respond.
	MessageKindUserMessage MessageKind = "user.message"
	// MessageKindToolResult returns the result of a tool call.
	MessageKindToolResult MessageKind = "tool.result"
	// MessageKindTurnCancel asks the gateway to cancel the current turn.
	MessageKindTurnCancel MessageKind = "turn.cancel"
	// MessageKindSessionClose asks the gateway to close the session.
	MessageKindSessionClose MessageKind = "session.close"
	// MessageKindSessionReady reports that the gateway session is ready.
	MessageKindSessionReady MessageKind = "session.ready"
	// MessageKindTranscriptDelta streams transcript text.
	MessageKindTranscriptDelta MessageKind = "transcript.delta"
	// MessageKindAssistantTextDelta streams assistant text.
	MessageKindAssistantTextDelta MessageKind = "assistant.text.delta"
	// MessageKindSpeechStarted reports that speech output started.
	MessageKindSpeechStarted MessageKind = "speech.started"
	// MessageKindSpeechEnded reports that speech output ended.
	MessageKindSpeechEnded MessageKind = "speech.ended"
	// MessageKindToolCall asks the client to execute a tool.
	MessageKindToolCall MessageKind = "tool.call"
	// MessageKindInterrupted reports an interruption.
	MessageKindInterrupted MessageKind = "interrupted"
	// MessageKindError reports a gateway error.
	MessageKindError MessageKind = "error"
)

// Speaker identifies a transcript or speech speaker.
type Speaker string

const (
	// SpeakerUser is the human speaker.
	SpeakerUser Speaker = "user"
	// SpeakerAssistant is the assistant speaker.
	SpeakerAssistant Speaker = "assistant"
)

// InterruptSource identifies why a voice gateway turn was interrupted.
type InterruptSource string

const (
	// InterruptSourceUser means user speech interrupted the assistant.
	InterruptSourceUser InterruptSource = "user"
	// InterruptSourceTool means tool execution was cancelled.
	InterruptSourceTool InterruptSource = "tool"
)

// MessageEnvelope carries the kind used to dispatch data-channel messages.
type MessageEnvelope struct {
	// Kind is the message discriminator used to decode the concrete message type.
	Kind MessageKind `json:"kind"`
}

// VoiceConfig describes provider-neutral voice preferences.
type VoiceConfig struct {
	// Name is the requested provider-specific voice name.
	Name string `json:"name"`
	// Language is the requested BCP 47 language tag, such as "en" or "fr-CA".
	Language string `json:"language"`
}

// SessionSetup is the client message that initializes a voice session.
type SessionSetup struct {
	// Kind must be "session.setup".
	Kind MessageKind `json:"kind"`
	// Voice contains the requested voice and language.
	Voice VoiceConfig `json:"voice"`
	// Tools declares the client-side tools the assistant may call.
	Tools []ToolDeclaration `json:"tools"`
	// Context provides initial instructions or text context for the session.
	Context Context `json:"context"`
}

// SessionClose is a client message that closes the voice session.
type SessionClose struct {
	// Kind must be "session.close".
	Kind MessageKind `json:"kind"`
	// Reason optionally explains why the client closed the session.
	Reason string `json:"reason,omitempty"`
}

// SessionReady is a gateway message that reports the session is ready.
type SessionReady struct {
	// Kind is "session.ready".
	Kind MessageKind `json:"kind"`
}

// Context carries provider-neutral text or instruction context.
type Context struct {
	// SystemInstruction is provider-neutral instruction text for the assistant.
	SystemInstruction string `json:"systemInstruction,omitempty"`
	// Text is additional conversational context for the session.
	Text string `json:"text,omitempty"`
}

// ContextUpdate is a client message that appends session context.
type ContextUpdate struct {
	// Kind must be "context.update".
	Kind MessageKind `json:"kind"`
	// Context contains the instructions or text to append.
	Context Context `json:"context"`
}

// UserMessage is a client message that appends completed user text and asks the assistant to respond.
type UserMessage struct {
	// Kind must be "user.message".
	Kind MessageKind `json:"kind"`
	// Text is the completed user utterance to send to the assistant.
	Text string `json:"text"`
}

// ToolDeclaration is a provider-neutral service tool declaration.
type ToolDeclaration struct {
	// Name is the tool name the assistant uses in tool.call messages.
	Name string `json:"name"`
	// Description explains when and how the assistant should use the tool.
	Description string `json:"description"`
	// Parameters is a JSON Schema object describing the tool arguments.
	Parameters json.RawMessage `json:"parameters"`
}

// ToolCall is a gateway message that asks the client to execute a tool.
type ToolCall struct {
	// Kind is "tool.call".
	Kind MessageKind `json:"kind"`
	// ID uniquely identifies this tool call for the matching tool.result.
	ID string `json:"id"`
	// Name is the requested tool name.
	Name string `json:"name"`
	// Args is the JSON argument object for the requested tool.
	Args json.RawMessage `json:"args"`
}

// ToolResult is a client message that returns a tool execution result.
type ToolResult struct {
	// Kind must be "tool.result".
	Kind MessageKind `json:"kind"`
	// ID matches the tool.call ID being answered.
	ID string `json:"id"`
	// Name is the tool name that produced the result.
	Name string `json:"name"`
	// Result is the JSON result returned to the assistant.
	Result json.RawMessage `json:"result"`
}

// TurnCancel is a client message that cancels the current turn.
type TurnCancel struct {
	// Kind must be "turn.cancel".
	Kind MessageKind `json:"kind"`
	// Reason optionally explains why the current assistant turn was cancelled.
	Reason string `json:"reason,omitempty"`
}

// TranscriptDelta is a gateway message that streams transcript text.
type TranscriptDelta struct {
	// Kind is "transcript.delta".
	Kind MessageKind `json:"kind"`
	// Speaker identifies who produced this transcript text.
	Speaker Speaker `json:"speaker"`
	// Text is an incremental transcript fragment.
	Text string `json:"text"`
}

// AssistantTextDelta is a gateway message that streams assistant text.
type AssistantTextDelta struct {
	// Kind is "assistant.text.delta".
	Kind MessageKind `json:"kind"`
	// Text is an incremental assistant text fragment.
	Text string `json:"text"`
}

// SpeechStarted is a gateway message that reports speech output started.
type SpeechStarted struct {
	// Kind is "speech.started".
	Kind MessageKind `json:"kind"`
	// Speaker identifies whose speech output started.
	Speaker Speaker `json:"speaker"`
}

// SpeechEnded is a gateway message that reports speech output ended.
type SpeechEnded struct {
	// Kind is "speech.ended".
	Kind MessageKind `json:"kind"`
	// Speaker identifies whose speech output ended.
	Speaker Speaker `json:"speaker"`
}

// Interrupted is a gateway message that reports an interruption.
type Interrupted struct {
	// Kind is "interrupted".
	Kind MessageKind `json:"kind"`
	// Source identifies what interrupted the turn.
	Source InterruptSource `json:"source"`
	// Message optionally provides more detail about the interruption.
	Message string `json:"message,omitempty"`
}

// Error is a gateway message that reports an error.
type Error struct {
	// Kind is "error".
	Kind MessageKind `json:"kind"`
	// Message is a human-readable error description.
	Message string `json:"message"`
	// Recoverable reports whether the client may continue using the session.
	Recoverable bool `json:"recoverable"`
}
