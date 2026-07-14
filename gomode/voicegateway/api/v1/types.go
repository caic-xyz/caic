// Exported request and response types for the voice gateway API.

package v1

import "github.com/caic-xyz/caic/gomode/voicegateway/api"

// StatusResp is a common response for mutation endpoints.
type StatusResp struct {
	// Status is a short human-readable result such as "closed".
	Status string `json:"status"`
}

// VoiceRTCOfferReq is the request body for POST /api/voicegateway/v1/voice/rtc/offer.
type VoiceRTCOfferReq struct {
	// SDP is the browser/client WebRTC offer session description from RTCSessionDescription.sdp after createOffer and setLocalDescription.
	SDP string `json:"sdp"`
}

// Validate checks that the SDP offer is provided.
func (r *VoiceRTCOfferReq) Validate() error {
	if r.SDP == "" {
		return api.BadRequest("sdp is required")
	}
	return nil
}

// VoiceRTCAnswerResp is the response for POST /api/voicegateway/v1/voice/rtc/offer.
type VoiceRTCAnswerResp struct {
	// SDP is the gateway WebRTC answer session description to pass to setRemoteDescription with type "answer".
	SDP string `json:"sdp"`
	// SessionID identifies the voice RTC session for diagnostics and close calls.
	SessionID string `json:"sessionID"`
}

// VoiceRTCConnectivitySide identifies where a voice RTC failure appears to be.
type VoiceRTCConnectivitySide string

// Voice RTC connectivity sides.
const (
	VoiceRTCConnectivitySideNone    VoiceRTCConnectivitySide = "none"
	VoiceRTCConnectivitySideServer  VoiceRTCConnectivitySide = "server"
	VoiceRTCConnectivitySideClient  VoiceRTCConnectivitySide = "client"
	VoiceRTCConnectivitySideNetwork VoiceRTCConnectivitySide = "network"
	VoiceRTCConnectivitySideUnknown VoiceRTCConnectivitySide = "unknown"
)

// VoiceRTCConnectivityIssue identifies a structured voice RTC connectivity issue.
type VoiceRTCConnectivityIssue string

// Voice RTC connectivity issues.
const (
	VoiceRTCConnectivityIssueNone                     VoiceRTCConnectivityIssue = "none"
	VoiceRTCConnectivityIssueVoiceBridgeUnavailable   VoiceRTCConnectivityIssue = "voice_bridge_unavailable"
	VoiceRTCConnectivityIssueServerSessionMissing     VoiceRTCConnectivityIssue = "server_session_missing"
	VoiceRTCConnectivityIssueServerICEFailed          VoiceRTCConnectivityIssue = "server_ice_failed"
	VoiceRTCConnectivityIssueUDPUnreachable           VoiceRTCConnectivityIssue = "udp_unreachable"
	VoiceRTCConnectivityIssueDataChannelNotOpen       VoiceRTCConnectivityIssue = "data_channel_not_open"
	VoiceRTCConnectivityIssueVoiceBackendConnecting   VoiceRTCConnectivityIssue = "voice_backend_connecting"
	VoiceRTCConnectivityIssueSessionReadyNotDelivered VoiceRTCConnectivityIssue = "session_ready_not_delivered"
	VoiceRTCConnectivityIssueUnknownTimeout           VoiceRTCConnectivityIssue = "unknown_timeout"
)

// VoiceRTCICEConnectionState is a WebRTC ICE transport connection state.
type VoiceRTCICEConnectionState string

// Voice RTC ICE connection states.
const (
	VoiceRTCICEConnectionStateNew          VoiceRTCICEConnectionState = "new"
	VoiceRTCICEConnectionStateChecking     VoiceRTCICEConnectionState = "checking"
	VoiceRTCICEConnectionStateConnected    VoiceRTCICEConnectionState = "connected"
	VoiceRTCICEConnectionStateCompleted    VoiceRTCICEConnectionState = "completed"
	VoiceRTCICEConnectionStateDisconnected VoiceRTCICEConnectionState = "disconnected"
	VoiceRTCICEConnectionStateFailed       VoiceRTCICEConnectionState = "failed"
	VoiceRTCICEConnectionStateClosed       VoiceRTCICEConnectionState = "closed"
)

// VoiceRTCICEGatheringState is a WebRTC ICE candidate gathering state.
type VoiceRTCICEGatheringState string

// Voice RTC ICE gathering states.
const (
	VoiceRTCICEGatheringStateNew       VoiceRTCICEGatheringState = "new"
	VoiceRTCICEGatheringStateGathering VoiceRTCICEGatheringState = "gathering"
	VoiceRTCICEGatheringStateComplete  VoiceRTCICEGatheringState = "complete"
)

// VoiceRTCConnectionState is a WebRTC peer connection state.
type VoiceRTCConnectionState string

// Voice RTC connection states.
const (
	VoiceRTCConnectionStateNew          VoiceRTCConnectionState = "new"
	VoiceRTCConnectionStateConnecting   VoiceRTCConnectionState = "connecting"
	VoiceRTCConnectionStateConnected    VoiceRTCConnectionState = "connected"
	VoiceRTCConnectionStateDisconnected VoiceRTCConnectionState = "disconnected"
	VoiceRTCConnectionStateFailed       VoiceRTCConnectionState = "failed"
	VoiceRTCConnectionStateClosed       VoiceRTCConnectionState = "closed"
)

// VoiceRTCSignalingState is a WebRTC peer connection signaling state.
type VoiceRTCSignalingState string

// Voice RTC signaling states.
const (
	VoiceRTCSignalingStateStable             VoiceRTCSignalingState = "stable"
	VoiceRTCSignalingStateHaveLocalOffer     VoiceRTCSignalingState = "have-local-offer"
	VoiceRTCSignalingStateHaveRemoteOffer    VoiceRTCSignalingState = "have-remote-offer"
	VoiceRTCSignalingStateHaveLocalPranswer  VoiceRTCSignalingState = "have-local-pranswer"
	VoiceRTCSignalingStateHaveRemotePranswer VoiceRTCSignalingState = "have-remote-pranswer"
	VoiceRTCSignalingStateClosed             VoiceRTCSignalingState = "closed"
)

// VoiceRTCDataChannelState is a WebRTC data channel state.
type VoiceRTCDataChannelState string

// Voice RTC data channel states.
const (
	VoiceRTCDataChannelStateNew        VoiceRTCDataChannelState = "new"
	VoiceRTCDataChannelStateConnecting VoiceRTCDataChannelState = "connecting"
	VoiceRTCDataChannelStateOpen       VoiceRTCDataChannelState = "open"
	VoiceRTCDataChannelStateClosing    VoiceRTCDataChannelState = "closing"
	VoiceRTCDataChannelStateClosed     VoiceRTCDataChannelState = "closed"
)

// VoiceRTCClientDiagnostics reports client-observed WebRTC state for diagnosis.
type VoiceRTCClientDiagnostics struct {
	// ICEConnectionState is the client's RTCPeerConnection.iceConnectionState.
	ICEConnectionState VoiceRTCICEConnectionState `json:"iceConnectionState,omitempty"`
	// ICEGatheringState is the client's RTCPeerConnection.iceGatheringState.
	ICEGatheringState VoiceRTCICEGatheringState `json:"iceGatheringState,omitempty"`
	// ConnectionState is the client's RTCPeerConnection.connectionState.
	ConnectionState VoiceRTCConnectionState `json:"connectionState,omitempty"`
	// SignalingState is the client's RTCPeerConnection.signalingState.
	SignalingState VoiceRTCSignalingState `json:"signalingState,omitempty"`
	// DataChannelState is the client's voice-gateway RTCDataChannel.readyState.
	DataChannelState VoiceRTCDataChannelState `json:"dataChannelState,omitempty"`
}

// VoiceRTCUDPEndpoint reports one server UDP candidate for diagnosis.
type VoiceRTCUDPEndpoint struct {
	// Host is an IP address advertised in the gateway SDP answer.
	Host string `json:"host"`
	// Port is the UDP port paired with host in the gateway SDP answer.
	Port int `json:"port"`
}

// VoiceRTCServerDiagnostics reports server-observed WebRTC state for diagnosis.
type VoiceRTCServerDiagnostics struct {
	// SessionFound reports whether the gateway still has the requested session.
	SessionFound bool `json:"sessionFound"`
	// UDPEndpoints are the LAN/Tailscale and optional UPnP public UDP candidates advertised to the client.
	UDPEndpoints []VoiceRTCUDPEndpoint `json:"udpEndpoints,omitempty"`
	// UDPMappingError is the last UPnP mapping or refresh error, if any.
	UDPMappingError string `json:"udpMappingError,omitempty"`
	// ICEConnectionState is the gateway PeerConnection ICE connection state.
	ICEConnectionState VoiceRTCICEConnectionState `json:"iceConnectionState,omitempty"`
	// ICEGatheringState is the gateway PeerConnection ICE gathering state.
	ICEGatheringState VoiceRTCICEGatheringState `json:"iceGatheringState,omitempty"`
	// ConnectionState is the gateway PeerConnection aggregate connection state.
	ConnectionState VoiceRTCConnectionState `json:"connectionState,omitempty"`
	// SignalingState is the gateway PeerConnection signaling state.
	SignalingState VoiceRTCSignalingState `json:"signalingState,omitempty"`
	// DataChannelState is the gateway-observed voice data channel state.
	DataChannelState VoiceRTCDataChannelState `json:"dataChannelState,omitempty"`
	// DataChannelOpened reports whether the voice data channel reached open.
	DataChannelOpened bool `json:"dataChannelOpened,omitempty"`
	// AudioTrackReceived reports whether the gateway received the client's microphone track.
	AudioTrackReceived bool `json:"audioTrackReceived,omitempty"`
	// BackendConnected reports whether the gateway connected to the configured voice backend.
	BackendConnected bool `json:"backendConnected,omitempty"`
	// SessionReadySent reports whether the gateway sent session.ready to the client.
	SessionReadySent bool `json:"sessionReadySent,omitempty"`
	// LastError is the last gateway error sent to the client, if any.
	LastError string `json:"lastError,omitempty"`
}

// VoiceRTCDiagnosticsReq is the request body for POST /api/voicegateway/v1/voice/rtc/{sessionID}/diagnostics.
type VoiceRTCDiagnosticsReq struct {
	// Client contains the browser/client WebRTC state observed by the caller.
	Client VoiceRTCClientDiagnostics `json:"client,omitzero"`
}

// VoiceRTCDiagnosticsResp reports structured WebRTC connectivity diagnostics.
type VoiceRTCDiagnosticsResp struct {
	// SessionID is the diagnosed voice RTC session ID.
	SessionID string `json:"sessionID"`
	// Issue is the machine-readable connectivity diagnosis.
	Issue VoiceRTCConnectivityIssue `json:"issue"`
	// Side identifies where the issue appears to be.
	Side VoiceRTCConnectivitySide `json:"side"`
	// Message is a human-readable explanation of the diagnosis.
	Message string `json:"message"`
	// Server contains gateway-observed WebRTC state.
	Server VoiceRTCServerDiagnostics `json:"server"`
	// Client echoes the client diagnostics included in the request.
	Client VoiceRTCClientDiagnostics `json:"client,omitzero"`
}
