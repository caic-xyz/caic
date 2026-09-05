// Provider-neutral voice gateway protocol translation for WebRTC sessions.

package voicertc

import (
	"encoding/json"
	"errors"
	"fmt"

	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
)

var errSessionClosed = errors.New("session closed")

func translateGatewayClientMessage(data []byte) ([]byte, error) {
	var env voicev1.MessageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode gateway message: %w", err)
	}
	switch env.Kind {
	case voicev1.MessageKindSessionSetup:
		var msg voicev1.SessionSetup
		if err := decodeGatewayMessage(data, env.Kind, &msg); err != nil {
			return nil, err
		}
		return buildGeminiSetup(&msg)
	case voicev1.MessageKindContextUpdate:
		var msg voicev1.ContextUpdate
		if err := decodeGatewayMessage(data, env.Kind, &msg); err != nil {
			return nil, err
		}
		return buildGeminiRealtimeText(msg.Context.Text)
	case voicev1.MessageKindUserMessage:
		var msg voicev1.UserMessage
		if err := decodeGatewayMessage(data, env.Kind, &msg); err != nil {
			return nil, err
		}
		return buildGeminiClientContentText(msg.Text)
	case voicev1.MessageKindToolResult:
		var msg voicev1.ToolResult
		if err := decodeGatewayMessage(data, env.Kind, &msg); err != nil {
			return nil, err
		}
		return buildGeminiToolResponse(&msg)
	case voicev1.MessageKindTurnCancel:
		return nil, nil
	case voicev1.MessageKindSessionClose:
		return nil, errSessionClosed
	default:
		return nil, fmt.Errorf("unsupported gateway message kind %q", env.Kind)
	}
}

func decodeGatewayMessage(data []byte, want voicev1.MessageKind, msg any) error {
	if err := json.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("decode %s message: %w", want, err)
	}
	var env voicev1.MessageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode %s envelope: %w", want, err)
	}
	if got := env.Kind; got != want {
		return fmt.Errorf("decode %s message: got kind %q", want, got)
	}
	return nil
}

func buildGeminiSetup(msg *voicev1.SessionSetup) ([]byte, error) {
	if msg.Context.SystemInstruction == "" {
		return nil, errors.New("session.setup context.systemInstruction is required")
	}
	voiceName := msg.Voice.Name
	if voiceName == "" {
		voiceName = "Orus"
	}
	decls := make([]geminiFunctionDeclaration, 0, len(msg.Tools))
	for _, tool := range msg.Tools {
		if tool.Name == "" {
			return nil, errors.New("session.setup tools contain an empty name")
		}
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if len(tool.Parameters) > 0 {
			parameters = tool.Parameters
		}
		decls = append(decls, geminiFunctionDeclaration{
			Name:                 tool.Name,
			Description:          tool.Description,
			ParametersJsonSchema: parameters,
		})
	}
	setup := geminiSetupMessage{
		Setup: geminiSetup{
			Model: geminiModelName,
			GenerationConfig: geminiGenerationConfig{
				ResponseModalities: []string{"AUDIO"},
				ThinkingConfig: &geminiThinkingConfig{
					ThinkingLevel: geminiThinkingLevelLow,
				},
				SpeechConfig: geminiSpeechConfig{
					VoiceConfig: geminiVoiceConfig{
						PrebuiltVoiceConfig: geminiPrebuiltVoiceConfig{
							VoiceName: voiceName,
						},
					},
				},
			},
			SystemInstruction: geminiContent{
				Parts: []geminiPart{{Text: msg.Context.SystemInstruction}},
			},
			Tools: []geminiTool{{
				FunctionDeclarations: decls,
			}},
			RealtimeInputConfig: geminiRealtimeInputConfig{
				ActivityHandling: "START_OF_ACTIVITY_INTERRUPTS",
			},
			InputAudioTranscription:  geminiAudioTranscriptionConfig{},
			OutputAudioTranscription: geminiAudioTranscriptionConfig{},
		},
	}
	return json.Marshal(setup)
}

func buildGeminiRealtimeText(text string) ([]byte, error) {
	if text == "" {
		return nil, errors.New("context.update context.text is required")
	}
	msg := geminiRealtimeText{}
	msg.RealtimeInput.Text = text
	return json.Marshal(msg)
}

func buildGeminiClientContentText(text string) ([]byte, error) {
	if text == "" {
		return nil, errors.New("client content text is required")
	}
	msg := geminiClientContentMessage{
		ClientContent: geminiClientContent{
			Turns:        []geminiContent{geminiTextTurn("user", text)},
			TurnComplete: true,
		},
	}
	return json.Marshal(msg)
}

func geminiTextTurn(role, text string) geminiContent {
	return geminiContent{
		Role:  role,
		Parts: []geminiPart{{Text: text}},
	}
}

func buildGeminiToolResponse(msg *voicev1.ToolResult) ([]byte, error) {
	if msg.ID == "" {
		return nil, errors.New("tool.result id is required")
	}
	if msg.Name == "" {
		return nil, errors.New("tool.result name is required")
	}
	result := msg.Result
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	resp := geminiToolResponseMessage{
		ToolResponse: geminiToolResponse{
			FunctionResponses: []geminiFunctionResponse{{
				ID:       msg.ID,
				Name:     msg.Name,
				Response: result,
			}},
		},
	}
	return json.Marshal(resp)
}

func translateGeminiServerMessage(data []byte) ([][]byte, error) {
	var msg geminiBidiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("decode provider message: %w", err)
	}
	var out [][]byte
	if msg.Error != nil {
		out = append(out, mustGatewayServerMessage(&voicev1.Error{
			Kind:        voicev1.MessageKindError,
			Message:     msg.Error.Message,
			Recoverable: false,
		}))
	}
	// session.ready is emitted by the gateway core (session.backendReady), not by
	// this provider translation. SetupComplete only signals backend readiness.
	if msg.ServerContent != nil {
		out = append(out, translateServerContent(msg.ServerContent)...)
	}
	if msg.ToolCall != nil {
		for _, call := range msg.ToolCall.FunctionCalls {
			out = append(out, mustGatewayServerMessage(&voicev1.ToolCall{
				Kind: voicev1.MessageKindToolCall,
				ID:   call.ID,
				Name: call.Name,
				Args: call.Args,
			}))
		}
	}
	if msg.ToolCallCancellation != nil {
		out = append(out, mustGatewayServerMessage(&voicev1.Interrupted{
			Kind:    voicev1.MessageKindInterrupted,
			Source:  voicev1.InterruptSourceTool,
			Message: "tool call cancelled",
		}))
	}
	return out, nil
}

func translateServerContent(content *serverContent) [][]byte {
	var out [][]byte
	if content.InputTranscription.Text != "" {
		out = append(out, mustGatewayServerMessage(&voicev1.TranscriptDelta{
			Kind:    voicev1.MessageKindTranscriptDelta,
			Speaker: voicev1.SpeakerUser,
			Text:    content.InputTranscription.Text,
		}))
	}
	if content.OutputTranscription.Text != "" {
		out = append(out,
			mustGatewayServerMessage(&voicev1.TranscriptDelta{
				Kind:    voicev1.MessageKindTranscriptDelta,
				Speaker: voicev1.SpeakerAssistant,
				Text:    content.OutputTranscription.Text,
			}),
			mustGatewayServerMessage(&voicev1.AssistantTextDelta{
				Kind: voicev1.MessageKindAssistantTextDelta,
				Text: content.OutputTranscription.Text,
			}),
		)
	}
	if content.Interrupted {
		out = append(out, mustGatewayServerMessage(&voicev1.Interrupted{
			Kind:   voicev1.MessageKindInterrupted,
			Source: voicev1.InterruptSourceUser,
		}))
	}
	if content.TurnComplete {
		out = append(out, mustGatewayServerMessage(&voicev1.SpeechEnded{
			Kind:    voicev1.MessageKindSpeechEnded,
			Speaker: voicev1.SpeakerAssistant,
		}))
	}
	return out
}

// gatewaySessionReady builds the session.ready message the gateway core emits
// once a backend is ready.
func gatewaySessionReady() []byte {
	return mustGatewayServerMessage(&voicev1.SessionReady{
		Kind: voicev1.MessageKindSessionReady,
	})
}

func mustGatewayServerMessage(msg any) []byte {
	data, err := json.Marshal(msg)
	if err != nil {
		panic("marshal gateway message: " + err.Error())
	}
	return data
}
