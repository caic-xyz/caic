// WebRTC voice session bridge for the voice gateway protocol.

// Package voicertc implements WebRTC voice sessions through the voice gateway protocol.
package voicertc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

const (
	// idleTimeout closes sessions after 30 minutes of inactivity.
	idleTimeout = 30 * time.Minute

	// micSampleRate is the decoded microphone PCM rate.
	micSampleRate = 16000

	// backendOutputSampleRate is the assistant PCM rate consumed by the RTP encoder.
	backendOutputSampleRate = 24000

	// encoderSampleRate is the Opus encoder input rate. We encode at 48kHz —
	// the native Opus rate that every WebRTC implementation handles.
	encoderSampleRate = 48000

	// frameDuration is the Opus frame duration.
	frameDuration = 20 * time.Millisecond

	// encoderFrameSamples is the number of samples per 20ms frame at the encoder rate.
	encoderFrameSamples = encoderSampleRate * int(frameDuration/time.Millisecond) / 1000 // 960

	// backendOutputFrameBytes is one 20ms frame of backend PCM output.
	backendOutputFrameBytes = backendOutputSampleRate * 2 * int(frameDuration/time.Millisecond) / 1000 // 960 bytes
)

// Bridge manages active WebRTC voice sessions for a single configured backend.
type Bridge struct {
	backend  backendConnector
	api      *webrtc.API
	udpMux   ice.UDPMux
	mu       sync.Mutex
	sessions map[string]*session
}

// NewBridge creates a Bridge that multiplexes all WebRTC traffic through a
// single UDP port. This avoids opening ephemeral port ranges in the firewall.
//
// A gateway instance serves exactly the backend named in cfg. geminiAPIKey is
// only consumed by the Gemini Live backend.
func NewBridge(ctx context.Context, cfg *voicegateway.Config, geminiAPIKey string, udpPort int) (*Bridge, error) {
	backend, err := backendForConfig(ctx, cfg, geminiAPIKey)
	if err != nil {
		return nil, err
	}
	b, err := newBridgeWithBackend(ctx, backend, udpPort)
	if err != nil {
		if cerr := backend.Close(); cerr != nil {
			slog.Warn("voicertc: close backend", "err", cerr)
		}
		return nil, err
	}
	return b, nil
}

// backendForConfig constructs the single backend a gateway instance serves.
func backendForConfig(ctx context.Context, cfg *voicegateway.Config, geminiAPIKey string) (backendConnector, error) {
	switch cfg.Backend {
	case voicegateway.BackendGeminiLive:
		return newGeminiBridgeBackend(geminiAPIKey)
	case voicegateway.BackendLocalStack:
		return localStackBackendForConfig(ctx, &cfg.LocalStack)
	default:
		return nil, fmt.Errorf("unknown voice backend %q", cfg.Backend)
	}
}

func newBridgeWithBackend(ctx context.Context, backend backendConnector, udpPort int) (*Bridge, error) {
	if backend == nil {
		return nil, errors.New("voice backend is required")
	}
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
		backend:  backend,
		api:      api,
		udpMux:   mux,
		sessions: make(map[string]*session),
	}, nil
}

// HandleOffer processes a WebRTC SDP offer and returns the SDP answer.
func (b *Bridge) HandleOffer(ctx context.Context, sdpOffer string) (sdpAnswer, sessionID string, err error) {
	if !strings.Contains(sdpOffer, "m=audio ") {
		return "", "", errors.New("SDP offer must include an audio track")
	}
	connector := b.backend

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
		id:      generateSessionID(),
		pc:      pc,
		backend: nil,
		cancel:  cancel,
	}

	// Set up RTP audio track (server → client).
	audioTrack, trackErr := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "assistant-voice",
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
		if sess.backendSetupComplete {
			sess.mu.Unlock()
			go sess.audioRxLoop(sessionCtx, track)
		} else {
			// Store the track until the backend accepts setup. Sending realtime
			// audio before setupComplete can make realtime backends reject the session.
			sess.pendingTrack = track
			sess.mu.Unlock()
		}
	})

	// Set up data channel handler. The client creates the "voice-gateway" data channel.
	// backendConnected is closed once the backend session is connected, unblocking
	// any client messages that arrived before the dial completed.
	backendConnected := make(chan struct{})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		slog.Info("voicertc: data channel opened", "label", dc.Label(), "session", sess.id)
		sess.mu.Lock()
		sess.dc = dc
		sess.mu.Unlock()

		dc.OnOpen(func() {
			if err := sess.startAudioOutput(sessionCtx); err != nil {
				slog.WarnContext(sessionCtx, "voicertc: encoder init failed", "session", sess.id, "err", err)
				sess.sendError("Voice audio unavailable: codec failed to initialise")
			}
			backendSession, err := connector.connect(sessionCtx, sess.id, sess)
			if err != nil {
				slog.Error("voicertc: backend connect failed", "session", sess.id, "err", err)
				sess.sendError("Failed to connect voice backend: " + err.Error())
				cancel()
				return
			}
			sess.mu.Lock()
			sess.backend = backendSession
			sess.mu.Unlock()
			close(backendConnected)
			slog.Info("voicertc: backend connected", "session", sess.id)
		})

		// Data channel → backend adapter. Blocks until the backend is connected
		// so the client's session.setup message is never dropped.
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			select {
			case <-backendConnected:
			case <-sessionCtx.Done():
				return
			}
			sess.mu.Lock()
			backendSession := sess.backend
			sess.mu.Unlock()
			if backendSession == nil {
				return
			}
			err := backendSession.acceptClientMessage(sessionCtx, msg.Data)
			if err != nil {
				if errors.Is(err, errSessionClosed) {
					cancel()
					return
				}
				slog.Warn("voicertc: gateway client message failed", "session", sess.id, "err", err)
				sess.sendError(err.Error())
				return
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
	if err := b.backend.Close(); err != nil {
		slog.Warn("voicertc: close backend", "err", err)
	}
}

// session holds all state for one bridge session.
type session struct {
	id string

	mu                   sync.Mutex
	pc                   *webrtc.PeerConnection
	dc                   *webrtc.DataChannel
	audioTrack           *webrtc.TrackLocalStaticSample
	backend              backendSession
	pendingTrack         *webrtc.TrackRemote // set by OnTrack, consumed after backend setupComplete
	backendSetupComplete bool
	audioOutputStarted   bool
	cancel               context.CancelFunc

	audioMu  sync.Mutex
	audioBuf []byte // pending backend PCM bytes, drained by audioSendLoop
}

func (s *session) cancelSession() {
	s.cancel()
}

func (s *session) backendReady(ctx context.Context) {
	s.mu.Lock()
	if s.backendSetupComplete {
		s.mu.Unlock()
		return
	}
	s.backendSetupComplete = true
	track := s.pendingTrack
	s.pendingTrack = nil
	s.mu.Unlock()

	// The gateway core owns session.ready, a provider-neutral readiness signal.
	if err := s.sendGatewayMessage(ctx, gatewaySessionReady()); err != nil {
		slog.WarnContext(ctx, "voicertc: session.ready send failed", "session", s.id, "err", err)
	}

	if track != nil {
		slog.InfoContext(ctx, "voicertc: starting mic forwarding after backend setup", "session", s.id)
		go s.audioRxLoop(ctx, track)
	}
}

func (s *session) sendGatewayMessage(_ context.Context, data []byte) error {
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	if dc == nil {
		return nil
	}
	return dc.SendText(string(data))
}

func (s *session) sendGatewayError(message string) {
	s.sendError(message)
}

func (s *session) addAssistantPCM(pcmBytes []byte) {
	s.audioMu.Lock()
	s.audioBuf = append(s.audioBuf, pcmBytes...)
	s.audioMu.Unlock()
}

func (s *session) clearAssistantAudio() {
	s.audioMu.Lock()
	s.audioBuf = nil
	s.audioMu.Unlock()
}

// audioRxLoop reads Opus RTP from the client's mic track, decodes to PCM,
// and forwards PCM to the active backend.
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
		// Convert int16 PCM to little-endian bytes for the backend adapter.
		pcmBytes := make([]byte, len(pcm)*2)
		for i, sample := range pcm {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(sample)) //nolint:gosec // PCM int16→uint16 reinterpret is intentional
		}

		s.mu.Lock()
		backendSession := s.backend
		s.mu.Unlock()
		if backendSession == nil {
			return
		}
		if err := backendSession.acceptMicPCM(ctx, pcmBytes); err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "voicertc: audio→backend write failed", "session", s.id, "err", err)
			}
			return
		}
	}
}

func (s *session) startAudioOutput(ctx context.Context) error {
	s.mu.Lock()
	if s.audioOutputStarted {
		s.mu.Unlock()
		return nil
	}
	s.audioOutputStarted = true
	audioTrack := s.audioTrack
	s.mu.Unlock()
	if audioTrack == nil {
		return nil
	}
	enc, err := newEncoder()
	if err != nil {
		return err
	}
	go s.audioSendLoop(ctx, enc)
	return nil
}

// audioSendLoop drains audioBuf one 20ms frame at a time on a fixed ticker.
//
// Backends send audio as a stream of small PCM chunks. Each chunk is appended
// to audioBuf; we drain it here at exactly realtime rate so the receiver's
// jitter buffer sees a steady arrival schedule. When the buffer is empty
// (between utterances) the tick is a no-op, producing a natural pause.
func (s *session) audioSendLoop(ctx context.Context, enc *opusEncoder) {
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.audioMu.Lock()
			if len(s.audioBuf) < backendOutputFrameBytes {
				s.audioMu.Unlock()
				continue
			}
			frame := make([]byte, backendOutputFrameBytes)
			copy(frame, s.audioBuf[:backendOutputFrameBytes])
			s.audioBuf = s.audioBuf[backendOutputFrameBytes:]
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

// close tears down the backend session and PeerConnection.
func (s *session) close() {
	s.mu.Lock()
	backendSession := s.backend
	s.backend = nil
	pc := s.pc
	s.pc = nil
	s.mu.Unlock()
	if backendSession != nil {
		_ = backendSession.close()
	}
	if pc != nil {
		_ = pc.Close()
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
