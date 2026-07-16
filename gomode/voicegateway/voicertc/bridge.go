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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/caic-xyz/caic/gomode/voicegateway"
	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
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
	udpMux   ice.UDPMux
	voiceNet *ipv4Net
	hostIP   net.IP
	udpPort  int

	setupMu             sync.Mutex
	api                 *webrtc.API
	upnpMapping         *upnpMapping
	advertisedEndpoints []udpCandidate
	udpMappingError     string

	sessionsMu sync.Mutex
	sessions   map[string]*session
}

type udpCandidate struct {
	host net.IP
	port int
}

// NewBridge creates a Bridge that multiplexes WebRTC traffic through a single
// UDP port and eagerly verifies its UPnP publication.
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
			slog.WarnContext(ctx, "voicertc: close backend", "err", cerr)
		}
		return nil, err
	}
	if _, err := b.ensureWebRTCAPI(ctx); err != nil {
		b.CloseAll(ctx)
		return nil, fmt.Errorf("initialize WebRTC API: %w", err)
	}
	return b, nil
}

// backendForConfig constructs the single backend a gateway instance serves.
func backendForConfig(ctx context.Context, cfg *voicegateway.Config, geminiAPIKey string) (backendConnector, error) {
	switch cfg.Backend {
	case voicegateway.BackendGeminiLive:
		if geminiAPIKey == "" {
			return nil, errors.New("GEMINI_API_KEY not configured")
		}
		return &geminiBridgeBackend{apiKey: geminiAPIKey}, nil
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
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected local address type: %T", conn.LocalAddr())
	}
	candidateIPs := iceCandidateIPv4s(hostIP)
	voiceNet := newIPv4Net(ctx, candidateIPs)
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn, Net: voiceNet})
	slog.InfoContext(ctx, "voicertc: listening", "udpPort", addr.Port, "hostIP", hostIP)
	return &Bridge{
		backend:             backend,
		udpMux:              mux,
		voiceNet:            voiceNet,
		hostIP:              append(net.IP(nil), hostIP...),
		advertisedEndpoints: udpCandidates(candidateIPs, addr.Port),
		udpPort:             addr.Port,
		sessions:            make(map[string]*session),
	}, nil
}

// HandleOffer processes a WebRTC SDP offer and returns the SDP answer.
func (b *Bridge) HandleOffer(ctx context.Context, sdpOffer string) (sdpAnswer, sessionID string, err error) {
	if !strings.Contains(sdpOffer, "m=audio ") {
		return "", "", errors.New("SDP offer must include an audio track")
	}
	api, err := b.ensureWebRTCAPI(ctx)
	if err != nil {
		return "", "", err
	}
	connector := b.backend

	// Create PeerConnection using the shared UDP mux. The gateway publishes a
	// host candidate rewritten to the UPnP external address when available; the
	// client owns STUN for its side. Server-side STUN would re-enter Pion's
	// default interface discovery and fail on hosts without IPv6 netlink support.
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", "", fmt.Errorf("create peer connection: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := &session{
		id:                  generateSessionID(),
		pc:                  pc,
		backend:             nil,
		iceConnectionState:  voicev1.VoiceRTCICEConnectionState(pc.ICEConnectionState().String()),
		iceGatheringState:   voicev1.VoiceRTCICEGatheringState(pc.ICEGatheringState().String()),
		signalingState:      voicev1.VoiceRTCSignalingState(pc.SignalingState().String()),
		peerConnectionState: voicev1.VoiceRTCConnectionState(pc.ConnectionState().String()),
		dataChannelState:    voicev1.VoiceRTCDataChannelStateNew,
		cancel:              cancel,
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
		slog.InfoContext(sessionCtx, "voicertc: audio track received", "session", sess.id, "codec", track.Codec().MimeType)
		sess.markAudioTrackReceived()
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
		slog.InfoContext(sessionCtx, "voicertc: data channel opened", "label", dc.Label(), "session", sess.id)
		sess.mu.Lock()
		sess.dc = dc
		sess.dataChannelState = voicev1.VoiceRTCDataChannelState(dc.ReadyState().String())
		sess.mu.Unlock()

		dc.OnOpen(func() {
			sess.setDataChannelState(dc.ReadyState().String())
			if err := sess.startAudioOutput(sessionCtx); err != nil {
				slog.WarnContext(sessionCtx, "voicertc: encoder init failed", "session", sess.id, "err", err)
				sess.sendError("Voice audio unavailable: codec failed to initialise")
			}
			backendSession, err := connector.connect(sessionCtx, sess.id, sess)
			if err != nil {
				slog.ErrorContext(sessionCtx, "voicertc: backend connect failed", "session", sess.id, "err", err)
				sess.sendError("Failed to connect voice backend: " + err.Error())
				cancel()
				return
			}
			sess.mu.Lock()
			sess.backend = backendSession
			sess.backendConnected = true
			sess.mu.Unlock()
			close(backendConnected)
			slog.InfoContext(sessionCtx, "voicertc: backend connected", "session", sess.id)
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
				slog.WarnContext(sessionCtx, "voicertc: gateway client message failed", "session", sess.id, "err", err)
				sess.sendError(err.Error())
				return
			}
		})

		dc.OnClose(func() {
			slog.InfoContext(sessionCtx, "voicertc: data channel closed", "session", sess.id)
			sess.setDataChannelState(dc.ReadyState().String())
			cancel()
		})
	})

	// Monitor ICE connection state.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.DebugContext(sessionCtx, "voicertc: ICE state", "session", sess.id, "state", state.String())
		sess.setICEConnectionState(state.String())
		//exhaustive:ignore
		switch state {
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateClosed:
			cancel()
		default:
		}
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		sess.setICEGatheringState(state.String())
	})
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		sess.setSignalingState(state.String())
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		sess.setPeerConnectionState(state.String())
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
	b.sessionsMu.Lock()
	b.sessions[sess.id] = sess
	b.sessionsMu.Unlock()

	// Background cleanup.
	go func() {
		defer func() {
			b.sessionsMu.Lock()
			delete(b.sessions, sess.id)
			b.sessionsMu.Unlock()
			sess.close()
			slog.InfoContext(sessionCtx, "voicertc: session cleaned up", "session", sess.id)
		}()

		idleTimer := time.NewTimer(idleTimeout)
		defer idleTimer.Stop()

		select {
		case <-sessionCtx.Done():
		case <-idleTimer.C:
			slog.InfoContext(sessionCtx, "voicertc: idle timeout", "session", sess.id)
		}
	}()

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		_ = pc.Close()
		cancel()
		return "", "", errors.New("no local description after ICE gathering")
	}
	return b.rewriteMappedCandidatePort(localDesc.SDP), sess.id, nil
}

// DiagnoseVoiceRTC returns structured connectivity diagnostics for a session.
func (b *Bridge) DiagnoseVoiceRTC(_ context.Context, sessionID string, client *voicev1.VoiceRTCClientDiagnostics) voicev1.VoiceRTCDiagnosticsResp {
	udpEndpoints, udpMappingError := b.udpDiagnostics()
	b.sessionsMu.Lock()
	sess := b.sessions[sessionID]
	b.sessionsMu.Unlock()
	if sess == nil {
		server := voicev1.VoiceRTCServerDiagnostics{
			SessionFound:    false,
			UDPEndpoints:    udpEndpoints,
			UDPMappingError: udpMappingError,
		}
		return buildVoiceRTCDiagnostics(sessionID, &server, client)
	}
	server := sess.diagnostics(udpEndpoints, udpMappingError)
	return buildVoiceRTCDiagnostics(sessionID, &server, client)
}

// Close tears down a session by ID. No-op if not found.
func (b *Bridge) Close(sessionID string) {
	b.sessionsMu.Lock()
	sess, ok := b.sessions[sessionID]
	if ok {
		delete(b.sessions, sessionID)
	}
	b.sessionsMu.Unlock()
	if ok {
		sess.cancel()
		sess.close()
	}
}

// CloseAll tears down all sessions and the UDP mux. Called on server shutdown.
func (b *Bridge) CloseAll(ctx context.Context) {
	b.sessionsMu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.sessions = make(map[string]*session)
	b.sessionsMu.Unlock()
	for _, s := range sessions {
		s.cancel()
		s.close()
	}
	b.setupMu.Lock()
	upnpMapping := b.upnpMapping
	b.upnpMapping = nil
	b.api = nil
	b.advertisedEndpoints = nil
	b.udpMappingError = ""
	b.setupMu.Unlock()
	if b.udpMux != nil {
		_ = b.udpMux.Close()
	}
	if upnpMapping != nil {
		if err := upnpMapping.close(ctx); err != nil {
			slog.WarnContext(ctx, "voicertc: delete UPnP mapping", "err", err)
		}
	}
	if err := b.backend.Close(); err != nil {
		slog.Warn("voicertc: close backend", "err", err)
	}
}

func (b *Bridge) ensureWebRTCAPI(ctx context.Context) (*webrtc.API, error) {
	b.setupMu.Lock()
	defer b.setupMu.Unlock()
	if b.api != nil {
		return b.api, nil
	}
	advertisedEndpoints := append([]udpCandidate(nil), b.advertisedEndpoints...)
	mapping, err := mapUPnPUDP(ctx, b.hostIP, b.udpPort)
	if err != nil {
		b.udpMappingError = fmt.Sprintf("UPnP UDP mapping failed: %v", err)
		slog.ErrorContext(ctx, "voicertc: UPnP UDP mapping failed; falling back to local ICE address", "udpPort", b.udpPort, "err", err)
	} else {
		b.udpMappingError = ""
		updated := appendUniqueUDPCandidate(nil, udpCandidate{host: mapping.ip, port: int(mapping.externalPort)})
		for _, c := range advertisedEndpoints {
			updated = appendUniqueUDPCandidate(updated, c)
		}
		advertisedEndpoints = updated
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(b.udpMux)
	se.SetNet(b.voiceNet)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if mapping != nil {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{mapping.ip.String()},
			Local:           b.hostIP.String(),
			AsCandidateType: webrtc.ICECandidateTypeSrflx,
			Mode:            webrtc.ICEAddressRewriteAppend,
		}); err != nil {
			_ = mapping.close(ctx)
			return nil, fmt.Errorf("set ICE address rewrite: %w", err)
		}
	}
	b.api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	b.upnpMapping = mapping
	b.advertisedEndpoints = advertisedEndpoints
	slog.InfoContext(ctx, "voicertc: WebRTC API ready", "udpPort", b.udpPort, "hostIP", b.hostIP, "advertisedEndpoints", udpEndpointStrings(advertisedEndpoints), "upnpMapped", mapping != nil)
	return b.api, nil
}

func (b *Bridge) udpDiagnostics() (endpoints []voicev1.VoiceRTCUDPEndpoint, mappingError string) {
	b.setupMu.Lock()
	mapping := b.upnpMapping
	endpoints = voiceRTCUDPEndpoints(b.advertisedEndpoints)
	mappingError = b.udpMappingError
	b.setupMu.Unlock()
	if mappingError != "" {
		return endpoints, mappingError
	}
	if mapping == nil {
		return endpoints, ""
	}
	return endpoints, mapping.refreshError()
}

func (b *Bridge) rewriteMappedCandidatePort(sdp string) string {
	b.setupMu.Lock()
	mapping := b.upnpMapping
	b.setupMu.Unlock()
	if mapping == nil {
		return sdp
	}
	return rewriteSDPMappedCandidates(sdp, mapping.ip.String(), int(mapping.externalPort))
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
	iceConnectionState   voicev1.VoiceRTCICEConnectionState
	iceGatheringState    voicev1.VoiceRTCICEGatheringState
	peerConnectionState  voicev1.VoiceRTCConnectionState
	signalingState       voicev1.VoiceRTCSignalingState
	dataChannelState     voicev1.VoiceRTCDataChannelState
	dataChannelOpened    bool
	audioTrackReceived   bool
	backendConnected     bool
	sessionReadySent     bool
	lastError            string

	audioMu             sync.Mutex
	audioBuf            []byte // pending backend PCM bytes, drained by audioSendLoop
	audioBufStartedAt   time.Time
	audioFirstRTPLogged bool
}

func (s *session) cancelSession() {
	s.cancel()
}

func (s *session) setICEConnectionState(state string) {
	s.mu.Lock()
	s.iceConnectionState = voicev1.VoiceRTCICEConnectionState(state)
	s.mu.Unlock()
}

func (s *session) setICEGatheringState(state string) {
	s.mu.Lock()
	s.iceGatheringState = voicev1.VoiceRTCICEGatheringState(state)
	s.mu.Unlock()
}

func (s *session) setPeerConnectionState(state string) {
	s.mu.Lock()
	s.peerConnectionState = voicev1.VoiceRTCConnectionState(state)
	s.mu.Unlock()
}

func (s *session) setSignalingState(state string) {
	s.mu.Lock()
	s.signalingState = voicev1.VoiceRTCSignalingState(state)
	s.mu.Unlock()
}

func (s *session) setDataChannelState(state string) {
	s.mu.Lock()
	s.dataChannelState = voicev1.VoiceRTCDataChannelState(state)
	if strings.EqualFold(state, "open") {
		s.dataChannelOpened = true
	}
	s.mu.Unlock()
}

func (s *session) markAudioTrackReceived() {
	s.mu.Lock()
	s.audioTrackReceived = true
	s.mu.Unlock()
}

func (s *session) diagnostics(endpoints []voicev1.VoiceRTCUDPEndpoint, mappingError string) voicev1.VoiceRTCServerDiagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return voicev1.VoiceRTCServerDiagnostics{
		SessionFound:       true,
		UDPEndpoints:       endpoints,
		UDPMappingError:    mappingError,
		ICEConnectionState: s.iceConnectionState,
		ICEGatheringState:  s.iceGatheringState,
		ConnectionState:    s.peerConnectionState,
		SignalingState:     s.signalingState,
		DataChannelState:   s.dataChannelState,
		DataChannelOpened:  s.dataChannelOpened,
		AudioTrackReceived: s.audioTrackReceived,
		BackendConnected:   s.backendConnected,
		SessionReadySent:   s.sessionReadySent,
		LastError:          s.lastError,
	}
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
	s.mu.Lock()
	s.sessionReadySent = true
	s.mu.Unlock()
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
	if len(pcmBytes) == 0 {
		return
	}
	s.audioMu.Lock()
	if len(s.audioBuf) == 0 {
		s.audioBufStartedAt = time.Now()
		s.audioFirstRTPLogged = false
	}
	s.audioBuf = append(s.audioBuf, pcmBytes...)
	buffered := assistantPCMDuration(len(s.audioBuf))
	s.audioMu.Unlock()
	slog.Debug("voicertc: assistant pcm buffered", "session", s.id, "buffered", buffered, "bytes", len(pcmBytes))
}

func (s *session) clearAssistantAudio() {
	s.audioMu.Lock()
	s.audioBuf = nil
	s.audioBufStartedAt = time.Time{}
	s.audioFirstRTPLogged = false
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
			slog.DebugContext(ctx, "voicertc: opus decode failed", "session", s.id, "err", decErr)
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
			queuedAt := s.audioBufStartedAt
			logFirstRTP := !s.audioFirstRTPLogged && !queuedAt.IsZero()
			if logFirstRTP {
				s.audioFirstRTPLogged = true
			}
			buffered := assistantPCMDuration(len(s.audioBuf))
			if len(s.audioBuf) == 0 {
				s.audioBufStartedAt = time.Time{}
				s.audioFirstRTPLogged = false
			}
			s.audioMu.Unlock()
			if logFirstRTP {
				slog.InfoContext(ctx, "voicertc: assistant first rtp", "session", s.id, "latency", time.Since(queuedAt), "buffered", buffered)
			}

			pcm48 := upsample24to48(frame)
			opusPkt, err := enc.Encode(pcm48)
			if err != nil {
				slog.DebugContext(ctx, "voicertc: opus encode failed", "session", s.id, "err", err)
				continue
			}
			if err := s.audioTrack.WriteSample(media.Sample{
				Data:     opusPkt,
				Duration: frameDuration,
			}); err != nil {
				slog.DebugContext(ctx, "voicertc: rtp write failed", "session", s.id, "err", err)
				s.cancel()
				return
			}
		}
	}
}

// sendError delivers a normalized error message to the client via the data channel.
func (s *session) sendError(msg string) {
	s.mu.Lock()
	s.lastError = msg
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

func buildVoiceRTCDiagnostics(sessionID string, server *voicev1.VoiceRTCServerDiagnostics, client *voicev1.VoiceRTCClientDiagnostics) voicev1.VoiceRTCDiagnosticsResp {
	if client == nil {
		client = &voicev1.VoiceRTCClientDiagnostics{}
	}
	issue, side, message := classifyVoiceRTCConnectivity(server, client)
	return voicev1.VoiceRTCDiagnosticsResp{
		SessionID: sessionID,
		Issue:     issue,
		Side:      side,
		Message:   message,
		Server:    *server,
		Client:    *client,
	}
}

func rewriteSDPMappedCandidates(sdp, externalHost string, externalPort int) string {
	lines := strings.Split(sdp, "\n")
	externalPortString := strconv.Itoa(externalPort)
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\r")
		suffix := line[len(body):]
		if !strings.HasPrefix(body, "a=candidate:") {
			continue
		}
		fields := strings.Fields(body)
		if !mappedCandidateNeedsRewrite(fields) {
			continue
		}
		fields[4] = externalHost
		fields[5] = externalPortString
		lines[i] = strings.Join(fields, " ") + suffix
	}
	return strings.Join(lines, "\n")
}

func mappedCandidateNeedsRewrite(fields []string) bool {
	// The bridge has no server-side STUN configuration. Every srflx candidate
	// Pion produces therefore represents the local UDP mux and must advertise
	// its UPnP-mapped endpoint, including Pion's 0.0.0.0 placeholders.
	return len(fields) >= 8 && fields[7] == "srflx"
}

func udpCandidates(ips []net.IP, port int) []udpCandidate {
	candidates := make([]udpCandidate, 0, len(ips))
	for _, ip := range ips {
		candidates = appendUniqueUDPCandidate(candidates, udpCandidate{host: ip, port: port})
	}
	return candidates
}

func appendUniqueUDPCandidate(candidates []udpCandidate, c udpCandidate) []udpCandidate {
	v4 := c.host.To4()
	if v4 == nil || c.port <= 0 {
		return candidates
	}
	for _, existing := range candidates {
		if existing.host.Equal(v4) && existing.port == c.port {
			return candidates
		}
	}
	return append(candidates, udpCandidate{host: append(net.IP(nil), v4...), port: c.port})
}

func voiceRTCUDPEndpoints(candidates []udpCandidate) []voicev1.VoiceRTCUDPEndpoint {
	if len(candidates) == 0 {
		return nil
	}
	endpoints := make([]voicev1.VoiceRTCUDPEndpoint, 0, len(candidates))
	for _, c := range candidates {
		endpoints = append(endpoints, voicev1.VoiceRTCUDPEndpoint{Host: c.host.String(), Port: c.port})
	}
	return endpoints
}

func udpEndpointStrings(candidates []udpCandidate) []string {
	if len(candidates) == 0 {
		return nil
	}
	endpoints := make([]string, 0, len(candidates))
	for _, c := range candidates {
		endpoints = append(endpoints, net.JoinHostPort(c.host.String(), strconv.Itoa(c.port)))
	}
	return endpoints
}

func formatUDPServerCandidates(server *voicev1.VoiceRTCServerDiagnostics) string {
	if len(server.UDPEndpoints) == 0 {
		return "unknown"
	}
	candidates := make([]string, 0, len(server.UDPEndpoints))
	for _, e := range server.UDPEndpoints {
		candidates = append(candidates, net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
	}
	return strings.Join(candidates, ", ")
}

func classifyVoiceRTCConnectivity(server *voicev1.VoiceRTCServerDiagnostics, client *voicev1.VoiceRTCClientDiagnostics) (voicev1.VoiceRTCConnectivityIssue, voicev1.VoiceRTCConnectivitySide, string) {
	if !server.SessionFound {
		return voicev1.VoiceRTCConnectivityIssueServerSessionMissing,
			voicev1.VoiceRTCConnectivitySideServer,
			"server no longer has this voice RTC session"
	}
	serverICE := strings.ToLower(string(server.ICEConnectionState))
	clientICE := strings.ToLower(string(client.ICEConnectionState))
	clientConn := strings.ToLower(string(client.ConnectionState))
	if serverICE == "failed" || serverICE == "disconnected" || serverICE == "closed" {
		return voicev1.VoiceRTCConnectivityIssueServerICEFailed,
			voicev1.VoiceRTCConnectivitySideNetwork,
			"server ICE failed; UDP packets are not flowing reliably between client and server"
	}
	if !server.DataChannelOpened {
		if clientICE == "failed" || clientICE == "disconnected" || clientICE == "closed" || clientConn == "failed" || clientConn == "closed" {
			return voicev1.VoiceRTCConnectivityIssueUDPUnreachable,
				voicev1.VoiceRTCConnectivitySideNetwork,
				"client could not establish ICE with server UDP candidates " + formatUDPServerCandidates(server)
		}
		if clientICE == "connected" || clientICE == "completed" || clientConn == "connected" {
			return voicev1.VoiceRTCConnectivityIssueDataChannelNotOpen,
				voicev1.VoiceRTCConnectivitySideClient,
				"client ICE connected, but the voice data channel did not open"
		}
		return voicev1.VoiceRTCConnectivityIssueUDPUnreachable,
			voicev1.VoiceRTCConnectivitySideNetwork,
			"server is waiting for a WebRTC data channel on UDP candidates " + formatUDPServerCandidates(server)
	}
	if !server.BackendConnected {
		return voicev1.VoiceRTCConnectivityIssueVoiceBackendConnecting,
			voicev1.VoiceRTCConnectivitySideServer,
			"WebRTC connected, but the server voice backend has not connected yet"
	}
	if !server.SessionReadySent {
		return voicev1.VoiceRTCConnectivityIssueVoiceBackendConnecting,
			voicev1.VoiceRTCConnectivitySideServer,
			"server voice backend connected but has not reported session readiness"
	}
	return voicev1.VoiceRTCConnectivityIssueSessionReadyNotDelivered,
		voicev1.VoiceRTCConnectivitySideClient,
		"server sent session.ready; the client did not receive or process it before timeout"
}

func assistantPCMDuration(bytes int) time.Duration {
	if bytes <= 0 {
		return 0
	}
	return time.Duration(bytes/2) * time.Second / time.Duration(backendOutputSampleRate)
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
