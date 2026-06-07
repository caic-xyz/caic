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
	Kind MessageKind `json:"kind"`
}

// VoiceConfig describes provider-neutral voice preferences.
type VoiceConfig struct {
	Name     string `json:"name"`
	Language string `json:"language"`
}

// SessionSetup is the client message that initializes a voice session.
type SessionSetup struct {
	Kind    MessageKind       `json:"kind"`
	Voice   VoiceConfig       `json:"voice"`
	Tools   []ToolDeclaration `json:"tools"`
	Context Context           `json:"context"`
}

// SessionClose is a client message that closes the voice session.
type SessionClose struct {
	Kind   MessageKind `json:"kind"`
	Reason string      `json:"reason,omitempty"`
}

// SessionReady is a gateway message that reports session readiness.
type SessionReady struct {
	Kind         MessageKind `json:"kind"`
	Profile      string      `json:"profile"`
	Capabilities []string    `json:"capabilities"`
}

// Context carries provider-neutral text or instruction context.
type Context struct {
	SystemInstruction string `json:"systemInstruction,omitempty"`
	Text              string `json:"text,omitempty"`
}

// ContextUpdate is a client message that appends session context.
type ContextUpdate struct {
	Kind    MessageKind `json:"kind"`
	Context Context     `json:"context"`
}

// ToolDeclaration is a provider-neutral service tool declaration.
type ToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a gateway message that asks the client to execute a tool.
type ToolCall struct {
	Kind MessageKind     `json:"kind"`
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult is a client message that returns a tool execution result.
type ToolResult struct {
	Kind   MessageKind     `json:"kind"`
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Result json.RawMessage `json:"result"`
}

// TurnCancel is a client message that cancels the current turn.
type TurnCancel struct {
	Kind   MessageKind `json:"kind"`
	Reason string      `json:"reason,omitempty"`
}

// TranscriptDelta is a gateway message that streams transcript text.
type TranscriptDelta struct {
	Kind    MessageKind `json:"kind"`
	Speaker Speaker     `json:"speaker"`
	Text    string      `json:"text"`
}

// AssistantTextDelta is a gateway message that streams assistant text.
type AssistantTextDelta struct {
	Kind MessageKind `json:"kind"`
	Text string      `json:"text"`
}

// SpeechStarted is a gateway message that reports speech output started.
type SpeechStarted struct {
	Kind    MessageKind `json:"kind"`
	Speaker Speaker     `json:"speaker"`
}

// SpeechEnded is a gateway message that reports speech output ended.
type SpeechEnded struct {
	Kind    MessageKind `json:"kind"`
	Speaker Speaker     `json:"speaker"`
}

// Interrupted is a gateway message that reports an interruption.
type Interrupted struct {
	Kind    MessageKind     `json:"kind"`
	Source  InterruptSource `json:"source"`
	Message string          `json:"message,omitempty"`
}

// Error is a gateway message that reports an error.
type Error struct {
	Kind        MessageKind `json:"kind"`
	Message     string      `json:"message"`
	Recoverable bool        `json:"recoverable"`
}
