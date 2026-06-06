// WebRTC voice bridge HTTP handlers.

package server

import (
	"context"
	"net/http"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func (s *Server) voiceRTCOffer(ctx context.Context, req *voicev1.VoiceRTCOfferReq) (*voicev1.VoiceRTCAnswerResp, error) {
	if s.voiceBridge == nil {
		return nil, api.BadRequest("WebRTC is not enabled (set voice-gateway.config.server.webrtc_udp_port and GEMINI_API_KEY)")
	}
	sdpAnswer, sessionID, err := s.voiceBridge.HandleOffer(ctx, req.SDP)
	if err != nil {
		return nil, api.InternalError("WebRTC offer failed: " + err.Error()).Wrap(err)
	}
	return &voicev1.VoiceRTCAnswerResp{
		SDP:       sdpAnswer,
		SessionID: sessionID,
	}, nil
}

func (s *Server) handleVoiceRTCClose(w http.ResponseWriter, r *http.Request) {
	if s.voiceBridge == nil {
		writeError(w, api.BadRequest("WebRTC is not enabled"))
		return
	}
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeError(w, api.BadRequest("sessionID is required"))
		return
	}
	s.voiceBridge.Close(sessionID)
	writeJSONResponse[voicev1.StatusResp](w, &voicev1.StatusResp{Status: "closed"}, nil)
}
