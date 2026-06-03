// WebRTC voice bridge HTTP handlers.

package server

import (
	"context"
	"net/http"

	api "github.com/caic-xyz/caic/backend/internal/api"
	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
)

func (s *Server) voiceRTCOffer(ctx context.Context, req *v1.VoiceRTCOfferReq) (*v1.VoiceRTCAnswerResp, error) {
	if s.voiceBridge == nil {
		return nil, api.BadRequest("WebRTC is not enabled (set voice-gateway.config.server.webrtc_udp_port and GEMINI_API_KEY)")
	}
	sdpAnswer, sessionID, err := s.voiceBridge.HandleOffer(ctx, req.SDP)
	if err != nil {
		return nil, api.InternalError("WebRTC offer failed: " + err.Error()).Wrap(err)
	}
	return &v1.VoiceRTCAnswerResp{
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
	writeJSONResponse[v1.StatusResp](w, &v1.StatusResp{Status: "closed"}, nil)
}
