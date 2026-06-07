// WebRTC voice session bridge for the voice gateway protocol.

//go:build !race

// Package voicertc implements WebRTC voice sessions through the voice gateway protocol.
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
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

const (
	// geminiWSEndpoint is the Gemini Live BidiGenerateContent WebSocket URL.
	geminiWSEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	// geminiModelName is the default Gemini Live model used by the first gateway backend.
	geminiModelName = "models/gemini-3.1-flash-live-preview"

	// idleTimeout closes sessions after 30 minutes of inactivity.
	idleTimeout = 30 * time.Minute

	// wsReadLimit is the max WebSocket message size (16 MiB for audio chunks).
	wsReadLimit = 16 * 1024 * 1024

	// inputSampleRate matches Gemini's required input rate (PCM 16-bit, 16kHz).
	inputSampleRate = 16000

	// geminiSampleRate is the PCM sample rate of Gemini's audio output.
	geminiSampleRate = 24000

	// encoderSampleRate is the Opus encoder input rate. We encode at 48kHz —
	// the native Opus rate that every WebRTC implementation handles. Gemini
	// outputs 24kHz; we upsample before encoding.
	encoderSampleRate = 48000

	// frameDuration is the Opus frame duration.
	frameDuration = 20 * time.Millisecond

	// encoderFrameSamples is the number of samples per 20ms frame at the encoder rate.
	encoderFrameSamples = encoderSampleRate * int(frameDuration/time.Millisecond) / 1000 // 960

	// inputFrameBytes is one 20ms frame of Gemini PCM output (24kHz S16LE).
	inputFrameBytes = geminiSampleRate * 2 * int(frameDuration/time.Millisecond) / 1000 // 960 bytes
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
func NewBridge(ctx context.Context, geminiAPIKey string, udpPort int) (*Bridge, error) {
	// Discover the host's default IPv4 address without netlink (which fails
	// on hosts without IPv6 due to pion/anet's netlinkrib call).
	hostIP, err := defaultIPv4(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect host IP: %w", err)
	}
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", udpPort))
	if err != nil {
		return nil, fmt.Errorf("listen UDP4 :%d: %w", udpPort, err)
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	se.SetNet(newIPv4Net(ctx, hostIP))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
		External:        []string{hostIP.String()},
		AsCandidateType: webrtc.ICECandidateTypeHost,
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set ICE address rewrite: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected local address type: %T", conn.LocalAddr())
	}
	slog.Info("voicertc: listening", "udpPort", addr.Port, "hostIP", hostIP)
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
	if !strings.Contains(sdpOffer, "m=audio ") {
		return "", "", errors.New("SDP offer must include an audio track")
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

	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := &session{
		id:     generateSessionID(),
		pc:     pc,
		cancel: cancel,
	}

	// Set up RTP audio track (server → client).
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

	// Handle incoming audio track (client → server).
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		slog.Info("voicertc: audio track received", "session", sess.id, "codec", track.Codec().MimeType)
		sess.mu.Lock()
		if sess.geminiSetupComplete {
			sess.mu.Unlock()
			go sess.audioRxLoop(sessionCtx, track)
		} else {
			// Store the track until Gemini accepts setup. Sending realtime
			// audio before setupComplete can make Gemini reject the session.
			sess.pendingTrack = track
			sess.mu.Unlock()
		}
	})

	// Set up data channel handler. The client creates the "voice-gateway" data channel.
	// geminiReady is closed once the provider WebSocket is connected, unblocking
	// any client messages that arrived before the dial completed.
	geminiReady := make(chan struct{})
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
				sess.sendError("Failed to connect to Gemini: " + err.Error())
				cancel()
				return
			}
			wsConn.SetReadLimit(wsReadLimit)
			sess.mu.Lock()
			sess.geminiWS = wsConn
			sess.mu.Unlock()
			close(geminiReady)
			slog.Info("voicertc: gemini connected", "session", sess.id)

			// Start Gemini → data channel / RTP forwarding.
			go sess.geminiRxLoop(sessionCtx)
		})

		// Data channel → provider adapter. Blocks until the provider is connected
		// so the client's session.setup message is never dropped.
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			select {
			case <-geminiReady:
			case <-sessionCtx.Done():
				return
			}
			sess.mu.Lock()
			wsConn := sess.geminiWS
			sess.mu.Unlock()
			if wsConn == nil {
				return
			}
			providerMsg, err := translateGatewayClientMessage(msg.Data)
			if err != nil {
				if errors.Is(err, errSessionClosed) {
					cancel()
					return
				}
				slog.Warn("voicertc: gateway client message failed", "session", sess.id, "err", err)
				sess.sendError(err.Error())
				return
			}
			if len(providerMsg) == 0 {
				return
			}
			if err := wsConn.Write(sessionCtx, websocket.MessageText, providerMsg); err != nil {
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
	id string

	mu                  sync.Mutex
	pc                  *webrtc.PeerConnection
	dc                  *webrtc.DataChannel
	audioTrack          *webrtc.TrackLocalStaticSample
	geminiWS            *websocket.Conn
	pendingTrack        *webrtc.TrackRemote // set by OnTrack, consumed after Gemini setupComplete
	geminiSetupComplete bool
	cancel              context.CancelFunc

	audioMu  sync.Mutex
	audioBuf []byte // pending Gemini PCM bytes, drained by audioSendLoop
}

// audioRxLoop reads Opus RTP from the client's mic track, decodes to PCM,
// and sends base64 realtimeInput messages to Gemini.
func (s *session) audioRxLoop(ctx context.Context, track *webrtc.TrackRemote) {
	dec, err := newDecoder()
	if err != nil {
		slog.ErrorContext(ctx, "voicertc: decoder init failed", "session", s.id, "err", err)
		s.sendError("Microphone unavailable: voice codec failed to initialise")
		return
	}
	for {
		pkt, _, readErr := track.ReadRTP()
		if readErr != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "voicertc: audio read failed", "session", s.id, "err", readErr)
				s.sendError("Microphone lost: " + readErr.Error())
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
		chunk := geminiAudioChunk{}
		chunk.RealtimeInput.Audio = geminiBlob{
			MimeType: fmt.Sprintf("audio/pcm;rate=%d", inputSampleRate),
			Data:     b64,
		}
		msg, err := json.Marshal(chunk)
		if err != nil {
			slog.WarnContext(ctx, "voicertc: marshal audio", "session", s.id, "err", err)
			return
		}

		s.mu.Lock()
		wsConn := s.geminiWS
		s.mu.Unlock()
		if wsConn == nil {
			return
		}
		if err := wsConn.Write(ctx, websocket.MessageText, msg); err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "voicertc: audio→gemini write failed", "session", s.id, "err", err)
			}
			return
		}
	}
}

// geminiRxLoop reads from the provider WebSocket and forwards normalized messages.
//
// When the client has an RTP audio track: audio chunks are appended to audioBuf
// for real-time playback by audioSendLoop, and non-audio content goes through
// the data channel. Provider-specific JSON never reaches the client.
func (s *session) geminiRxLoop(ctx context.Context) {
	var enc *opusEncoder
	if s.audioTrack != nil {
		var err error
		enc, err = newEncoder()
		if err != nil {
			slog.WarnContext(ctx, "voicertc: encoder init failed", "session", s.id, "err", err)
			s.sendError("Voice audio unavailable: codec failed to initialise")
			// Don't cancel — let the client see the error and disconnect.
			return
		}
		go s.audioSendLoop(ctx, enc)
	}

	for {
		_, data, err := s.geminiWS.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "voicertc: gemini read failed", "session", s.id, "err", err)
				s.sendError("Connection to Gemini lost: " + geminiCloseReason(err))
			}
			// Don't cancel here — the session stays alive so the error reaches the
			// client via the data channel. The client will disconnect on error,
			// which closes the data channel and triggers cleanup via dc.OnClose.
			return
		}

		// When encoder is available, intercept serverContent audio and send via RTP.
		if enc != nil {
			if modified, ok := s.handleAudioExtraction(data); ok {
				data = modified
			}
		}

		if geminiHasSetupComplete(data) {
			s.markGeminiSetupComplete(ctx)
		}

		clientMessages, err := translateGeminiServerMessage(data)
		if err != nil {
			slog.WarnContext(ctx, "voicertc: provider message translation failed", "session", s.id, "err", err)
			continue
		}
		if len(clientMessages) == 0 {
			continue
		}

		s.mu.Lock()
		dc := s.dc
		s.mu.Unlock()
		if dc == nil {
			continue
		}
		for _, clientMessage := range clientMessages {
			if err := dc.SendText(string(clientMessage)); err != nil {
				slog.Warn("voicertc: provider→dc send failed", "session", s.id, "err", err)
				s.cancel()
				return
			}
		}
	}
}

func (s *session) markGeminiSetupComplete(ctx context.Context) {
	s.mu.Lock()
	if s.geminiSetupComplete {
		s.mu.Unlock()
		return
	}
	s.geminiSetupComplete = true
	track := s.pendingTrack
	s.pendingTrack = nil
	s.mu.Unlock()
	if track != nil {
		slog.InfoContext(ctx, "voicertc: starting mic forwarding after gemini setup", "session", s.id)
		go s.audioRxLoop(ctx, track)
	}
}

func geminiHasSetupComplete(data []byte) bool {
	var msg geminiBidiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return false
	}
	return msg.SetupComplete != nil
}

// handleAudioExtraction checks if a Gemini message contains serverContent with
// inlineData audio. If so, it appends the PCM bytes to the audio buffer for
// real-time playback by audioSendLoop, and returns a modified message with the
// audio stripped. On interrupted=true it clears the buffer immediately.
// Returns (modifiedData, true) if audio was extracted, (nil, false) otherwise.
func (s *session) handleAudioExtraction(data []byte) ([]byte, bool) {
	var msg geminiBidiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, false
	}
	if msg.ServerContent == nil {
		return nil, false
	}

	if msg.ServerContent.Interrupted {
		s.interruptAudio()
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
			s.sendError("Unexpected audio format from Gemini: " + mt)
			s.cancel()
			return nil, false
		}
		pcmBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			slog.Debug("voicertc: base64 decode failed", "session", s.id, "err", err)
			continue
		}
		s.appendAudioPCM(pcmBytes)
	}
	if !hadAudio {
		return nil, false
	}

	// Rebuild the message without audio parts.
	msg.ServerContent.ModelTurn.Parts = filteredParts
	rebuilt, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return rebuilt, true
}

// audioSendLoop drains audioBuf one 20ms frame at a time on a fixed ticker.
//
// Gemini sends audio as a stream of small PCM chunks. Each chunk is appended to
// audioBuf by appendAudioPCM; we drain it here at exactly realtime rate so the
// receiver's jitter buffer sees a steady arrival schedule. When the buffer is
// empty (between utterances) the tick is a no-op, producing a natural pause.
func (s *session) audioSendLoop(ctx context.Context, enc *opusEncoder) {
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.audioMu.Lock()
			if len(s.audioBuf) < inputFrameBytes {
				s.audioMu.Unlock()
				continue
			}
			frame := make([]byte, inputFrameBytes)
			copy(frame, s.audioBuf[:inputFrameBytes])
			s.audioBuf = s.audioBuf[inputFrameBytes:]
			s.audioMu.Unlock()

			pcm48 := upsample24to48(frame)
			opusPkt, err := enc.Encode(pcm48)
			if err != nil {
				slog.Debug("voicertc: opus encode failed", "session", s.id, "err", err)
				continue
			}
			if err := s.audioTrack.WriteSample(media.Sample{
				Data:     opusPkt,
				Duration: frameDuration,
			}); err != nil {
				slog.Debug("voicertc: rtp write failed", "session", s.id, "err", err)
				s.cancel()
				return
			}
		}
	}
}

// appendAudioPCM appends raw 24kHz S16LE PCM bytes from Gemini to the playback buffer.
func (s *session) appendAudioPCM(pcmBytes []byte) {
	s.audioMu.Lock()
	s.audioBuf = append(s.audioBuf, pcmBytes...)
	s.audioMu.Unlock()
}

// interruptAudio clears the playback buffer when Gemini signals an interruption.
func (s *session) interruptAudio() {
	s.audioMu.Lock()
	s.audioBuf = nil
	s.audioMu.Unlock()
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

// sendError delivers a normalized error message to the client via the data channel.
func (s *session) sendError(msg string) {
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	if dc == nil {
		return
	}
	data := mustGatewayServerMessage(&voicev1.Error{
		Kind:        voicev1.MessageKindError,
		Message:     msg,
		Recoverable: false,
	})
	_ = dc.SendText(string(data))
}

// upsample24to48 converts PCM from 24kHz S16LE to 48kHz using linear
// interpolation (factor 2).
func upsample24to48(pcmBytes []byte) []int16 {
	samples24 := len(pcmBytes) / 2
	pcm48 := make([]int16, samples24*2)
	for i := range samples24 {
		s0 := int16(binary.LittleEndian.Uint16(pcmBytes[i*2:])) //nolint:gosec // PCM uint16→int16 reinterpret
		pcm48[i*2] = s0
		if i+1 < samples24 {
			s1 := int16(binary.LittleEndian.Uint16(pcmBytes[(i+1)*2:])) //nolint:gosec // PCM uint16→int16 reinterpret
			pcm48[i*2+1] = int16((int32(s0) + int32(s1)) / 2)           //nolint:gosec // sum/2 fits in int16
		} else {
			pcm48[i*2+1] = s0
		}
	}
	return pcm48
}

// close tears down the Gemini WebSocket and PeerConnection.
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
