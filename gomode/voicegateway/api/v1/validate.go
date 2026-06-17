// Request validation methods for the voice gateway API.

package v1

import (
	"github.com/caic-xyz/caic/gomode/voicegateway/api"
)

// Validate checks that the SDP offer is provided.
func (r *VoiceRTCOfferReq) Validate() error {
	if r.SDP == "" {
		return api.BadRequest("sdp is required")
	}
	return nil
}
