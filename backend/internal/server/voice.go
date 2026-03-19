// WebRTC voice bridge HTTP handlers.

package server

import (
	"context"
	"net/http"

	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

func (s *Server) voiceRTCOffer(ctx context.Context, req *v1.VoiceRTCOfferReq) (*v1.VoiceRTCAnswerResp, error) {
	if s.voiceBridge == nil {
		return nil, dto.BadRequest("WebRTC is not enabled (set CAIC_WEBRTC_PORT)")
	}
	sdpAnswer, sessionID, err := s.voiceBridge.HandleOffer(ctx, req.SDP)
	if err != nil {
		return nil, dto.InternalError("WebRTC offer failed: " + err.Error()).Wrap(err)
	}
	return &v1.VoiceRTCAnswerResp{
		SDP:       sdpAnswer,
		SessionID: sessionID,
	}, nil
}

func (s *Server) handleVoiceRTCClose(w http.ResponseWriter, r *http.Request) {
	if s.voiceBridge == nil {
		writeError(w, dto.BadRequest("WebRTC is not enabled"))
		return
	}
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeError(w, dto.BadRequest("sessionID is required"))
		return
	}
	s.voiceBridge.Close(sessionID)
	writeJSONResponse[v1.StatusResp](w, &v1.StatusResp{Status: "closed"}, nil)
}
