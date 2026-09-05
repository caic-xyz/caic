// Tests for the WebRTC voice bridge IPv4-only network layer and audio pipeline.

package voicertc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strconv"
	"testing"

	"github.com/maruel/gopus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/caic-xyz/caic/gomode/voicegateway"
	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
)

func TestClassifyVoiceRTCConnectivity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		server     voicev1.VoiceRTCServerDiagnostics
		client     voicev1.VoiceRTCClientDiagnostics
		wantIssue  voicev1.VoiceRTCConnectivityIssue
		wantSide   voicev1.VoiceRTCConnectivitySide
		wantReason string
	}{
		{
			name:       "server session missing",
			server:     voicev1.VoiceRTCServerDiagnostics{SessionFound: false},
			wantIssue:  voicev1.VoiceRTCConnectivityIssueServerSessionMissing,
			wantSide:   voicev1.VoiceRTCConnectivitySideServer,
			wantReason: "server no longer has this voice RTC session",
		},
		{
			name:       "udp unreachable",
			server:     voicev1.VoiceRTCServerDiagnostics{SessionFound: true, UDPEndpoints: []voicev1.VoiceRTCUDPEndpoint{{Host: "192.0.2.10", Port: 3478}}},
			client:     voicev1.VoiceRTCClientDiagnostics{ICEConnectionState: "failed"},
			wantIssue:  voicev1.VoiceRTCConnectivityIssueUDPUnreachable,
			wantSide:   voicev1.VoiceRTCConnectivitySideNetwork,
			wantReason: "client could not establish ICE with server UDP candidates 192.0.2.10:3478",
		},
		{
			name:       "backend connecting",
			server:     voicev1.VoiceRTCServerDiagnostics{SessionFound: true, DataChannelOpened: true},
			wantIssue:  voicev1.VoiceRTCConnectivityIssueVoiceBackendConnecting,
			wantSide:   voicev1.VoiceRTCConnectivitySideServer,
			wantReason: "WebRTC connected, but the server voice backend has not connected yet",
		},
		{
			name: "ready not delivered",
			server: voicev1.VoiceRTCServerDiagnostics{
				SessionFound:      true,
				DataChannelOpened: true,
				BackendConnected:  true,
				SessionReadySent:  true,
			},
			wantIssue:  voicev1.VoiceRTCConnectivityIssueSessionReadyNotDelivered,
			wantSide:   voicev1.VoiceRTCConnectivitySideClient,
			wantReason: "server sent session.ready; the client did not receive or process it before timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotIssue, gotSide, gotReason := classifyVoiceRTCConnectivity(&tc.server, &tc.client)
			if gotIssue != tc.wantIssue || gotSide != tc.wantSide || gotReason != tc.wantReason {
				t.Fatalf("diagnosis = (%q, %q, %q), want (%q, %q, %q)", gotIssue, gotSide, gotReason, tc.wantIssue, tc.wantSide, tc.wantReason)
			}
		})
	}
}

func TestTranslateGatewayClientMessage(t *testing.T) {
	t.Parallel()
	t.Run("setup", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"session.setup","voice":{"name":"Kore","language":"en"},"tools":[{"name":"tasks_list","description":"List tasks","parameters":{"type":"object","properties":{}}}],"context":{"systemInstruction":"system prompt"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg geminiSetupMessage
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Setup.Model != geminiModelName {
			t.Errorf("model = %q, want %q", msg.Setup.Model, geminiModelName)
		}
		if len(msg.Setup.SystemInstruction.Parts) != 1 || msg.Setup.SystemInstruction.Parts[0].Text != "system prompt" {
			t.Errorf("system instruction = %#v, want system prompt", msg.Setup.SystemInstruction)
		}
		if len(msg.Setup.Tools) != 1 || len(msg.Setup.Tools[0].FunctionDeclarations) != 1 {
			t.Fatalf("tools = %#v, want one declaration", msg.Setup.Tools)
		}
		decl := msg.Setup.Tools[0].FunctionDeclarations[0]
		if decl.Name != "tasks_list" {
			t.Errorf("declaration name = %q, want tasks_list", decl.Name)
		}
		if string(decl.ParametersJsonSchema) != `{"type":"object","properties":{}}` {
			t.Errorf("parametersJsonSchema = %s, want empty object schema", decl.ParametersJsonSchema)
		}
	})

	t.Run("context update", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"context.update","context":{"text":"status update"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg geminiRealtimeText
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.RealtimeInput.Text != "status update" {
			t.Fatalf("realtimeInput.text = %q, want status update", msg.RealtimeInput.Text)
		}
	})

	t.Run("user message", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"user.message","text":"Say exactly one word: Ready"}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg geminiClientContentMessage
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		if !msg.ClientContent.TurnComplete {
			t.Fatal("turnComplete = false, want true")
		}
	})

	t.Run("tool result", func(t *testing.T) {
		t.Parallel()
		got, err := translateGatewayClientMessage([]byte(`{"kind":"tool.result","id":"call-1","name":"tasks_list","result":{"ok":true}}`))
		if err != nil {
			t.Fatal(err)
		}
		var msg geminiToolResponseMessage
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Fatal(err)
		}
		if len(msg.ToolResponse.FunctionResponses) != 1 || msg.ToolResponse.FunctionResponses[0].ID != "call-1" {
			t.Fatalf("tool response = %#v, want call-1", msg.ToolResponse)
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

func TestBuildGeminiClientContentText(t *testing.T) {
	t.Parallel()
	got, err := buildGeminiClientContentText("Say exactly one word: Ready")
	if err != nil {
		t.Fatal(err)
	}
	var msg geminiClientContentMessage
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.ClientContent.TurnComplete {
		t.Fatal("turnComplete = false, want true")
	}
	if len(msg.ClientContent.Turns) != 1 {
		t.Fatalf("turns = %d, want one turn", len(msg.ClientContent.Turns))
	}
	turn := msg.ClientContent.Turns[0]
	if turn.Role != "user" {
		t.Fatalf("role = %q, want user", turn.Role)
	}
	if len(turn.Parts) != 1 || turn.Parts[0].Text != "Say exactly one word: Ready" {
		t.Fatalf("parts = %#v, want user message", turn.Parts)
	}
}

func TestTranslateGeminiServerMessage(t *testing.T) {
	t.Parallel()
	t.Run("setup complete emits no client message", func(t *testing.T) {
		t.Parallel()
		// session.ready is emitted by the gateway core, not the provider
		// translation. SetupComplete only triggers backendReady.
		got, err := translateGeminiServerMessage([]byte(`{"setupComplete":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("messages = %d, want 0", len(got))
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

func TestGatewaySessionReady(t *testing.T) {
	t.Parallel()
	var msg voicev1.SessionReady
	if err := json.Unmarshal(gatewaySessionReady(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Kind != voicev1.MessageKindSessionReady {
		t.Fatalf("kind = %q, want session.ready", msg.Kind)
	}
}

func TestGeminiAudioExtractionEmitsSpeechStarted(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	sess := &geminiBridgeSession{id: "test", sink: sink}
	msg := []byte(`{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"AQIDBA=="}}]}}}`)

	modified, ok := sess.handleAudioExtraction(t.Context(), msg)
	if !ok {
		t.Fatal("handleAudioExtraction did not report audio")
	}
	if bytes.Contains(modified, []byte("AQIDBA==")) {
		t.Fatal("audio data was not stripped from provider message")
	}

	sink.mu.Lock()
	pcmLen := len(sink.pcm)
	msgs := append([][]byte(nil), sink.msgs...)
	sink.mu.Unlock()

	if pcmLen != 4 {
		t.Fatalf("pcm bytes = %d, want 4", pcmLen)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want speech.started", len(msgs))
	}
	var started voicev1.SpeechStarted
	if err := json.Unmarshal(msgs[0], &started); err != nil {
		t.Fatal(err)
	}
	if started.Kind != voicev1.MessageKindSpeechStarted || started.Speaker != voicev1.SpeakerAssistant {
		t.Fatalf("message = %+v, want assistant speech.started", started)
	}
}

func TestNewBridge(t *testing.T) {
	t.Parallel()
	cfg := &voicegateway.Config{Backend: voicegateway.BackendGeminiLive}
	t.Run("NewBridge", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), cfg, "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll(t.Context())
	})

	t.Run("PeerConnection", func(t *testing.T) {
		t.Parallel()
		b, err := NewBridge(t.Context(), cfg, "test-key", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll(t.Context())

		api, err := b.ensureWebRTCAPI(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
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

func TestRewriteSDPMappedCandidates(t *testing.T) {
	t.Parallel()
	t.Run("rewrites mapped external port", func(t *testing.T) {
		t.Parallel()
		sdp := "v=0\r\na=candidate:1 1 udp 2130706431 203.0.113.10 3478 typ srflx\r\na=candidate:2 1 udp 2130706431 192.168.1.20 3478 typ host\r\n"
		got := rewriteSDPMappedCandidates(sdp, "203.0.113.10", 40000)
		want := "v=0\r\na=candidate:1 1 udp 2130706431 203.0.113.10 40000 typ srflx\r\na=candidate:2 1 udp 2130706431 192.168.1.20 3478 typ host\r\n"
		if got != want {
			t.Fatalf("sdp = %q, want %q", got, want)
		}
	})

	t.Run("rewrites unspecified srflx candidate from pion", func(t *testing.T) {
		t.Parallel()
		sdp := "v=0\r\na=candidate:1 1 udp 2130706431 192.168.1.20 3478 typ host\r\na=candidate:2 1 udp 1694498815 0.0.0.0 34123 typ srflx raddr 192.168.1.20 rport 3478\r\n"
		got := rewriteSDPMappedCandidates(sdp, "203.0.113.10", 40000)
		want := "v=0\r\na=candidate:1 1 udp 2130706431 192.168.1.20 3478 typ host\r\na=candidate:2 1 udp 1694498815 203.0.113.10 40000 typ srflx raddr 192.168.1.20 rport 3478\r\n"
		if got != want {
			t.Fatalf("sdp = %q, want %q", got, want)
		}
	})

	t.Run("rewrites every srflx candidate", func(t *testing.T) {
		t.Parallel()
		sdp := "v=0\r\na=candidate:1 1 udp 1694498815 0.0.0.0 34123 typ srflx raddr 192.168.122.1 rport 3478\r\n"
		got := rewriteSDPMappedCandidates(sdp, "203.0.113.10", 40000)
		want := "v=0\r\na=candidate:1 1 udp 1694498815 203.0.113.10 40000 typ srflx raddr 192.168.122.1 rport 3478\r\n"
		if got != want {
			t.Fatalf("sdp = %q, want %q", got, want)
		}
	})
}

func TestIceCandidateIPv4(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"192.168.1.10", "100.64.1.2", "10.0.0.5"} {
			if !iceCandidateIPv4(net.ParseIP(raw)) {
				t.Fatalf("iceCandidateIPv4(%s) = false, want true", raw)
			}
		}
	})

	t.Run("rejects loopback and link local", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"127.0.0.1", "169.254.10.20"} {
			if iceCandidateIPv4(net.ParseIP(raw)) {
				t.Fatalf("iceCandidateIPv4(%s) = true, want false", raw)
			}
		}
	})
}

func TestNewIPv4Net(t *testing.T) {
	t.Parallel()
	n := newIPv4Net(t.Context(), []net.IP{net.ParseIP("192.168.1.10"), net.ParseIP("100.64.1.2")})
	interfaces, err := n.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 1 {
		t.Fatalf("len(interfaces) = %d, want 1", len(interfaces))
	}
	addrs, err := interfaces[0].Addrs()
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 {
		t.Fatalf("len(addrs) = %d, want 2", len(addrs))
	}
}

func TestDefaultIPv4(t *testing.T) {
	t.Parallel()
	t.Run("prefers non link local", func(t *testing.T) {
		t.Parallel()
		got, ok := bestIPv4Candidate([]net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("169.254.10.20"),
			net.ParseIP("192.168.1.10"),
		})
		if !ok {
			t.Fatal("expected candidate")
		}
		if !got.Equal(net.ParseIP("192.168.1.10")) {
			t.Fatalf("candidate = %s, want 192.168.1.10", got)
		}
	})

	t.Run("uses link local offline address", func(t *testing.T) {
		t.Parallel()
		got, ok := bestIPv4Candidate([]net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("169.254.10.20"),
		})
		if !ok {
			t.Fatal("expected candidate")
		}
		if !got.Equal(net.ParseIP("169.254.10.20")) {
			t.Fatalf("candidate = %s, want 169.254.10.20", got)
		}
	})

	t.Run("uses loopback when only local use is available", func(t *testing.T) {
		t.Parallel()
		got, ok := bestIPv4Candidate([]net.IP{net.ParseIP("127.0.0.1")})
		if !ok {
			t.Fatal("expected candidate")
		}
		if !got.Equal(net.ParseIP("127.0.0.1")) {
			t.Fatalf("candidate = %s, want 127.0.0.1", got)
		}
	})

	t.Run("rejects empty candidates", func(t *testing.T) {
		t.Parallel()
		_, ok := bestIPv4Candidate(nil)
		if ok {
			t.Fatal("unexpected candidate")
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
		b.CloseAll(t.Context())
		if !backend.closed {
			t.Fatal("backend was not closed by CloseAll")
		}
	})

	t.Run("NewBridgeWithBackendBindsUDPAndDefersAPI", func(t *testing.T) {
		t.Parallel()
		backend := &fakeBackendConnector{}
		b, err := newBridgeWithBackend(t.Context(), backend, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer b.CloseAll(t.Context())
		if b.udpPort == 0 {
			t.Fatal("udpPort = 0, want bound port")
		}
		if b.api != nil {
			t.Fatal("api initialized before first offer")
		}
		var lc net.ListenConfig
		conn, err := lc.ListenPacket(t.Context(), "udp4", net.JoinHostPort("", strconv.Itoa(b.udpPort)))
		if err == nil {
			_ = conn.Close()
			t.Fatal("expected UDP port to be in use")
		}
	})

	t.Run("DiagnosticsSurfacesUDPMappingError", func(t *testing.T) {
		t.Parallel()
		mapping := &upnpMapping{}
		mapping.setRefreshErr(errors.New("refresh failed"))
		b := &Bridge{
			upnpMapping: mapping,
			advertisedEndpoints: []udpCandidate{{
				host: net.ParseIP("192.168.1.20"),
				port: 3478,
			}},
		}
		got := b.DiagnoseVoiceRTC(t.Context(), "missing", nil)
		if got.Server.UDPMappingError != "refresh failed" {
			t.Fatalf("UDPMappingError = %q, want refresh failed", got.Server.UDPMappingError)
		}
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
	closed    bool
	closeErr  error
	session   *fakeBackendSession
}

func (b *fakeBackendConnector) Close() error {
	b.closed = true
	return b.closeErr
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
