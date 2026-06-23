// Smoke test for managed local-stack ASR, LLM, and TTS voice sessions.

// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build smoke

package voicertc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers/llamacpp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
)

// TestSmokeVoiceRTCLocalAudio verifies the managed local-stack ASR, LLM, and
// TTS paths with direct queries, managed tool calls, and WebRTC turns.
func TestSmokeVoiceRTCLocalAudio(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(runtimeCancel)
	root := t

	var spokenPCM24 []byte
	var tts *kittenTTSAdapter
	var asr *genaiASRAdapter
	var llm *genaiLLMAdapter

	t.Run("Audio", func(t *testing.T) {

		t.Run("TTS", func(t *testing.T) {
			startup := time.Now()
			adapter, err := newKittenTTSAdapter(runtimeCtx)
			t.Logf("KittenTTS startup setup time, excluded from latency measurements: %s", smokeElapsed(startup))
			if err != nil {
				t.Fatal(err)
			}
			tts = adapter
			root.Cleanup(func() {
				if err := adapter.Close(); err != nil {
					root.Fatal(err)
				}
			})

			const smokeSentence = "I love bananas"
			chunks, firstAudioLatency, synthesisLatency, err := collectTimedTTSChunks(t.Context(), adapter, smokeSentence)
			t.Logf("KittenTTS first-audio latency: %s", firstAudioLatency)
			t.Logf("KittenTTS synthesis latency: %s", synthesisLatency)
			if err != nil {
				t.Fatal(err)
			}
			pcm := bytes.Join(chunks, nil)
			if len(pcm) < backendOutputSampleRate {
				t.Fatalf("TTS PCM length = %d bytes, want at least %d", len(pcm), backendOutputSampleRate)
			}
			if energy := pcmS16LEEnergy(pcm); energy < 1_000 {
				t.Fatalf("TTS PCM energy = %.0f, want audible signal", energy)
			}
			spokenPCM24 = pcm
		})

		t.Run("ASR", func(t *testing.T) {
			if len(spokenPCM24) == 0 {
				t.Fatal("TTS did not produce ASR input")
			}
			startup := time.Now()
			endpoint, err := localStackLlamaEndpoint(runtimeCtx, "", "", defaultLocalStackASRModel, startManagedLlamaServer)
			t.Logf("managed Qwen3-ASR startup setup time, excluded from latency measurements: %s", smokeElapsed(startup))
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.runtime != nil {
				root.Cleanup(func() {
					if err := endpoint.runtime.Close(); err != nil {
						root.Fatal(err)
					}
				})
			}
			asr = &genaiASRAdapter{provider: endpoint.provider}

			transcription := time.Now()
			text, err := asr.transcribe(t.Context(), downsample24to16(spokenPCM24))
			t.Logf("Qwen3-ASR transcription latency: %s", smokeElapsed(transcription))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(text) == "" {
				t.Fatal("ASR returned empty transcript")
			}
			normalized := strings.ToLower(text)
			normalized = strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' {
					return r
				}
				return ' '
			}, normalized)
			normalized = strings.Join(strings.Fields(normalized), " ")
			if !strings.Contains(normalized, "i love bananas") {
				t.Fatalf("ASR transcript = %q, want %q", text, "I love bananas")
			}
			t.Logf("ASR transcript: %s", strings.TrimSpace(text))
		})
	})

	t.Run("LLM", func(t *testing.T) {
		startup := time.Now()
		endpoint, err := localStackLlamaEndpoint(runtimeCtx, "", "", defaultLocalStackLLMModel, startManagedLlamaServer)
		t.Logf("managed Gemma startup setup time, excluded from latency measurements: %s", smokeElapsed(startup))
		if err != nil {
			t.Fatal(err)
		}
		if endpoint.runtime != nil {
			root.Cleanup(func() {
				if err := endpoint.runtime.Close(); err != nil {
					root.Fatal(err)
				}
			})
		}
		llm = &genaiLLMAdapter{provider: endpoint.provider}

		conv := llm.newConversation(
			"Answer with a short plain text sentence.",
			nil,
		)
		firstReply := time.Now()
		step, err := conv.user(t.Context(), "Reply with the word banana.")
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for delta := range step.text {
			text.WriteString(delta)
		}
		reply, err := step.finish()
		t.Logf("Gemma first text reply latency: %s", smokeElapsed(firstReply))
		if err != nil {
			t.Fatal(err)
		}
		if text.Len() == 0 {
			text.WriteString(reply.text)
		}
		replyText := strings.TrimSpace(text.String())
		if replyText == "" {
			t.Fatal("llama.cpp query returned empty text")
		}
		if !strings.Contains(strings.ToLower(replyText), "banana") {
			t.Fatalf("llama.cpp reply = %q, want banana", replyText)
		}
		t.Logf("llama.cpp reply: %s", replyText)

		verifyManagedLLMToolCall(t, endpoint.provider)

		s := newVoiceRTCTestSession(t.Context(), t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			fixedASR{text: "hello"}, llm, placeholderTTS{},
		))
		turn := time.Now()
		writeLocalMicAudio(t.Context(), t, s.micTrack)
		data := waitForVoiceRTCMessage(t.Context(), t, s.messages, s.signalErrs, voicev1.MessageKindAssistantTextDelta)
		t.Logf("WebRTC local turn to assistant text latency: %s", smokeElapsed(turn))
		var msg voicev1.AssistantTextDelta
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(msg.Text) == "" {
			t.Fatal("assistant text is empty")
		}
	})

	t.Run("FullWebRTC", func(t *testing.T) {
		if len(spokenPCM24) == 0 {
			t.Fatal("missing TTS-generated microphone input")
		}
		if asr == nil {
			t.Fatal("missing managed ASR adapter")
		}
		if llm == nil {
			t.Fatal("missing managed LLM adapter")
		}
		if tts == nil {
			t.Fatal("missing KittenTTS adapter")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)
		s := newVoiceRTCTestSession(ctx, t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			asr, llm, tts,
		))
		turn := time.Now()
		writePCM24MicAudio(ctx, t, s.micTrack, spokenPCM24)
		data := waitForVoiceRTCMessage(ctx, t, s.messages, s.signalErrs, voicev1.MessageKindTranscriptDelta)
		var transcript voicev1.TranscriptDelta
		if err := json.Unmarshal(data, &transcript); err != nil {
			t.Fatal(err)
		}
		if transcript.Speaker != voicev1.SpeakerUser || strings.TrimSpace(transcript.Text) == "" {
			t.Fatalf("user transcript = %+v, want non-empty user transcript", transcript)
		}
		t.Logf("full WebRTC user transcript: %s", strings.TrimSpace(transcript.Text))

		data = waitForVoiceRTCMessage(ctx, t, s.messages, s.signalErrs, voicev1.MessageKindAssistantTextDelta)
		var assistant voicev1.AssistantTextDelta
		if err := json.Unmarshal(data, &assistant); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(assistant.Text) == "" {
			t.Fatal("full WebRTC assistant text is empty")
		}
		t.Logf("full WebRTC assistant text: %s", strings.TrimSpace(assistant.Text))

		select {
		case energy := <-s.remoteAudioEnergy:
			if energy < 1_000 {
				t.Fatalf("full WebRTC assistant RTP audio energy = %.0f, want audible signal", energy)
			}
			t.Logf("full WebRTC assistant RTP audio energy: %.0f", energy)
		case err := <-s.mediaErrs:
			t.Fatal(err)
		case err := <-s.signalErrs:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		t.Logf("full WebRTC turn to audible RTP latency: %s", smokeElapsed(turn))
	})
}

func smokeElapsed(start time.Time) time.Duration {
	return time.Since(start).Round(time.Millisecond)
}

func genStreamResult(ctx context.Context, provider genai.Provider, messages genai.Messages, options ...genai.GenOption) (genai.Result, error) {
	fragments, finish := provider.GenStream(ctx, messages, options...)
	for range fragments {
	}
	return finish()
}

func verifyManagedLLMToolCall(t *testing.T, provider genai.Provider) {
	t.Run("ToolCall", func(t *testing.T) {
		tools, err := genaiToolDefs([]voicev1.ToolDeclaration{{
			Name:        "tasks_list",
			Description: "List current tasks.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":5}},"required":["limit"],"additionalProperties":false}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
		toolOptions := &genai.GenOptionTools{Tools: tools, Force: genai.ToolCallRequired}
		options := []genai.GenOption{
			&genai.GenOptionText{SystemPrompt: "Use tools when required, then answer briefly after tool results."},
			&llamacpp.GenOption{},
			toolOptions,
		}
		messages := genai.Messages{genai.NewTextMessage("Call tasks_list now with limit 1.")}

		toolCall := time.Now()
		res, err := genStreamResult(t.Context(), provider, messages, options...)
		t.Logf("Gemma forced tool-call latency: %s", smokeElapsed(toolCall))
		if err != nil {
			t.Fatal(err)
		}
		call, ok := firstGenAIToolCall(&res.Message)
		if !ok {
			t.Fatalf("Gemma response = %#v, want tool call", res.Message)
		}
		if call.Name != "tasks_list" {
			t.Fatalf("tool call name = %q, want tasks_list", call.Name)
		}
		if call.ID == "" {
			call.ID = "smoke-tool-call-1"
		}
		if call.Arguments == "" {
			call.Arguments = "{}"
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			t.Fatalf("tool call arguments = %q: %v", call.Arguments, err)
		}
		if _, ok := args["limit"]; !ok {
			t.Fatalf("tool call arguments = %s, want limit", call.Arguments)
		}
		t.Logf("Gemma tool call: name=%s args=%s", call.Name, call.Arguments)

		messages = append(messages, res.Message, genai.Message{ToolCallResults: []genai.ToolCallResult{{
			ID:     call.ID,
			Name:   call.Name,
			Result: `{"tasks":[{"id":"smoke","title":"smoke task"}]}`,
		}}})
		toolOptions.Force = genai.ToolCallAny
		toolResult := time.Now()
		followup, err := genStreamResult(t.Context(), provider, messages, options...)
		t.Logf("Gemma after tool-result latency: %s", smokeElapsed(toolResult))
		if err != nil {
			t.Fatal(err)
		}
		if call, ok := firstGenAIToolCall(&followup.Message); ok {
			t.Fatalf("follow-up tool call = %+v, want text", call)
		}
		text := strings.TrimSpace(genAIText(&followup.Message))
		if text == "" {
			t.Fatalf("follow-up response = %#v, want text", followup.Message)
		}
		t.Logf("Gemma tool result reply: %s", text)
	})
}

func firstGenAIToolCall(msg *genai.Message) (*genai.ToolCall, bool) {
	for i := range msg.Replies {
		if !msg.Replies[i].ToolCall.IsZero() {
			return &msg.Replies[i].ToolCall, true
		}
	}
	return nil, false
}

func genAIText(msg *genai.Message) string {
	text := strings.Builder{}
	for i := range msg.Replies {
		text.WriteString(msg.Replies[i].Text)
	}
	return text.String()
}

func collectTimedTTSChunks(ctx context.Context, tts *kittenTTSAdapter, text string) ([][]byte, time.Duration, time.Duration, error) {
	start := time.Now()
	var firstChunkLatency time.Duration
	var chunks [][]byte
	for pcm, err := range tts.synthesize(ctx, text) {
		if err != nil {
			return nil, firstChunkLatency, time.Since(start).Round(time.Millisecond), err
		}
		if len(chunks) == 0 {
			firstChunkLatency = time.Since(start).Round(time.Millisecond)
		}
		chunks = append(chunks, pcm)
	}
	return chunks, firstChunkLatency, time.Since(start).Round(time.Millisecond), nil
}

func writePCM24MicAudio(ctx context.Context, t *testing.T, track *webrtc.TrackLocalStaticSample, pcm []byte) {
	enc, err := newEncoder()
	if err != nil {
		t.Fatal(err)
	}
	pcm48 := upsample24PCMTo48Samples(pcm, vadSilenceHangoverMS+200)
	for off := 0; off < len(pcm48); off += encoderFrameSamples {
		frame := make([]int16, encoderFrameSamples)
		copy(frame, pcm48[off:min(off+encoderFrameSamples, len(pcm48))])
		pkt, err := enc.Encode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if err := track.WriteSample(media.Sample{Data: pkt, Duration: frameDuration}); err != nil {
			t.Fatal(err)
		}
		if !sleepCtx(ctx, frameDuration) {
			t.Fatal(ctx.Err())
		}
	}
}

func upsample24PCMTo48Samples(pcm []byte, trailingSilenceMS int) []int16 {
	samples24 := len(pcm) / 2
	trailingSilenceSamples := encoderSampleRate * trailingSilenceMS / 1000
	out := make([]int16, samples24*2+trailingSilenceSamples)
	for i := range samples24 {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:])) //nolint:gosec // PCM uint16 to int16 reinterpret
		out[i*2] = sample
		out[i*2+1] = sample
	}
	return out
}

func downsample24to16(pcm []byte) []byte {
	samples24 := len(pcm) / 2
	samples16 := samples24 * micSampleRate / backendOutputSampleRate
	out := make([]byte, samples16*2)
	for i := range samples16 {
		src := i * backendOutputSampleRate / micSampleRate
		copy(out[i*2:], pcm[src*2:src*2+2])
	}
	return out
}

func pcmS16LEEnergy(pcm []byte) float64 {
	samples := len(pcm) / 2
	if samples == 0 {
		return 0
	}
	var energy float64
	for i := range samples {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:])) //nolint:gosec // PCM uint16 to int16 reinterpret
		energy += float64(sample) * float64(sample)
	}
	return math.Sqrt(energy / float64(samples))
}
