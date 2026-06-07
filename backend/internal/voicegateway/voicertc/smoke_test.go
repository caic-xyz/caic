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

	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

// TestSmokeVoiceRTCLocalAudio verifies the managed local-stack ASR, LLM, and
// TTS paths with direct queries plus one WebRTC turn for the LLM.
func TestSmokeVoiceRTCLocalAudio(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	t.Run("Audio", func(t *testing.T) {
		t.Parallel()

		var spokenPCM24 []byte

		t.Run("TTS", func(t *testing.T) {
			runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(t.Context()))
			t.Cleanup(runtimeCancel)
			tts, err := newKittenTTSAdapter(runtimeCtx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tts.Close(); err != nil {
					t.Fatal(err)
				}
			})

			const smokeSentence = "I love bananas"
			chunks, err := collectTTSChunks(t.Context(), tts, smokeSentence)
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
			runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(t.Context()))
			t.Cleanup(runtimeCancel)
			endpoint, err := localStackLlamaEndpoint(runtimeCtx, "", "", defaultLocalStackASRModel, startManagedLlamaServer)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := endpoint.runtime.Close(); err != nil {
					t.Fatal(err)
				}
			})

			text, err := (&genaiASRAdapter{provider: endpoint.provider}).transcribe(t.Context(), downsample24to16(spokenPCM24))
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
		t.Parallel()

		runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(t.Context()))
		t.Cleanup(runtimeCancel)
		endpoint, err := localStackLlamaEndpoint(runtimeCtx, "", "", defaultLocalStackLLMModel, startManagedLlamaServer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := endpoint.runtime.Close(); err != nil {
				t.Fatal(err)
			}
		})

		conv := (&genaiLLMAdapter{provider: endpoint.provider}).newConversation(
			"Answer with a short plain text sentence.",
			nil,
		)
		reply, err := conv.user(t.Context(), "Reply with the word banana.")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(reply.text) == "" {
			t.Fatal("llama.cpp query returned empty text")
		}
		if !strings.Contains(strings.ToLower(reply.text), "banana") {
			t.Fatalf("llama.cpp reply = %q, want banana", reply.text)
		}
		t.Logf("llama.cpp reply: %s", strings.TrimSpace(reply.text))

		s := newVoiceRTCTestSession(t.Context(), t, newLocalStackBackend(
			func() vadSegmenter { return &energyVAD{} },
			fixedASR{text: "hello"}, &genaiLLMAdapter{provider: endpoint.provider}, placeholderTTS{},
		))
		writeLocalMicAudio(t.Context(), t, s.micTrack)
		data := waitForVoiceRTCMessage(t.Context(), t, s.messages, s.signalErrs, voicev1.MessageKindAssistantTextDelta)
		var msg voicev1.AssistantTextDelta
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(msg.Text) == "" {
			t.Fatal("assistant text is empty")
		}
	})
}

func collectTTSChunks(ctx context.Context, tts *kittenTTSAdapter, text string) ([][]byte, error) {
	var chunks [][]byte
	for pcm, err := range tts.synthesize(ctx, text) {
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, pcm)
	}
	return chunks, nil
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
