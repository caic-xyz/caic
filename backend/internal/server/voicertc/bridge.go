// Package voicertc implements a WebRTC-to-Gemini-WebSocket bridge for voice sessions.
package voicertc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// geminiWSEndpoint is the Gemini Live BidiGenerateContent WebSocket URL.
	geminiWSEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	// idleTimeout closes sessions after 30 minutes of inactivity.
	idleTimeout = 30 * time.Minute

	// wsReadLimit is the max WebSocket message size (16 MiB for audio chunks).
	wsReadLimit = 16 * 1024 * 1024

	// sampleRate is the PCM sample rate used for Gemini audio I/O.
	sampleRate = 16000

	// frameDuration is the Opus frame duration.
	frameDuration = 20 * time.Millisecond

	// frameSamples is the number of 16kHz samples per 20ms frame.
	frameSamples = sampleRate * int(frameDuration/time.Millisecond) / 1000 // 320
)

// Bridge manages active WebRTC voice sessions.
type Bridge struct {
	geminiAPIKey string
	api          *webrtc.API
	udpMux       ice.UDPMux
	mu           sync.Mutex
	sessions     map[string]*session
}

// NewBridge creates a Bridge that multiplexes all WebRTC traffic through a
// single UDP port. This avoids opening ephemeral port ranges in the firewall.
func NewBridge(geminiAPIKey string, udpPort int) (*Bridge, error) {
	mux, err := ice.NewMultiUDPMuxFromPort(udpPort)
	if err != nil {
		return nil, fmt.Errorf("listen UDP :%d: %w", udpPort, err)
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	slog.Info("voicertc: listening", "udpPort", udpPort)
	return &Bridge{
		geminiAPIKey: geminiAPIKey,
		api:          api,
		udpMux:       mux,
		sessions:     make(map[string]*session),
	}, nil
}

// HandleOffer processes a WebRTC SDP offer, dials Gemini, and returns the SDP answer.
func (b *Bridge) HandleOffer(ctx context.Context, sdpOffer string) (sdpAnswer, sessionID string, err error) {
	if b.geminiAPIKey == "" {
		return "", "", errors.New("GEMINI_API_KEY not configured")
	}

	// Create PeerConnection using the shared UDP mux.
	pc, err := b.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create peer connection: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	sess := &session{
		id:     generateSessionID(),
		pc:     pc,
		cancel: cancel,
	}

	// Set up RTP audio track (server → client) when codec is available.
	if codecAvailable {
		audioTrack, trackErr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			"audio", "gemini-voice",
		)
		if trackErr != nil {
			_ = pc.Close()
			cancel()
			return "", "", fmt.Errorf("create audio track: %w", trackErr)
		}
		if _, trackErr = pc.AddTrack(audioTrack); trackErr != nil {
			_ = pc.Close()
			cancel()
			return "", "", fmt.Errorf("add audio track: %w", trackErr)
		}
		sess.audioTrack = audioTrack
	}

	// Handle incoming audio track (client → server) when codec is available.
	if codecAvailable {
		pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			if track.Kind() != webrtc.RTPCodecTypeAudio {
				return
			}
			slog.Info("voicertc: audio track received", "session", sess.id, "codec", track.Codec().MimeType)
			go sess.audioRxLoop(sessionCtx, track)
		})
	}

	// Set up data channel handler. The client creates the "gemini" data channel.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		slog.Info("voicertc: data channel opened", "label", dc.Label(), "session", sess.id)
		sess.mu.Lock()
		sess.dc = dc
		sess.mu.Unlock()

		dc.OnOpen(func() {
			// Connect to Gemini WebSocket.
			geminiURL := geminiWSEndpoint + "?key=" + url.QueryEscape(b.geminiAPIKey)
			wsConn, _, err := websocket.Dial(sessionCtx, geminiURL, nil)
			if err != nil {
				slog.Error("voicertc: gemini dial failed", "session", sess.id, "err", err)
				cancel()
				return
			}
			wsConn.SetReadLimit(wsReadLimit)
			sess.mu.Lock()
			sess.geminiWS = wsConn
			sess.mu.Unlock()
			slog.Info("voicertc: gemini connected", "session", sess.id, "rtpAudio", codecAvailable)

			// Start Gemini → data channel / RTP forwarding.
			go sess.geminiRxLoop(sessionCtx)
		})

		// Data channel → Gemini passthrough.
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			sess.mu.Lock()
			wsConn := sess.geminiWS
			sess.mu.Unlock()
			if wsConn == nil {
				return
			}
			if err := wsConn.Write(sessionCtx, websocket.MessageText, msg.Data); err != nil {
				slog.Warn("voicertc: dc→gemini write failed", "session", sess.id, "err", err)
			}
		})

		dc.OnClose(func() {
			slog.Info("voicertc: data channel closed", "session", sess.id)
			cancel()
		})
	})

	// Monitor ICE connection state.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Debug("voicertc: ICE state", "session", sess.id, "state", state.String())
		//exhaustive:ignore
		switch state {
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateClosed:
			cancel()
		default:
		}
	})

	// Set remote description (the offer).
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}); err != nil {
		_ = pc.Close()
		cancel()
		return "", "", fmt.Errorf("set remote description: %w", err)
	}

	// Create answer.
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		cancel()
		return "", "", fmt.Errorf("create answer: %w", err)
	}

	// Gather ICE candidates (block until complete for non-trickle ICE).
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		cancel()
		return "", "", fmt.Errorf("set local description: %w", err)
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		_ = pc.Close()
		cancel()
		return "", "", ctx.Err()
	}

	// Register session.
	b.mu.Lock()
	b.sessions[sess.id] = sess
	b.mu.Unlock()

	// Background cleanup.
	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.sessions, sess.id)
			b.mu.Unlock()
			sess.close()
			slog.Info("voicertc: session cleaned up", "session", sess.id)
		}()

		idleTimer := time.NewTimer(idleTimeout)
		defer idleTimer.Stop()

		select {
		case <-sessionCtx.Done():
		case <-idleTimer.C:
			slog.Info("voicertc: idle timeout", "session", sess.id)
		}
	}()

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		_ = pc.Close()
		cancel()
		return "", "", errors.New("no local description after ICE gathering")
	}
	return localDesc.SDP, sess.id, nil
}

// Close tears down a session by ID. No-op if not found.
func (b *Bridge) Close(sessionID string) {
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if ok {
		delete(b.sessions, sessionID)
	}
	b.mu.Unlock()
	if ok {
		sess.cancel()
		sess.close()
	}
}

// CloseAll tears down all sessions and the UDP mux. Called on server shutdown.
func (b *Bridge) CloseAll() {
	b.mu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.sessions = make(map[string]*session)
	b.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
		s.close()
	}
	if b.udpMux != nil {
		_ = b.udpMux.Close()
	}
}

// session holds all state for one bridge session.
type session struct {
	id         string
	mu         sync.Mutex
	pc         *webrtc.PeerConnection
	dc         *webrtc.DataChannel
	audioTrack *webrtc.TrackLocalStaticSample
	geminiWS   *websocket.Conn
	cancel     context.CancelFunc
}

// audioRxLoop reads Opus RTP from the client's mic track, decodes to PCM,
// and sends base64 realtimeInput messages to Gemini.
func (s *session) audioRxLoop(ctx context.Context, track *webrtc.TrackRemote) {
	dec, err := newDecoder()
	if err != nil {
		slog.Error("voicertc: decoder init failed", "session", s.id, "err", err)
		return
	}
	for {
		pkt, _, readErr := track.ReadRTP()
		if readErr != nil {
			if ctx.Err() == nil {
				slog.Warn("voicertc: audio read failed", "session", s.id, "err", readErr)
			}
			return
		}
		pcm, decErr := dec.Decode(pkt.Payload)
		if decErr != nil {
			slog.Debug("voicertc: opus decode failed", "session", s.id, "err", decErr)
			continue
		}
		// Convert int16 PCM to little-endian bytes, then base64 for Gemini.
		pcmBytes := make([]byte, len(pcm)*2)
		for i, sample := range pcm {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(sample)) //nolint:gosec // PCM int16→uint16 reinterpret is intentional
		}
		b64 := base64.StdEncoding.EncodeToString(pcmBytes)
		msg, _ := json.Marshal(map[string]any{
			"realtimeInput": map[string]any{
				"audio": map[string]string{
					"mimeType": fmt.Sprintf("audio/pcm;rate=%d", sampleRate),
					"data":     b64,
				},
			},
		})

		s.mu.Lock()
		wsConn := s.geminiWS
		s.mu.Unlock()
		if wsConn == nil {
			return
		}
		if err := wsConn.Write(ctx, websocket.MessageText, msg); err != nil {
			if ctx.Err() == nil {
				slog.Warn("voicertc: audio→gemini write failed", "session", s.id, "err", err)
			}
			return
		}
	}
}

// geminiRxLoop reads from the Gemini WebSocket and forwards messages.
// When codec is available: audio chunks are encoded to Opus and sent via RTP,
// non-audio content goes through the data channel.
// Without codec: everything goes through the data channel (passthrough).
func (s *session) geminiRxLoop(ctx context.Context) {
	var enc *opusEncoder
	if codecAvailable && s.audioTrack != nil {
		var err error
		enc, err = newEncoder()
		if err != nil {
			slog.Warn("voicertc: encoder init failed, falling back to passthrough", "session", s.id, "err", err)
		}
	}

	for {
		_, data, err := s.geminiWS.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("voicertc: gemini read failed", "session", s.id, "err", err)
			}
			s.cancel()
			return
		}

		// When encoder is available, intercept serverContent audio and send via RTP.
		if enc != nil {
			if modified, ok := s.handleAudioExtraction(ctx, data, enc); ok {
				data = modified
			}
		}

		s.mu.Lock()
		dc := s.dc
		s.mu.Unlock()
		if dc == nil {
			continue
		}
		if err := dc.SendText(string(data)); err != nil {
			slog.Warn("voicertc: gemini→dc send failed", "session", s.id, "err", err)
			s.cancel()
			return
		}
	}
}

// handleAudioExtraction checks if a Gemini message contains serverContent with
// inlineData audio. If so, it encodes the PCM audio to Opus and sends it via
// the RTP audio track, then returns a modified message with the audio stripped.
// Returns (modifiedData, true) if audio was extracted, (nil, false) otherwise.
func (s *session) handleAudioExtraction(ctx context.Context, data []byte, enc *opusEncoder) ([]byte, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false
	}
	scRaw, ok := msg["serverContent"]
	if !ok {
		return nil, false
	}

	var sc serverContent
	if err := json.Unmarshal(scRaw, &sc); err != nil {
		return nil, false
	}
	if sc.ModelTurn == nil {
		return nil, false
	}

	hadAudio := false
	filteredParts := make([]modelPart, 0, len(sc.ModelTurn.Parts))
	for _, part := range sc.ModelTurn.Parts {
		if part.InlineData == nil || part.InlineData.Data == "" {
			filteredParts = append(filteredParts, part)
			continue
		}
		hadAudio = true
		// Decode base64 PCM and send as Opus RTP.
		pcmBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			slog.Debug("voicertc: base64 decode failed", "session", s.id, "err", err)
			continue
		}
		s.encodeAndSendRTP(ctx, pcmBytes, enc)
	}
	if !hadAudio {
		return nil, false
	}

	// Rebuild the message without audio parts.
	sc.ModelTurn.Parts = filteredParts
	newSC, err := json.Marshal(sc)
	if err != nil {
		return nil, false
	}
	msg["serverContent"] = newSC
	rebuilt, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return rebuilt, true
}

// encodeAndSendRTP converts Gemini PCM (24kHz S16LE) to 16kHz, encodes as
// Opus, and writes to the RTP audio track.
func (s *session) encodeAndSendRTP(_ context.Context, pcmBytes []byte, enc *opusEncoder) {
	// Gemini outputs 24kHz PCM. Downsample to 16kHz by dropping every 3rd sample
	// (24000/16000 = 3/2 ratio: take 2 out of every 3 samples).
	samples24 := len(pcmBytes) / 2
	samples16 := samples24 * 2 / 3
	pcm16 := make([]int16, 0, samples16)
	for i := 0; i < samples24; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcmBytes[i*2:])) //nolint:gosec // PCM uint16→int16 reinterpret is intentional
		// Keep samples at positions 0,1 of every group of 3; skip position 2.
		if i%3 != 2 {
			pcm16 = append(pcm16, sample)
		}
	}

	// Encode in 20ms frames (320 samples at 16kHz).
	for i := 0; i+frameSamples <= len(pcm16); i += frameSamples {
		frame := pcm16[i : i+frameSamples]
		opusPkt, err := enc.Encode(frame)
		if err != nil {
			slog.Debug("voicertc: opus encode failed", "session", s.id, "err", err)
			continue
		}
		if err := s.audioTrack.WriteSample(media.Sample{
			Data:     opusPkt,
			Duration: frameDuration,
		}); err != nil {
			slog.Debug("voicertc: rtp write failed", "session", s.id, "err", err)
			return
		}
	}
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.geminiWS != nil {
		_ = s.geminiWS.Close(websocket.StatusNormalClosure, "session closed")
		s.geminiWS = nil
	}
	if s.pc != nil {
		_ = s.pc.Close()
		s.pc = nil
	}
}

// generateSessionID creates a short random session identifier.
func generateSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// JSON types for parsing Gemini serverContent to extract audio.
type serverContent struct {
	ModelTurn           *modelTurn     `json:"modelTurn,omitempty"`
	TurnComplete        *bool          `json:"turnComplete,omitempty"`
	Interrupted         *bool          `json:"interrupted,omitempty"`
	InputTranscription  *transcription `json:"inputTranscription,omitempty"`
	OutputTranscription *transcription `json:"outputTranscription,omitempty"`
}

type modelTurn struct {
	Parts []modelPart `json:"parts,omitempty"`
}

type modelPart struct {
	InlineData *inlineData `json:"inlineData,omitempty"`
	Text       string      `json:"text,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data"`
}

type transcription struct {
	Text string `json:"text,omitempty"`
}
