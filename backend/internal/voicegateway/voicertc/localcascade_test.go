// Tests for the local cascade backend: VAD, turn flow, tool round trip, barge-in.

//go:build !race

package voicertc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func TestEnergyVAD(t *testing.T) {
	t.Parallel()

	t.Run("speech then silence yields an utterance", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		utt, active := v.push(loudPCM(200))
		if utt != nil {
			t.Fatalf("utterance during speech = %d bytes, want nil", len(utt))
		}
		if !active {
			t.Fatal("speechActive = false during speech, want true")
		}
		utt, active = v.push(silencePCM(vadSilenceHangoverMS + vadFrameMS))
		if utt == nil {
			t.Fatal("utterance after silence = nil, want non-empty")
		}
		if active {
			t.Fatal("speechActive = true after utterance end, want false")
		}
	})

	t.Run("too-short speech is discarded", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		v.push(loudPCM(vadFrameMS * 2)) // 40ms < vadMinSpeechMS
		utt, _ := v.push(silencePCM(vadSilenceHangoverMS + vadFrameMS))
		if utt != nil {
			t.Fatalf("utterance = %d bytes, want nil for too-short speech", len(utt))
		}
	})

	t.Run("leading silence is dropped", func(t *testing.T) {
		t.Parallel()
		v := &energyVAD{}
		utt, active := v.push(silencePCM(200))
		if utt != nil || active {
			t.Fatalf("silence produced utterance=%v active=%v, want nil/false", utt != nil, active)
		}
	})
}

func TestLocalCascadeTurn(t *testing.T) {
	t.Parallel()
	backend := newLocalCascadeBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, placeholderLLM{}, placeholderTTS{},
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "turn", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	setup := mustJSON(t, voicev1.SessionSetup{
		Kind:  voicev1.MessageKindSessionSetup,
		Voice: voicev1.VoiceConfig{Name: "local", Language: "en"},
		Tools: []voicev1.ToolDeclaration{{Name: "tasks_list", Description: "List tasks", Parameters: json.RawMessage(`{}`)}},
	})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}
	if !sink.isReady() {
		t.Fatal("backend not ready after session.setup")
	}

	// Synthetic utterance: loud speech followed by silence.
	mustAcceptMic(t, sess, loudPCM(200))
	mustAcceptMic(t, sess, silencePCM(vadSilenceHangoverMS+vadFrameMS))

	// The placeholder LLM calls the first declared tool first.
	waitForKind(t, sink, voicev1.MessageKindToolCall)
	if !slices.Contains(sink.kinds(), voicev1.MessageKindTranscriptDelta) {
		t.Fatal("missing user transcript before tool call")
	}
	call := decodeToolCall(t, sink)
	if call.Name != "tasks_list" {
		t.Fatalf("tool call name = %q, want tasks_list", call.Name)
	}

	// Return the tool result; the turn should then speak.
	result := mustJSON(t, voicev1.ToolResult{
		Kind: voicev1.MessageKindToolResult, ID: call.ID, Name: call.Name, Result: json.RawMessage(`{"tasks":[]}`),
	})
	if err := sess.acceptClientMessage(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	waitForKind(t, sink, voicev1.MessageKindSpeechEnded)
	kinds := sink.kinds()
	for _, want := range []voicev1.MessageKind{
		voicev1.MessageKindSpeechStarted,
		voicev1.MessageKindAssistantTextDelta,
		voicev1.MessageKindSpeechEnded,
	} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("kinds = %v, missing %s", kinds, want)
		}
	}
	if sink.pcmLen() == 0 {
		t.Fatal("no assistant audio produced")
	}
}

func TestLocalCascadeBargeIn(t *testing.T) {
	t.Parallel()
	backend := newLocalCascadeBackend(
		func() vadSegmenter { return &energyVAD{} },
		placeholderASR{}, echoLLM{}, longTTS{ms: 800},
	)
	sink := &captureSink{}
	sess, err := backend.connect(t.Context(), "barge", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.close() })

	// No tools: the echo LLM speaks immediately.
	setup := mustJSON(t, voicev1.SessionSetup{Kind: voicev1.MessageKindSessionSetup})
	if err := sess.acceptClientMessage(t.Context(), setup); err != nil {
		t.Fatal(err)
	}

	mustAcceptMic(t, sess, loudPCM(200))
	mustAcceptMic(t, sess, silencePCM(vadSilenceHangoverMS+vadFrameMS))

	// Wait until the assistant is speaking, then barge in with new speech.
	waitForKind(t, sink, voicev1.MessageKindSpeechStarted)
	mustAcceptMic(t, sess, loudPCM(60))

	waitForKind(t, sink, voicev1.MessageKindInterrupted)
	// The interrupted turn must not complete (no speech.ended afterward).
	time.Sleep(50 * time.Millisecond)
	kinds := sink.kinds()
	if slices.Contains(kinds, voicev1.MessageKindSpeechEnded) {
		t.Fatalf("kinds = %v, barge-in must cancel before speech.ended", kinds)
	}
	if sink.wasCanceled() {
		t.Fatal("barge-in must not cancel the whole session")
	}
}

// --- test doubles and helpers ---

type captureSink struct {
	mu       sync.Mutex
	ready    bool
	msgs     [][]byte
	pcm      []byte
	canceled bool
}

func (c *captureSink) backendReady(context.Context) {
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
}

func (c *captureSink) sendGatewayMessage(_ context.Context, data []byte) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, slices.Clone(data))
	c.mu.Unlock()
	return nil
}

func (c *captureSink) sendGatewayError(string) {}

func (c *captureSink) cancelSession() {
	c.mu.Lock()
	c.canceled = true
	c.mu.Unlock()
}

func (c *captureSink) addAssistantPCM(pcm []byte) {
	c.mu.Lock()
	c.pcm = append(c.pcm, pcm...)
	c.mu.Unlock()
}

func (c *captureSink) clearAssistantAudio() {
	c.mu.Lock()
	c.pcm = nil
	c.mu.Unlock()
}

func (c *captureSink) isReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

func (c *captureSink) wasCanceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

func (c *captureSink) pcmLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pcm)
}

func (c *captureSink) kinds() []voicev1.MessageKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	kinds := make([]voicev1.MessageKind, 0, len(c.msgs))
	for _, m := range c.msgs {
		var env voicev1.MessageEnvelope
		if json.Unmarshal(m, &env) == nil {
			kinds = append(kinds, env.Kind)
		}
	}
	return kinds
}

func decodeToolCall(t *testing.T, sink *captureSink) voicev1.ToolCall {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, m := range sink.msgs {
		var env voicev1.MessageEnvelope
		if json.Unmarshal(m, &env) != nil || env.Kind != voicev1.MessageKindToolCall {
			continue
		}
		var call voicev1.ToolCall
		if err := json.Unmarshal(m, &call); err != nil {
			t.Fatal(err)
		}
		return call
	}
	t.Fatal("no tool.call captured")
	return voicev1.ToolCall{}
}

func waitForKind(t *testing.T, sink *captureSink, kind voicev1.MessageKind) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(sink.kinds(), kind) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %v", kind, sink.kinds())
}

func mustAcceptMic(t *testing.T, sess backendSession, pcm []byte) {
	t.Helper()
	if err := sess.acceptMicPCM(t.Context(), pcm); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loudPCM(ms int) []byte {
	samples := micSampleRate * ms / 1000
	b := make([]byte, samples*2)
	for i := range samples {
		v := int16(8000 * math.Sin(2*math.Pi*300*float64(i)/float64(micSampleRate)))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v)) //nolint:gosec // PCM int16→uint16 reinterpret
	}
	return b
}

func silencePCM(ms int) []byte {
	return make([]byte, micSampleRate*2*ms/1000)
}

type echoLLM struct{}

func (echoLLM) newConversation(string, []voicev1.ToolDeclaration) llmConversation { return echoConv{} }

type echoConv struct{}

func (echoConv) user(_ context.Context, text string) (llmReply, error) {
	return llmReply{text: "echo: " + text}, nil
}

func (echoConv) toolResult(context.Context, string, string, json.RawMessage) (llmReply, error) {
	return llmReply{text: "done"}, nil
}

func (echoConv) addContext(string) {}

type longTTS struct{ ms int }

func (t longTTS) synthesize(context.Context, string) ([]byte, error) {
	return make([]byte, backendOutputSampleRate*2*t.ms/1000), nil
}
