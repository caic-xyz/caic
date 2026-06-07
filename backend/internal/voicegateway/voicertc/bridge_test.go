// Tests for the WebRTC voice bridge IPv4-only network layer and audio pipeline.

//go:build !race

package voicertc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/maruel/gopus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func TestTranslateGatewayClientMessage(t *testing.T) {
	t.Parallel()
	t.Run("setup", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"session.setup","voice":{"name":"Kore","language":"en"},"tools":[{"name":"tasks_list","description":"List tasks","parameters":{"type":"object","properties":{}}}],"context":{"systemInstruction":"system prompt"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		setup, ok := msg["setup"].(map[string]any)
		if !ok {
			t.Fatalf("setup = %T, want object", msg["setup"])
		}
		if setup["model"] != geminiModelName {
			t.Errorf("model = %q, want %q", setup["model"], geminiModelName)
		}
		if _, ok := setup["systemInstruction"]; !ok {
			t.Fatal("missing systemInstruction")
		}
		if _, ok := setup["inputAudioTranscription"].(map[string]any); !ok {
			t.Fatalf("inputAudioTranscription = %T, want object", setup["inputAudioTranscription"])
		}
		if _, ok := setup["outputAudioTranscription"].(map[string]any); !ok {
			t.Fatalf("outputAudioTranscription = %T, want object", setup["outputAudioTranscription"])
		}
		tools, ok := setup["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %T len %d, want one tool", setup["tools"], len(tools))
		}
		tool, ok := tools[0].(map[string]any)
		if !ok {
			t.Fatalf("tool = %T, want object", tools[0])
		}
		decls, ok := tool["functionDeclarations"].([]any)
		if !ok || len(decls) != 1 {
			t.Fatalf("functionDeclarations = %T len %d, want one declaration", tool["functionDeclarations"], len(decls))
		}
		decl, ok := decls[0].(map[string]any)
		if !ok {
			t.Fatalf("declaration = %T, want object", decls[0])
		}
		if _, ok := decl["parameters"]; ok {
			t.Fatal("provider declaration used parameters, want parametersJsonSchema")
		}
		if _, ok := decl["parametersJsonSchema"]; !ok {
			t.Fatal("missing parametersJsonSchema")
		}
	})

	t.Run("context update", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"context.update","context":{"text":"status update"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		realtimeInput, ok := msg["realtimeInput"].(map[string]any)
		if !ok {
			t.Fatalf("realtimeInput = %T, want object", msg["realtimeInput"])
		}
		if realtimeInput["text"] != "status update" {
			t.Fatalf("realtimeInput.text = %q, want status update", realtimeInput["text"])
		}
	})

	t.Run("tool result", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"tool.result","id":"call-1","name":"tasks_list","result":{"ok":true}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		if _, ok := msg["toolResponse"]; !ok {
			t.Fatal("missing toolResponse")
		}
	})

	t.Run("rejects malformed setup", func(t *testing.T) {
		t.Parallel()
		_, err := translateGatewayClientMessage([]byte(`{"kind":"session.setup","voice":{"name":"Kore","language":"en"},"tools":[],"context":{}}`))
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects provider message", func(t *testing.T) {
		t.Parallel()
		_, err := translateGatewayClientMessage([]byte(`{"setup":{"model":"provider"}}`))
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestTranslateGeminiServerMessage(t *testing.T) {
	t.Parallel()
	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		got, err := translateGeminiServerMessage([]byte(`{"setupComplete":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("messages = %d, want 1", len(got))
		}
		var msg voicev1.SessionReady
		if err := json.Unmarshal(got[0], &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Kind != voicev1.MessageKindSessionReady || msg.Profile != gatewayProfileDefault {
			t.Fatalf("message = %+v, want session.ready default", msg)
		}
		if len(msg.Capabilities) == 0 || msg.Capabilities[0] != "voice.protocol.v1" {
			t.Fatalf("capabilities = %v, want voice.protocol.v1 first", msg.Capabilities)
		}
	})

	t.Run("transcript", func(t *testing.T) {
		t.Parallel()
		got, err := translateGeminiServerMessage([]byte(`{
			"serverContent":{
				"inputTranscription":{"text":"hello"},
				"outputTranscription":{"text":"Ready"},
				"turnComplete":true
			}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		kinds := make([]voicev1.MessageKind, 0, len(got))
		for _, raw := range got {
			var msg voicev1.MessageEnvelope
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, msg.Kind)
		}
		want := []voicev1.MessageKind{
			voicev1.MessageKindTranscriptDelta,
			voicev1.MessageKindSpeechStarted,
			voicev1.MessageKindTranscriptDelta,
			voicev1.MessageKindAssistantTextDelta,
			voicev1.MessageKindSpeechEnded,
		}
		if len(kinds) != len(want) {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
		for i, kind := range want {
			if kinds[i] != kind {
				t.Fatalf("kinds = %v, want %v", kinds, want)
			}
		}
	})

	t.Run("tool call", func(t *testing.T) {
		t.Parallel()
		got, err := translateGeminiServerMessage([]byte(`{
			"toolCall":{"functionCalls":[{"id":"call-1","name":"tasks_list","args":{}}]}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("messages = %d, want 1", len(got))
		}
		var msg voicev1.ToolCall
		if err := json.Unmarshal(got[0], &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Kind != voicev1.MessageKindToolCall || msg.ID != "call-1" || msg.Name != "tasks_list" {
			t.Fatalf("message = %+v, want tool.call", msg)
		}
	})
}

func TestNewBridge(t *testing.T) {
	t.Parallel()
	t.Run("NewBridge", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll()
	})

	t.Run("PeerConnection", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll()

		pc, err := b.api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal("create PeerConnection:", err)
		}
		defer func() { _ = pc.Close() }()

		if _, err := pc.CreateDataChannel("test", nil); err != nil {
			t.Fatal("create data channel:", err)
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal("create offer:", err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			t.Fatal("set local description:", err)
		}
	})
}

func TestBackendConnector(t *testing.T) {
	t.Parallel()
	t.Run("NewBridgeWithBackend", func(t *testing.T) {
		t.Parallel()
		backend := &fakeBackendConnector{}
		b, err := newBridgeWithBackend(t.Context(), backend, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll()
	})

	t.Run("SessionSink", func(t *testing.T) {
		t.Parallel()
		sess := &session{id: "test-session", cancel: func() {}}
		backend := &fakeBackendConnector{}
		backendSession, err := backend.connect(t.Context(), sess.id, sess)
		if err != nil {
			t.Fatal(err)
		}
		if err := backendSession.acceptClientMessage(t.Context(), []byte(`{"kind":"session.setup"}`)); err != nil {
			t.Fatal(err)
		}
		if err := backendSession.acceptMicPCM(t.Context(), []byte{1, 2}); err != nil {
			t.Fatal(err)
		}
		if err := backendSession.close(); err != nil {
			t.Fatal(err)
		}
		if !backend.connected || !backend.session.closed {
			t.Fatalf("backend connected=%t closed=%t, want both true", backend.connected, backend.session.closed)
		}
	})
}

type fakeBackendConnector struct {
	connected bool
	session   *fakeBackendSession
}

func (b *fakeBackendConnector) connect(
	_ context.Context,
	sessionID string,
	sink backendSink,
) (backendSession, error) {
	b.connected = true
	b.session = &fakeBackendSession{
		id:   sessionID,
		sink: sink,
	}
	return b.session, nil
}

type fakeBackendSession struct {
	id     string
	sink   backendSink
	closed bool
}

func (s *fakeBackendSession) acceptClientMessage(ctx context.Context, _ []byte) error {
	s.sink.backendReady(ctx)
	return nil
}

func (s *fakeBackendSession) acceptMicPCM(_ context.Context, pcm []byte) error {
	s.sink.addAssistantPCM(pcm)
	return nil
}

func (s *fakeBackendSession) close() error {
	s.closed = true
	return nil
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Parallel()
	const (
		freq       = 440.0
		durationMs = 200
		amplitude  = 28000.0
		samples24  = 24000 * durationMs / 1000
	)

	pcm24Bytes := make([]byte, samples24*2)
	for i := range samples24 {
		ts := float64(i) / 24000.0
		s := math.Sin(2 * math.Pi * freq * ts)
		binary.LittleEndian.PutUint16(pcm24Bytes[i*2:], uint16(int16(s*amplitude))) //nolint:gosec // fits in int16
	}

	// 24kHz → 48kHz upsampling.
	pcm48 := upsample24to48(pcm24Bytes)
	if len(pcm48) != samples24*2 {
		t.Fatalf("upsample: got %d samples, want %d", len(pcm48), samples24*2)
	}

	// Encode at 48kHz, 20ms frames (960 samples).
	enc, err := newEncoder()
	if err != nil {
		t.Fatalf("newEncoder: %v", err)
	}
	var packets [][]byte
	for i := 0; i+encoderFrameSamples <= len(pcm48); i += encoderFrameSamples {
		pkt, encErr := enc.Encode(pcm48[i : i+encoderFrameSamples])
		if encErr != nil {
			t.Fatalf("Encode at %d: %v", i, encErr)
		}
		packets = append(packets, pkt)
	}
	frames := len(pcm48) / encoderFrameSamples
	if len(packets) != frames {
		t.Fatalf("packets: got %d, want %d", len(packets), frames)
	}

	// Decode at 48kHz.
	dec, err := gopus.NewDecoder(48000, 1)
	if err != nil {
		t.Fatalf("gopus.NewDecoder: %v", err)
	}
	var decoded []int16
	for _, pkt := range packets {
		samples, decErr := decode48(dec, pkt)
		if decErr != nil {
			t.Fatalf("Decode: %v", decErr)
		}
		decoded = append(decoded, samples...)
	}

	expectedLen := frames * encoderFrameSamples
	if len(decoded) < expectedLen-1 || len(decoded) > expectedLen+1 {
		t.Fatalf("decoded length: got %d, want ~%d", len(decoded), expectedLen)
	}

	var energy float64
	for _, s := range decoded {
		energy += float64(s) * float64(s)
	}
	energy /= float64(len(decoded))
	if energy < 5e6 {
		t.Errorf("signal too quiet: energy=%.0f", energy)
	}

	crossings := countZeroCrossings(decoded)
	expectedZC := int(freq * float64(durationMs) / 1000 * 2)
	if percentDiff(crossings, expectedZC) > 25 {
		t.Errorf("zero-crossings: got %d, want %d (±25%%)", crossings, expectedZC)
	}

	t.Logf("roundtrip OK: %d frames, %d samples, energy=%.0f, zc=%d",
		frames, len(decoded), energy, crossings)
}

// decode48 decodes an Opus packet at 48kHz into int16 samples.
func decode48(dec *gopus.Decoder, pkt []byte) ([]int16, error) {
	pcm := make([]int16, maxFrameSamples)
	n, err := dec.Decode(pkt, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}

func countZeroCrossings(samples []int16) int {
	if len(samples) < 2 {
		return 0
	}
	n := 0
	for i := 1; i < len(samples); i++ {
		if (samples[i-1] < 0) != (samples[i] < 0) {
			n++
		}
	}
	return n
}

func percentDiff(a, b int) int {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return 100
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d * 100 / b
}

func TestUpsampleEdgeCases(t *testing.T) {
	t.Parallel()
	if g := upsample24to48(nil); len(g) != 0 {
		t.Errorf("nil: got %d", len(g))
	}
	if g := upsample24to48([]byte{}); len(g) != 0 {
		t.Errorf("empty: got %d", len(g))
	}

	single := make([]byte, 2)
	binary.LittleEndian.PutUint16(single, 0x3039) // 12345
	g := upsample24to48(single)
	if len(g) != 2 {
		t.Fatalf("single: got %d, want 2", len(g))
	}
	if g[0] != 12345 || g[1] != 12345 {
		t.Errorf("single: got [%d %d], want [12345 12345]", g[0], g[1])
	}

	two := make([]byte, 4)
	binary.LittleEndian.PutUint16(two[0:], 0)
	binary.LittleEndian.PutUint16(two[2:], 1000)
	g = upsample24to48(two)
	if len(g) != 4 {
		t.Fatalf("two: got %d, want 4", len(g))
	}
	if g[0] != 0 || g[1] != 500 || g[2] != 1000 || g[3] != 1000 {
		t.Errorf("two: got [%d %d %d %d], want [0 500 1000 1000]", g[0], g[1], g[2], g[3])
	}
}

func TestWriteSampleHasBinding(t *testing.T) {
	t.Parallel()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	if len(pc.GetSenders()) != 1 {
		t.Fatalf("expected 1 sender, got %d", len(pc.GetSenders()))
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if len(pc.GetSenders()) != 1 {
		t.Fatal("sender lost after SetLocalDescription")
	}

	if err := track.WriteSample(media.Sample{
		Data:     []byte{0xfc, 0xff, 0xfe},
		Duration: frameDuration,
	}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	t.Log("WriteSample binding OK")
}
