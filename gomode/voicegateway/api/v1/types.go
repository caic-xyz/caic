// Exported request and response types for the voice gateway API.

package v1

// StatusResp is a common response for mutation endpoints.
type StatusResp struct {
	Status string `json:"status"`
}

// VoiceRTCOfferReq is the request body for POST /api/voicegateway/v1/voice/rtc/offer.
type VoiceRTCOfferReq struct {
	SDP string `json:"sdp"`
}

// VoiceRTCAnswerResp is the response for POST /api/voicegateway/v1/voice/rtc/offer.
type VoiceRTCAnswerResp struct {
	SDP       string `json:"sdp"`
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
	ICEConnectionState VoiceRTCICEConnectionState `json:"iceConnectionState,omitempty"`
	ICEGatheringState  VoiceRTCICEGatheringState  `json:"iceGatheringState,omitempty"`
	ConnectionState    VoiceRTCConnectionState    `json:"connectionState,omitempty"`
	SignalingState     VoiceRTCSignalingState     `json:"signalingState,omitempty"`
	DataChannelState   VoiceRTCDataChannelState   `json:"dataChannelState,omitempty"`
}

// VoiceRTCServerDiagnostics reports server-observed WebRTC state for diagnosis.
type VoiceRTCServerDiagnostics struct {
	SessionFound       bool                       `json:"sessionFound"`
	UDPHost            string                     `json:"udpHost,omitempty"`
	UDPPort            int                        `json:"udpPort,omitempty"`
	ICEConnectionState VoiceRTCICEConnectionState `json:"iceConnectionState,omitempty"`
	ICEGatheringState  VoiceRTCICEGatheringState  `json:"iceGatheringState,omitempty"`
	ConnectionState    VoiceRTCConnectionState    `json:"connectionState,omitempty"`
	SignalingState     VoiceRTCSignalingState     `json:"signalingState,omitempty"`
	DataChannelState   VoiceRTCDataChannelState   `json:"dataChannelState,omitempty"`
	DataChannelOpened  bool                       `json:"dataChannelOpened,omitempty"`
	AudioTrackReceived bool                       `json:"audioTrackReceived,omitempty"`
	BackendConnected   bool                       `json:"backendConnected,omitempty"`
	SessionReadySent   bool                       `json:"sessionReadySent,omitempty"`
	LastError          string                     `json:"lastError,omitempty"`
}

// VoiceRTCDiagnosticsReq is the request body for POST /api/voicegateway/v1/voice/rtc/{sessionID}/diagnostics.
type VoiceRTCDiagnosticsReq struct {
	Client VoiceRTCClientDiagnostics `json:"client,omitzero"`
}

// VoiceRTCDiagnosticsResp reports structured WebRTC connectivity diagnostics.
type VoiceRTCDiagnosticsResp struct {
	SessionID string                    `json:"sessionID"`
	Issue     VoiceRTCConnectivityIssue `json:"issue"`
	Side      VoiceRTCConnectivitySide  `json:"side"`
	Message   string                    `json:"message"`
	Server    VoiceRTCServerDiagnostics `json:"server"`
	Client    VoiceRTCClientDiagnostics `json:"client,omitzero"`
}
