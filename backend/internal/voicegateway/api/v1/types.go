// Exported request and response types for the voice gateway API.

package v1

// StatusResp is a common response for mutation endpoints.
type StatusResp struct {
	Status string `json:"status"`
}

// VoiceTokenResp is the response for GET /api/v1/voice/token.
type VoiceTokenResp struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
	Ephemeral bool   `json:"ephemeral"`
}

// VoiceRTCOfferReq is the request body for POST /api/v1/voice/rtc/offer.
type VoiceRTCOfferReq struct {
	SDP string `json:"sdp"`
}

// VoiceRTCAnswerResp is the response for POST /api/v1/voice/rtc/offer.
type VoiceRTCAnswerResp struct {
	SDP       string `json:"sdp"`
	SessionID string `json:"sessionID"`
}
