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
	"net"
	"net/url"
	"strings"
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

	// inputSampleRate matches Gemini's required input rate (PCM 16-bit, 16kHz).
	inputSampleRate = 16000

	// encoderSampleRate is the Opus encoder input rate. We encode at 48kHz —
	// the native Opus rate that every WebRTC implementation handles. Gemini
	// outputs 24kHz; we upsample before encoding.
	encoderSampleRate = 48000

	// frameDuration is the Opus frame duration.
	frameDuration = 20 * time.Millisecond

	// encoderFrameSamples is the number of samples per 20ms frame at the encoder rate.
	encoderFrameSamples = encoderSampleRate * int(frameDuration/time.Millisecond) / 1000 // 960
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
	// Discover the host's default IPv4 address without netlink (which fails
	// on hosts without IPv6 due to pion/anet's netlinkrib call).
	hostIP, err := defaultIPv4()
	if err != nil {
		return nil, fmt.Errorf("detect host IP: %w", err)
	}
	conn, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", udpPort))
	if err != nil {
		return nil, fmt.Errorf("listen UDP4 :%d: %w", udpPort, err)
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	se.SetNet(newIPv4Net(hostIP))
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
			sess.mu.Lock()
			if sess.geminiWS != nil {
				// Gemini already connected (data channel opened first). Start now.
				sess.mu.Unlock()
				go sess.audioRxLoop(sessionCtx, track)
			} else {
				// Gemini not connected yet. Store the track; dc.OnOpen will start it.
				sess.pendingTrack = track
				sess.mu.Unlock()
			}
		})
	}

	// Set up data channel handler. The client creates the "gemini" data channel.
	// geminiReady is closed once the Gemini WebSocket is connected, unblocking
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
				cancel()
				return
			}
			wsConn.SetReadLimit(wsReadLimit)
			sess.mu.Lock()
			sess.geminiWS = wsConn
			track := sess.pendingTrack
			sess.pendingTrack = nil
			sess.mu.Unlock()
			close(geminiReady)
			slog.Info("voicertc: gemini connected", "session", sess.id, "rtpAudio", codecAvailable)

			// Start mic → Gemini forwarding if the track arrived before Gemini connected.
			if track != nil {
				go sess.audioRxLoop(sessionCtx, track)
			}

			// Start Gemini → data channel / RTP forwarding.
			go sess.geminiRxLoop(sessionCtx)
		})

		// Data channel → Gemini passthrough. Blocks until Gemini is connected
		// so the client's setup message is never dropped.
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

// audioJob represents one utterance to be sent via RTP.
type audioJob struct {
	ctx      context.Context
	pcmBytes []byte
}

// session holds all state for one bridge session.
type session struct {
	id string

	mu             sync.Mutex
	pc             *webrtc.PeerConnection
	dc             *webrtc.DataChannel
	audioTrack     *webrtc.TrackLocalStaticSample
	geminiWS       *websocket.Conn
	pendingTrack   *webrtc.TrackRemote // set by OnTrack, consumed after geminiWS connects
	cancel         context.CancelFunc
	audioJobCancel context.CancelFunc // cancels the currently-processing audio job
	audioJobs      chan *audioJob     // single consumer: audioSendLoop
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
					"mimeType": fmt.Sprintf("audio/pcm;rate=%d", inputSampleRate),
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
//
// When the client has an RTP audio track: audio chunks are encoded to Opus
// and sent via RTP, non-audio content goes through the data channel.
// Otherwise everything goes through the data channel (passthrough).
//
// A single consumer goroutine processes audioJobs serially so the Opus
// encoder is never used concurrently.
func (s *session) geminiRxLoop(ctx context.Context) {
	var enc *opusEncoder
	if codecAvailable && s.audioTrack != nil {
		var err error
		enc, err = newEncoder()
		if err != nil {
			slog.Warn("voicertc: encoder init failed, falling back to passthrough", "session", s.id, "err", err)
		} else {
			s.audioJobs = make(chan *audioJob, 1)
			go s.audioSendLoop(ctx, enc)
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
			if modified, ok := s.handleAudioExtraction(ctx, data); ok {
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
func (s *session) handleAudioExtraction(ctx context.Context, data []byte) ([]byte, bool) {
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
		// Decode base64 PCM and enqueue as a job for the audioSendLoop.
		pcmBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			slog.Debug("voicertc: base64 decode failed", "session", s.id, "err", err)
			continue
		}
		s.enqueueAudioJob(ctx, pcmBytes)
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

// audioSendLoop is the single consumer for audioJobs. It serializes all
// encoding and RTP writes so the Opus encoder is never used concurrently.
func (s *session) audioSendLoop(ctx context.Context, enc *opusEncoder) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-s.audioJobs:
			if !ok {
				return
			}
			s.encodeAndSendRTP(job.ctx, job.pcmBytes, enc) //nolint:contextcheck // job.ctx is created in enqueueAudioJob, used here by the single consumer
		}
	}
}

// enqueueAudioJob cancels any in-flight audio job and queues a new one.
// The audioSendLoop picks it up and processes it serially.
func (s *session) enqueueAudioJob(parentCtx context.Context, pcmBytes []byte) {
	s.mu.Lock()
	if s.audioJobCancel != nil {
		s.audioJobCancel()
	}
	// Drain any queued-but-not-started job.
	select {
	case <-s.audioJobs:
	default:
	}
	jobCtx, jobCancel := context.WithCancel(context.WithoutCancel(parentCtx))
	s.audioJobCancel = jobCancel
	s.mu.Unlock()
	s.audioJobs <- &audioJob{ctx: jobCtx, pcmBytes: pcmBytes}
}

// encodeAndSendRTP converts Gemini PCM (24kHz S16LE) to Opus and writes to
// the RTP audio track with frame pacing.
//
// Pacing is critical: without it, all Opus frames for an utterance are written
// in a tight loop, flooding the receiver's jitter buffer and causing stutter.
// Each 20ms frame is spaced ~20ms apart so the receiver's NetEq sees a realistic
// arrival schedule.
func (s *session) encodeAndSendRTP(ctx context.Context, pcmBytes []byte, enc *opusEncoder) {
	// Gemini outputs 24kHz PCM. Upsample to 48kHz for the Opus encoder.
	pcm48 := upsample24to48(pcmBytes)

	// Pre-encode all frames so pacing isn't affected by encoding latency.
	var frames [][]byte
	for i := 0; i+encoderFrameSamples <= len(pcm48); i += encoderFrameSamples {
		opusPkt, err := enc.Encode(pcm48[i : i+encoderFrameSamples])
		if err != nil {
			slog.Debug("voicertc: opus encode failed", "session", s.id, "err", err)
			continue
		}
		frames = append(frames, opusPkt)
	}

	// Pace writes at ~20ms per frame so the receiver's jitter buffer
	// doesn't get flooded.
	deadline := time.Now()
	for _, f := range frames {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Sleep until the deadline, then advance by one frame duration.
		if wait := time.Until(deadline); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		deadline = time.Now().Add(frameDuration)

		if err := s.audioTrack.WriteSample(media.Sample{
			Data:     f,
			Duration: frameDuration,
		}); err != nil {
			slog.Debug("voicertc: rtp write failed", "session", s.id, "err", err)
			return
		}
	}
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
